// Package monitor supervises one goroutine tree per configured chain: an RPC
// health checker, a websocket block watcher, and an alarm evaluation loop.
package monitor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/abhijitkrm/cometduty/internal/alert"
	"github.com/abhijitkrm/cometduty/internal/config"
	"github.com/abhijitkrm/cometduty/internal/rpc"
)

// MetricsSink is what the monitor needs from the prometheus exporter.
type MetricsSink interface {
	BlockResult(name, chainID, validator, moniker string, st SignState, consecutive int64)
	LastBlock(name, chainID string, height int64, sincePrev float64)
	Tick(name, chainID string, lastBlockTime time.Time)
	SignatureRatio(name, chainID string, ratio float64)
	NodeHealth(name, chainID, endpoint, label string, downSeconds, lagBlocks float64, peers float64)
	NodeCount(name, chainID string, total, unhealthy int)
	Window(name, chainID, validator, moniker string, missed, window int64)
	ActiveAlerts(name, chainID string, n int)
}

// nodeState tracks a configured endpoint's health.
type nodeState struct {
	cfg       *config.NodeConfig
	down      bool
	syncing   bool
	lastMsg   string
	downSince time.Time
	height    int64
	peers     int64
	alerted   bool // down alert currently open
	lagged    bool // lag alert currently open
}

// Target is one validator being watched on a chain.
type Target struct {
	vc   config.ValidatorConfig
	info *ValInfo
	vt   *voteTracker

	signs, props, miss, pvMiss, pcMiss, consec int64

	missedAlarm bool
	pctAlarm    bool
	inactive    string // remembers "jailed"/"tombstoned" for resolve text
}

// Chain monitors one chain (and its 1..n validators).
type Chain struct {
	name   string
	cfg    *config.ChainConfig   // swapped on reload under mu
	rootFn func() *config.Config // live global config (survives reload)
	eng    *alert.Engine
	met    MetricsSink
	log    *slog.Logger

	mu        sync.Mutex
	nodes     []*nodeState
	targets   []*Target
	byAddr    map[string]*Target // cons hex -> target
	client    *rpc.Client
	clientURL string

	lastBlockTime  time.Time
	lastHeight     int64
	stallAlarm     bool
	noNodes        bool
	noNodesAlarm   bool
	noNodesSince   time.Time
	slashingOK     bool
	lastSlashingAt time.Time
}

// NewChain builds a monitor for one chain. rootFn must return the current
// global config — it is evaluated per-use so hot reloads propagate.
func NewChain(name string, cc *config.ChainConfig, rootFn func() *config.Config, eng *alert.Engine, met MetricsSink) *Chain {
	c := &Chain{
		name:       name,
		cfg:        cc,
		rootFn:     rootFn,
		eng:        eng,
		met:        met,
		log:        slog.With("chain", name, "chain_id", cc.ChainID),
		byAddr:     map[string]*Target{},
		slashingOK: true,
	}
	c.rebuildLocked(cc)
	return c
}

// rebuildLocked (re)creates node and target runtimes from cfg.
func (c *Chain) rebuildLocked(cc *config.ChainConfig) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nodes = make([]*nodeState, 0, len(cc.Nodes))
	for _, n := range cc.Nodes {
		c.nodes = append(c.nodes, &nodeState{cfg: n})
	}
	c.targets = make([]*Target, 0)
	c.byAddr = map[string]*Target{}
	for _, vc := range cc.ValidatorTargets() {
		t := &Target{vc: vc, vt: newVoteTracker()}
		c.targets = append(c.targets, t)
	}
}

// Update swaps the config (hot reload) — nodes/targets rebuilt, stats reset.
func (c *Chain) Update(cc *config.ChainConfig) {
	c.rebuildLocked(cc)
	c.mu.Lock()
	c.cfg = cc
	c.mu.Unlock()
	c.log.Info("chain config reloaded")
}

func (c *Chain) chainCfg() *config.ChainConfig {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cfg
}

// Run is the per-chain supervisor loop: pick a working endpoint, refresh
// validator info, run the websocket watcher; restart everything on failure.
func (c *Chain) Run(ctx context.Context) {
	go c.healthLoop(ctx)
	go c.watchLoop(ctx)

	for {
		if ctx.Err() != nil {
			return
		}
		if err := c.pickClient(ctx); err != nil {
			c.log.Warn("no usable endpoint", "err", err)
			c.setNoNodes(true)
			select {
			case <-ctx.Done():
				return
			case <-time.After(15 * time.Second):
			}
			continue
		}
		c.setNoNodes(false)
		c.refreshValInfo(ctx, true)
		if err := c.wsLoop(ctx); err != nil {
			c.log.Warn("websocket ended, restarting monitor", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

// pickClient selects the first healthy endpoint and validates chain-id + sync.
// Probes run WITHOUT c.mu held (they do network I/O); shared state is only
// touched under the lock at the end of each probe.
func (c *Chain) pickClient(ctx context.Context) error {
	c.mu.Lock()
	nodes := append([]*nodeState{}, c.nodes...)
	cfg := c.cfg
	c.mu.Unlock()

	var firstErr error
	for _, n := range nodes {
		cl, st, err := probeEndpoint(ctx, n.cfg, cfg.ChainID)
		c.mu.Lock()
		if err != nil {
			n.down, n.lastMsg = true, err.Error()
			if st != nil && st.SyncInfo.CatchingUp {
				n.syncing = true
			}
			c.mu.Unlock()
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		n.down, n.syncing = false, false
		n.lastMsg = ""
		n.height = int64(st.SyncInfo.LatestBlockHeight)
		c.client = cl
		c.clientURL = n.cfg.URL
		c.mu.Unlock()
		return nil
	}

	// last resort: public endpoints from the chain registry
	if cfg.PublicFallback {
		if urls, err := publicEndpoints(ctx, cfg.ChainID); err == nil {
			for _, u := range urls {
				cl, st, err := probeRawURL(ctx, u, cfg.ChainID)
				if err != nil {
					continue
				}
				_ = st
				c.mu.Lock()
				c.client = cl
				c.clientURL = u
				c.mu.Unlock()
				c.log.Warn("using public fallback endpoint", "endpoint", u)
				return nil
			}
		} else {
			c.log.Debug("public fallback lookup failed", "err", err)
		}
	}

	c.mu.Lock()
	c.client = nil
	c.mu.Unlock()
	if firstErr == nil {
		firstErr = fmt.Errorf("no endpoints configured")
	}
	return firstErr
}

// probeEndpoint checks one configured node: reachability, chain-id, sync state.
func probeEndpoint(ctx context.Context, nc *config.NodeConfig, wantChainID string) (*rpc.Client, *rpc.Status, error) {
	opts := []rpc.Option{rpc.WithTimeout(8 * time.Second)}
	for k, v := range nc.Headers {
		opts = append(opts, rpc.WithHeader(k, v))
	}
	cl, err := rpc.New(nc.URL, opts...)
	if err != nil {
		return nil, nil, fmt.Errorf("bad url: %w", err)
	}
	sctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	st, err := cl.Status(sctx)
	cancel()
	if err != nil {
		return nil, nil, fmt.Errorf("status: %w", err)
	}
	if st.NodeInfo.Network != wantChainID {
		return nil, st, fmt.Errorf("wrong chain_id %s (want %s)", st.NodeInfo.Network, wantChainID)
	}
	if st.SyncInfo.CatchingUp {
		return nil, st, fmt.Errorf("node is catching up")
	}
	return cl, st, nil
}

// probeRawURL checks an arbitrary URL (used for registry fallback).
func probeRawURL(ctx context.Context, u, wantChainID string) (*rpc.Client, *rpc.Status, error) {
	cl, err := rpc.New(u, rpc.WithTimeout(8*time.Second))
	if err != nil {
		return nil, nil, err
	}
	sctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	st, err := cl.Status(sctx)
	cancel()
	if err != nil {
		return nil, nil, err
	}
	if st.NodeInfo.Network != wantChainID || st.SyncInfo.CatchingUp {
		return nil, st, fmt.Errorf("unsuitable endpoint")
	}
	return cl, st, nil
}

// refreshValInfo updates each target's staking/slashing state.
func (c *Chain) refreshValInfo(ctx context.Context, first bool) {
	c.mu.Lock()
	cl := c.client
	slashingOK := c.slashingOK
	c.mu.Unlock()
	if cl == nil {
		return
	}

	for _, t := range c.targets {
		var consAddr []byte
		var moniker string
		var jailed, bonded bool

		vc := t.vc
		if vc.ValconsOverride != "" || strings.Contains(vc.ValoperAddress, "valcons") {
			// consensus address provided directly — no staking query needed
			addr, vcStr, err := resolveConsAddress(vc.ValoperAddress, vc.ValconsOverride, vc.ConsPrefix, nil)
			if err != nil {
				c.log.Warn("bad valcons", "err", err)
				continue
			}
			consAddr, moniker, bonded = addr, vcStr, true
		} else {
			qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			var err error
			consAddr, moniker, jailed, bonded, err = getValidatorRecord(qctx, cl, vc.ValoperAddress)
			cancel()
			if err != nil {
				c.log.Warn("validator query failed", "valoper", vc.ValoperAddress, "err", err)
				continue
			}
		}

		if vc.Label != "" {
			moniker = vc.Label // operator display name wins over chain moniker
		}
		addr, valcons, err := resolveConsAddress(vc.ValoperAddress, vc.ValconsOverride, vc.ConsPrefix, consAddr)
		if err != nil {
			c.log.Warn("resolving consensus address", "err", err)
			continue
		}
		hexAddr := strings.ToUpper(fmt.Sprintf("%X", addr))

		c.mu.Lock()
		if t.info == nil {
			t.info = &ValInfo{}
		}
		ni := &ValInfo{
			Moniker: moniker, Bonded: bonded, Jailed: jailed,
			ConsAddr: addr, ConsHex: hexAddr, Valcons: valcons,
			Missed: t.info.Missed, Window: t.info.Window, Tombstoned: t.info.Tombstoned,
		}
		t.info = ni
		c.byAddr[hexAddr] = t
		c.mu.Unlock()

		if first {
			c.log.Info("monitoring validator", "valoper", vc.ValoperAddress, "moniker", moniker, "valcons", valcons, "bonded", bonded)
		}

		// slashing info — skip entirely on chains without the module
		if slashingOK {
			sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			tomb, missed, err := getSigningInfo(sctx, cl, valcons)
			cancel()
			switch {
			case errors.Is(err, errNoSlashingModule):
				c.mu.Lock()
				c.slashingOK = false
				c.mu.Unlock()
				c.log.Info("chain has no slashing module — window/tombstone alerts disabled", "chain_id", c.chainCfg().ChainID)
			case err != nil:
				c.log.Debug("signing info query failed", "err", err)
			default:
				c.mu.Lock()
				t.info.Tombstoned, t.info.Missed = tomb, missed
				c.mu.Unlock()
			}
			c.mu.Lock()
			refreshWindow := t.info.Window == 0 || time.Since(c.lastSlashingAt) > 30*time.Minute
			c.mu.Unlock()
			if refreshWindow && c.slashingOK {
				sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
				w, err := getSlashingWindow(sctx, cl)
				cancel()
				if err == nil && w > 0 {
					c.mu.Lock()
					t.info.Window = w
					c.lastSlashingAt = time.Now()
					c.mu.Unlock()
				}
			}
		}
		c.mu.Lock()
		if c.met != nil {
			c.met.Window(c.name, c.cfg.ChainID, valcons, t.info.Moniker, t.info.Missed, t.info.Window)
		}
		c.mu.Unlock()
	}
}

func (c *Chain) setNoNodes(v bool) {
	c.mu.Lock()
	if v && !c.noNodes {
		c.noNodesSince = time.Now()
	}
	c.noNodes = v
	c.mu.Unlock()
}
