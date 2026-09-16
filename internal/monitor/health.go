package monitor

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/abhijitkrm/cometduty/internal/alert"
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

	anyUp := false
	for _, n := range nodes {
		if !n.down {
			anyUp = true
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
	}
	c.noNodes = !anyUp
	if !anyUp {
		c.client = nil
	}
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

	if wasDown {
		c.log.Info("node recovered", "endpoint", nodeLabel(n))
	}
	return n
}

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
