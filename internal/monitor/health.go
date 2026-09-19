package monitor

import (
	"context"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/abhijitkrm/cometduty/internal/alert"
	"github.com/abhijitkrm/cometduty/internal/evm"
	"github.com/abhijitkrm/cometduty/internal/rpc"
)

// healthLoop periodically probes every configured node's /status: liveness,
// chain-id match, catching-up, block lag, and peer count. It also refreshes
// validator info each cycle.
func (c *Chain) healthLoop(ctx context.Context) {
	t := time.NewTicker(45 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.probeAllNodes(ctx)
			c.refreshValInfo(ctx, false)
		}
	}
}

func (c *Chain) probeAllNodes(ctx context.Context) {
	c.mu.Lock()
	nodes := c.nodes
	chainID := c.cfg.ChainID
	lagBlocks := int64(c.cfg.Alerts.LagBlocks)
	lagEnabled := c.cfg.Alerts.LagEnabled
	mempoolAlert := int64(c.cfg.Alerts.MempoolTxsAlert)
	roundAlert := int64(c.cfg.Alerts.ConsensusRoundAlert)
	evmURL := c.cfg.EvmRPC
	c.mu.Unlock()

	// best known height = max observed across nodes (for lag detection)
	var bestHeight int64
	results := make([]*nodeState, len(nodes))
	var wg sync.WaitGroup
	for i, n := range nodes {
		wg.Add(1)
		go func(i int, n *nodeState) {
			defer wg.Done()
			results[i] = c.probeNode(ctx, n, chainID)
		}(i, n)
	}
	wg.Wait()

	if evmURL != "" {
		c.probeEVM(ctx, evmURL)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	for _, n := range nodes {
		if !n.down && n.height > bestHeight {
			bestHeight = n.height
		}
	}
	// also compare against what the websocket has seen
	if c.lastHeight > bestHeight {
		bestHeight = c.lastHeight
	}

	anyUp, unhealthy := false, 0
	for _, n := range nodes {
		if !n.down {
			anyUp = true
		} else {
			unhealthy++
		}
		lag := int64(0)
		if !n.down && bestHeight > 0 {
			lag = bestHeight - n.height
		}
		if c.met != nil {
			var downSec float64
			if n.down && !n.downSince.IsZero() {
				downSec = time.Since(n.downSince).Seconds()
			}
			c.met.NodeHealth(c.name, chainID, nodeLabel(n), n.cfg.URL, downSec, float64(lag), float64(n.peers))
			if n.version != "" {
				c.met.NodeInfo(c.name, chainID, n.cfg.URL, n.moniker, n.version, n.network)
			}
		}

		// lag alerting
		if lagEnabled && lagBlocks > 0 && !n.down && lag > lagBlocks && !n.lagged {
			n.lagged = true
			c.alert(chainID, nil, "node-lag:"+n.cfg.URL, false, "warning",
				fmt.Sprintf("RPC node %s is %d blocks behind head (%d) on %s", nodeLabel(n), lag, bestHeight, chainID))
		} else if lagEnabled && n.lagged && lag <= lagBlocks {
			n.lagged = false
			c.alert(chainID, nil, "node-lag:"+n.cfg.URL, true, "info",
				fmt.Sprintf("RPC node %s is %d blocks behind head on %s", nodeLabel(n), lag, chainID))
		}

		if c.met != nil {
			var mempoolTxs, mempoolBytes, round float64
			if !n.down {
				mempoolTxs, mempoolBytes, round = float64(n.mempoolTxs), float64(n.mempoolBytes), float64(n.round)
			}
			c.met.NodeInternals(c.name, chainID, n.cfg.URL, nodeLabel(n), mempoolTxs, mempoolBytes, round)
		}

		// mempool backlog alerting (0 = metric only)
		if mempoolAlert > 0 && !n.down && n.mempoolTxs > int64(mempoolAlert) && !n.mempoolAlerted {
			n.mempoolAlerted = true
			c.alert(chainID, nil, "mempool-txs:"+n.cfg.URL, false, "warning",
				fmt.Sprintf("RPC node %s has %d unconfirmed txs (%d bytes) on %s — CheckTx/execution may be wedged", nodeLabel(n), n.mempoolTxs, n.mempoolBytes, chainID))
		} else if n.mempoolAlerted && (n.down || n.mempoolTxs <= int64(mempoolAlert)) {
			n.mempoolAlerted = false
			c.alert(chainID, nil, "mempool-txs:"+n.cfg.URL, true, "info",
				fmt.Sprintf("RPC node %s mempool backlog cleared on %s (%d txs)", nodeLabel(n), chainID, n.mempoolTxs))
		}

		// consensus round alerting (0 = metric only) — sustained elevation is
		// leader churn even while the node answers RPC
		if roundAlert > 0 && !n.down && n.round > int64(roundAlert) && !n.roundAlerted {
			n.roundAlerted = true
			c.alert(chainID, nil, "consensus-round:"+n.cfg.URL, false, "warning",
				fmt.Sprintf("RPC node %s is at consensus round %d (> %d) on %s — proposals timing out", nodeLabel(n), n.round, roundAlert, chainID))
		} else if n.roundAlerted && (n.down || n.round <= int64(roundAlert)) {
			n.roundAlerted = false
			c.alert(chainID, nil, "consensus-round:"+n.cfg.URL, true, "info",
				fmt.Sprintf("RPC node %s consensus recovered on %s (round %d)", nodeLabel(n), chainID, n.round))
		}
	}
	c.noNodes = !anyUp
	if !anyUp {
		c.client = nil
	}
	if c.met != nil {
		c.met.NodeCount(c.name, chainID, len(nodes), unhealthy)
	}
}

// probeEVM checks the configured EVM JSON-RPC endpoint: latest executed height
// and sync state. The gap between consensus height and EVM height is the
// execution-lag signal — consensus producing blocks the EVM never runs is a
// distinct outage (chain looks alive, transactions don't execute).
func (c *Chain) probeEVM(ctx context.Context, url string) {
	cl := evm.New(url, 8*time.Second)
	ectx, cancel := context.WithTimeout(ctx, 8*time.Second)
	height, err := cl.BlockNumber(ectx)
	cancel()
	c.mu.Lock()
	if err != nil {
		if !c.evmDown {
			c.evmDown, c.evmDownSince = true, time.Now()
		}
		c.evmLastMsg = "down: " + err.Error()
		c.mu.Unlock()
		return
	}
	c.evmDown, c.evmSyncing, c.evmLastMsg = false, false, ""
	c.evmHeight = height
	c.evmDownSince = time.Time{}
	c.mu.Unlock()

	sctx, scancel := context.WithTimeout(ctx, 5*time.Second)
	syncing, _, serr := cl.Syncing(sctx)
	scancel()
	c.mu.Lock()
	if serr == nil {
		c.evmSyncing = syncing
	}
	lag := c.lastHeight - c.evmHeight
	if lag < 0 {
		lag = 0 // evm can briefly lead while the next consensus block finalizes
	}
	var downSec float64
	if c.evmDown {
		downSec = time.Since(c.evmDownSince).Seconds()
	}
	if c.met != nil {
		c.met.EvmHealth(c.name, c.cfg.ChainID, url, c.evmHeight, lag, downSec, c.evmSyncing)
	}
	txpoolAlert := int64(c.cfg.Alerts.EvmTxpoolQueuedAlert)
	c.mu.Unlock()

	// execution internals — txpool depth and block fullness, both non-fatal
	tctx, tcancel := context.WithTimeout(ctx, 5*time.Second)
	pending, queued, terr := cl.TxpoolStatus(tctx)
	tcancel()
	gctx, gcancel := context.WithTimeout(ctx, 5*time.Second)
	ratio, gerr := cl.GasUsedRatio(gctx)
	gcancel()
	pctx, pcancel := context.WithTimeout(ctx, 5*time.Second)
	gp, gperr := cl.GasPrice(pctx)
	pcancel()

	c.mu.Lock()
	if terr == nil {
		c.evmPending, c.evmQueued = pending, queued
	}
	if gerr == nil {
		c.evmGasRatio = ratio
	}
	if c.met != nil {
		c.met.EvmInternals(c.name, c.cfg.ChainID, url, c.evmPending, c.evmQueued, c.evmGasRatio)
		if gperr == nil {
			gf, _ := new(big.Float).SetInt(gp).Float64()
			c.met.EvmGasPrice(c.name, c.cfg.ChainID, url, gf)
		}
	}
	if txpoolAlert > 0 && c.evmQueued > txpoolAlert && !c.evmTxpoolAlarm {
		c.evmTxpoolAlarm = true
		c.alert(c.cfg.ChainID, nil, "evm-txpool:"+url, false, "warning",
			fmt.Sprintf("EVM txpool on %s has %d queued txs (> %d) — execution wedge or nonce gap", url, c.evmQueued, txpoolAlert))
	} else if c.evmTxpoolAlarm && c.evmQueued <= txpoolAlert {
		c.evmTxpoolAlarm = false
		c.alert(c.cfg.ChainID, nil, "evm-txpool:"+url, true, "info",
			fmt.Sprintf("EVM txpool on %s drained (queued %d)", url, c.evmQueued))
	}
	c.mu.Unlock()
}

// probeNode queries one endpoint and updates its state. Returns n for convenience.
func (c *Chain) probeNode(ctx context.Context, n *nodeState, chainID string) *nodeState {
	opts := []rpc.Option{rpc.WithTimeout(8 * time.Second)}
	for k, v := range n.cfg.Headers {
		opts = append(opts, rpc.WithHeader(k, v))
	}
	cl, err := rpc.New(n.cfg.URL, opts...)
	if err != nil {
		c.mu.Lock()
		n.down, n.lastMsg = true, "bad url: "+err.Error()
		c.mu.Unlock()
		return n
	}
	sctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	st, err := cl.Status(sctx)
	cancel()
	if err != nil {
		c.markDown(n, "down: "+err.Error())
		return n
	}
	if st.NodeInfo.Network != chainID {
		c.markDown(n, fmt.Sprintf("wrong network %s (want %s)", st.NodeInfo.Network, chainID))
		return n
	}
	c.mu.Lock()
	n.height = int64(st.SyncInfo.LatestBlockHeight)
	n.moniker, n.version, n.network = st.NodeInfo.Moniker, st.NodeInfo.Version, st.NodeInfo.Network
	if st.SyncInfo.CatchingUp {
		n.down, n.syncing, n.lastMsg = true, true, "catching up"
		c.mu.Unlock()
		return n
	}
	wasDown := n.down
	n.down, n.syncing = false, false
	n.lastMsg = ""
	n.downSince = time.Time{}
	c.mu.Unlock()

	// peer count — non-fatal
	pctx, pcancel := context.WithTimeout(ctx, 5*time.Second)
	if ni, err := cl.NetInfo(pctx); err == nil {
		c.mu.Lock()
		n.peers = int64(ni.NPeers)
		c.mu.Unlock()
	}
	pcancel()

	// mempool backlog — non-fatal
	mctx, mcancel := context.WithTimeout(ctx, 5*time.Second)
	if txs, bytes, err := cl.NumUnconfirmedTxs(mctx); err == nil {
		c.mu.Lock()
		n.mempoolTxs, n.mempoolBytes = txs, bytes
		c.mu.Unlock()
	}
	mcancel()

	// consensus round — non-fatal; sustained elevation = leader churn
	rctx, rcancel := context.WithTimeout(ctx, 5*time.Second)
	if round, err := cl.ConsensusRound(rctx); err == nil {
		c.mu.Lock()
		n.round = round
		c.mu.Unlock()
	}
	rcancel()

	// host stats via the node's own prometheus endpoint — non-fatal
	if n.cfg.MetricsURL != "" {
		c.probeSysstats(ctx, n)
	}

	if wasDown {
		c.log.Info("node recovered", "endpoint", nodeLabel(n))
	}
	return n
}

// probeSysstats scrapes process_cpu_seconds_total and
// process_resident_memory_bytes off the node's own prometheus endpoint.
// CPU% is derived from the counter delta between probes — it can exceed 100
// on multi-core nodes.
func (c *Chain) probeSysstats(ctx context.Context, n *nodeState) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(n.cfg.MetricsURL, "/")+"/metrics", nil)
	if err != nil {
		return
	}
	hctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	resp, err := sysClient.Do(req.WithContext(hctx))
	if err != nil {
		return
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return
	}
	var cpuSec, memBytes float64
	for _, line := range strings.Split(string(body), "\n") {
		if v, ok := strings.CutPrefix(line, "process_cpu_seconds_total "); ok {
			cpuSec, _ = strconv.ParseFloat(strings.Fields(v)[0], 64)
		} else if v, ok := strings.CutPrefix(line, "process_resident_memory_bytes "); ok {
			memBytes, _ = strconv.ParseFloat(strings.Fields(v)[0], 64)
		}
	}

	c.mu.Lock()
	now := time.Now()
	if !n.sysPrevAt.IsZero() && cpuSec > 0 {
		dt := now.Sub(n.sysPrevAt).Seconds()
		if dt > 0 {
			n.cpuPct = (cpuSec - n.sysPrevCPU) / dt * 100
			if n.cpuPct < 0 {
				n.cpuPct = 0
			}
		}
	}
	n.sysPrevCPU, n.sysPrevAt, n.memBytes = cpuSec, now, memBytes
	cpuPct, mem := n.cpuPct, n.memBytes
	chainID, name := c.cfg.ChainID, c.name
	c.mu.Unlock()

	if c.met != nil {
		c.met.NodeSysstats(name, chainID, n.cfg.URL, cpuPct, mem)
	}

	a := c.cfg.Alerts
	if a.CpuPctAlert > 0 {
		key := "cpu-high:" + n.cfg.URL
		if cpuPct > float64(a.CpuPctAlert) && !n.cpuAlerted {
			n.cpuAlerted = true
			c.alert(chainID, nil, key, false, "warning",
				fmt.Sprintf("node %s CPU at %.0f%% (> %d%%) on %s", nodeLabel(n), cpuPct, a.CpuPctAlert, chainID))
		} else if n.cpuAlerted && cpuPct <= float64(a.CpuPctAlert) {
			n.cpuAlerted = false
			c.alert(chainID, nil, key, true, "info",
				fmt.Sprintf("node %s CPU back to %.0f%% on %s", nodeLabel(n), cpuPct, chainID))
		}
	}
	if a.MemBytesAlert > 0 {
		key := "mem-high:" + n.cfg.URL
		if mem > float64(a.MemBytesAlert) && !n.memAlerted {
			n.memAlerted = true
			c.alert(chainID, nil, key, false, "warning",
				fmt.Sprintf("node %s resident memory %.1f GiB (> %d bytes) on %s", nodeLabel(n), mem/1073741824, a.MemBytesAlert, chainID))
		} else if n.memAlerted && mem <= float64(a.MemBytesAlert) {
			n.memAlerted = false
			c.alert(chainID, nil, key, true, "info",
				fmt.Sprintf("node %s memory back to %.1f GiB on %s", nodeLabel(n), mem/1073741824, chainID))
		}
	}
}

var sysClient = &http.Client{Timeout: 6 * time.Second}

func (c *Chain) markDown(n *nodeState, msg string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !n.down {
		n.down = true
		n.downSince = time.Now()
	}
	n.lastMsg = msg
}

func nodeLabel(n *nodeState) string {
	if n.cfg.Name != "" {
		return n.cfg.Name
	}
	return n.cfg.URL
}

// watchLoop evaluates time-based alarms every 2 seconds: stalled chain, no
// servers, inactive/jailed transitions, consecutive & window-percentage misses,
// and node-down.
func (c *Chain) watchLoop(ctx context.Context) {
	nodeAlerted := map[string]bool{}
	syncAlerted := make(map[string]bool)
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		// NB: rootFn() acquires the supervisor lock — call it BEFORE c.mu to
		// keep lock ordering consistent with Reload (s.mu → chain.mu).
		root := c.rootFn()
		c.mu.Lock()
		cfg := c.cfg
		a := cfg.Alerts

		// --- stalled chain detection (with correct resolve) ---
		if a.StalledEnabled {
			stallFor := time.Since(c.lastBlockTime)
			stalled := !c.lastBlockTime.IsZero() && stallFor > time.Duration(a.StalledMinutes)*time.Minute
			switch {
			case stalled && !c.stallAlarm:
				c.stallAlarm = true
				c.alert(cfg.ChainID, nil, "stalled:"+cfg.ChainID, false, "critical",
					fmt.Sprintf("stalled: no new block on %s for %d minutes", cfg.ChainID, a.StalledMinutes))
			case !stalled && (c.stallAlarm || c.eng.HasOpen("stalled:"+cfg.ChainID)) && !c.lastBlockTime.IsZero():
				// blocks are flowing again — resolve. (v2 checked IsZero() here,
				// which can never be true once monitoring starts.)
				c.stallAlarm = false
				c.alert(cfg.ChainID, nil, "stalled:"+cfg.ChainID, true, "info",
					fmt.Sprintf("stalled: no new block on %s for %d minutes", cfg.ChainID, a.StalledMinutes))
			}
		}

		// --- no usable RPC endpoints ---
		if a.AlertIfNoServers {
			switch {
			case c.noNodes && !c.noNodesAlarm && time.Since(c.noNodesSince) > time.Duration(root.NodeDownMin)*time.Minute:
				c.noNodesAlarm = true
				c.alert(cfg.ChainID, nil, "no-nodes:"+cfg.ChainID, false, "critical",
					fmt.Sprintf("no RPC endpoints are working for %s", cfg.ChainID))
			case !c.noNodes && (c.noNodesAlarm || c.eng.HasOpen("no-nodes:"+cfg.ChainID)):
				c.noNodesAlarm = false
				c.alert(cfg.ChainID, nil, "no-nodes:"+cfg.ChainID, true, "info",
					fmt.Sprintf("no RPC endpoints are working for %s", cfg.ChainID))
			}
		}

		// --- node down alarms ---
		for _, n := range c.nodes {
			// catching-up: node is syncing — informative, distinct from down
			syncKey := "catching-up:" + n.cfg.URL
			if n.syncing && !syncAlerted[syncKey] {
				syncAlerted[syncKey] = true
				c.alert(cfg.ChainID, nil, syncKey, false, "warning",
					fmt.Sprintf("RPC node %s is catching up on %s (height %d)", nodeLabel(n), cfg.ChainID, n.height))
			} else if !n.syncing && (syncAlerted[syncKey] || c.eng.HasOpen(syncKey)) {
				syncAlerted[syncKey] = false
				c.alert(cfg.ChainID, nil, syncKey, true, "info",
					fmt.Sprintf("RPC node %s finished syncing on %s", nodeLabel(n), cfg.ChainID))
			}

			key := "node-down:" + n.cfg.URL
			if n.cfg.AlertIfDown && n.down && !n.downSince.IsZero() &&
				time.Since(n.downSince) > time.Duration(root.NodeDownMin)*time.Minute {
				if !nodeAlerted[key] {
					nodeAlerted[key] = true
					c.alert(cfg.ChainID, nil, key, false, root.NodeDownSeverity,
						fmt.Sprintf("RPC node %s down for > %d minutes on %s: %s", nodeLabel(n), root.NodeDownMin, cfg.ChainID, n.lastMsg))
				}
			} else if !n.down && (nodeAlerted[key] || c.eng.HasOpen(key)) {
				nodeAlerted[key] = false
				c.alert(cfg.ChainID, nil, key, true, "info",
					fmt.Sprintf("RPC node %s recovered on %s", nodeLabel(n), cfg.ChainID))
			}
		}

		// --- EVM execution-layer alarms (only when evm_rpc configured) ---
		if cfg.EvmRPC != "" {
			evmLag := c.lastHeight - c.evmHeight
			if evmLag < 0 {
				evmLag = 0
			}
			// evm endpoint down
			key := "evm-down:" + cfg.EvmRPC
			switch {
			case a.EvmDownEnabled && c.evmDown && !c.evmDownSince.IsZero() &&
				time.Since(c.evmDownSince) > time.Duration(root.NodeDownMin)*time.Minute && !c.evmDownAlarm:
				c.evmDownAlarm = true
				c.alert(cfg.ChainID, nil, key, false, "warning",
					fmt.Sprintf("EVM RPC %s down for > %d minutes on %s: %s", cfg.EvmRPC, root.NodeDownMin, cfg.ChainID, c.evmLastMsg))
			case !c.evmDown && (c.evmDownAlarm || c.eng.HasOpen(key)):
				c.evmDownAlarm = false
				c.alert(cfg.ChainID, nil, key, true, "info",
					fmt.Sprintf("EVM RPC %s recovered on %s", cfg.EvmRPC, cfg.ChainID))
			}
			// execution lag — only meaningful while the endpoint is up
			key = "evm-lag:" + cfg.EvmRPC
			switch {
			case a.EvmLagEnabled && !c.evmDown && !c.evmSyncing && c.evmHeight > 0 &&
				a.EvmLagBlocks > 0 && evmLag > int64(a.EvmLagBlocks) && !c.evmLagAlarm:
				c.evmLagAlarm = true
				c.alert(cfg.ChainID, nil, key, false, "warning",
					fmt.Sprintf("EVM execution is %d blocks behind consensus on %s (evm %d, comet %d)", evmLag, cfg.ChainID, c.evmHeight, c.lastHeight))
			case (!a.EvmLagEnabled || evmLag <= int64(a.EvmLagBlocks)) && (c.evmLagAlarm || c.eng.HasOpen(key)):
				c.evmLagAlarm = false
				c.alert(cfg.ChainID, nil, key, true, "info",
					fmt.Sprintf("EVM execution caught up on %s (lag %d)", cfg.ChainID, evmLag))
			}
		}

		// --- per-validator alarms ---
		for _, tg := range c.targets {
			if tg.info == nil {
				continue
			}
			// per-validator alerts: block overrides the chain policy
			va := a
			if tg.vc.Alerts != nil {
				va = *tg.vc.Alerts
			}
			// inactive transition (jailed / tombstoned / unbonded). info/prev
			// only refresh every 45s, so latch on tg.inactive — raise once when
			// it first shows unbonded, resolve once when it returns.
			if va.AlertIfInactive {
				key := "inactive:" + tg.info.Valcons
				switch {
				case !tg.info.Bonded && tg.inactive == "":
					tg.inactive = "jailed"
					if tg.info.Tombstoned {
						tg.inactive = "tombstoned (permanent)"
					}
					c.alert(cfg.ChainID, tg, key, false, "critical",
						fmt.Sprintf("%s is no longer in the active set on %s: %s", tg.info.Moniker, cfg.ChainID, tg.inactive))
				case tg.info.Bonded && (tg.inactive != "" || c.eng.HasOpen(key)):
					was := tg.inactive
					if was == "" {
						was = "inactive" // restored alert; local latch never set
					}
					tg.inactive = ""
					c.alert(cfg.ChainID, tg, key, true, "info",
						fmt.Sprintf("%s is back in the active set on %s (was %s)", tg.info.Moniker, cfg.ChainID, was))
				}
			}
			// consecutive misses
			if va.ConsecutiveEnabled {
				key := "consecutive:" + tg.info.Valcons
				if !tg.missedAlarm && tg.consec >= int64(va.ConsecutiveMissed) && va.ConsecutiveMissed > 0 {
					tg.missedAlarm = true
					c.alert(cfg.ChainID, tg, key, false, va.ConsecutivePriority,
						fmt.Sprintf("%s has missed %d consecutive blocks on %s", tg.info.Moniker, tg.consec, cfg.ChainID))
				} else if tg.consec < int64(va.ConsecutiveMissed) && (tg.missedAlarm || c.eng.HasOpen(key)) {
					tg.missedAlarm = false
					c.alert(cfg.ChainID, tg, key, true, "info",
						fmt.Sprintf("%s consecutive-miss alarm cleared on %s", tg.info.Moniker, cfg.ChainID))
				}
			}
			// window percentage
			if va.PercentageEnabled && tg.info.Window > 0 {
				key := "window-pct:" + tg.info.Valcons
				pct := 100 * float64(tg.info.Missed) / float64(tg.info.Window)
				if !tg.pctAlarm && pct > float64(va.WindowPct) {
					tg.pctAlarm = true
					c.alert(cfg.ChainID, tg, key, false, va.PercentagePriority,
						fmt.Sprintf("%s missed %.1f%% of the slashing window on %s", tg.info.Moniker, pct, cfg.ChainID))
				} else if pct <= float64(va.WindowPct) && (tg.pctAlarm || c.eng.HasOpen(key)) {
					tg.pctAlarm = false
					// v2 bug fixed: this must send resolved=true
					c.alert(cfg.ChainID, tg, key, true, "info",
						fmt.Sprintf("%s window-miss alarm cleared on %s (%.1f%%)", tg.info.Moniker, cfg.ChainID, pct))
				}
			}
		}
		if c.met != nil {
			c.met.ActiveAlerts(c.name, cfg.ChainID, c.eng.ActiveCount(c.name))
		}
		c.mu.Unlock()
	}
}

// alert sends a notification. It never touches c.mu — callers pass the chainID
// they already hold under lock so this is safe from inside locked sections.
func (c *Chain) alert(chainID string, tg *Target, key string, resolved bool, severity, msg string) {
	if severity == "" {
		severity = "warning"
	}
	a := alert.Alert{
		Chain:    c.name,
		ChainID:  chainID,
		Key:      key,
		Message:  msg,
		Severity: severity,
		Resolved: resolved,
		Time:     time.Now(),
	}
	if tg != nil && tg.info != nil {
		a.Moniker = tg.info.Moniker
		a.Valcons = tg.info.Valcons
		a.Scoped = tg.vc.Alerts
	}
	if resolved {
		if c.eng.HasOpen(key) {
			c.log.Info("resolve", "key", key)
		}
	} else {
		c.log.Warn("alert raised", "key", key, "msg", msg)
	}
	// Always dispatch resolves: the engine clears the active-set and delivers
	// resolve notices only to destinations that actually got the raise.
	go c.eng.Dispatch(context.Background(), a)
}

// ready reports whether the chain is being observed end-to-end: at least one
// healthy RPC endpoint and a block seen recently. Backs /readyz.
func (c *Chain) ready() (bool, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.noNodes {
		return false, "no healthy RPC endpoints"
	}
	if c.lastBlockTime.IsZero() {
		return false, "waiting for first block"
	}
	if age := time.Since(c.lastBlockTime); age > 2*time.Minute {
		return false, fmt.Sprintf("no block for %s", age.Round(time.Second))
	}
	return true, ""
}
