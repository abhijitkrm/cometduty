package monitor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/cometduty/cometduty/internal/rpc"
)

const (
	queryNewBlock = `tm.event='NewBlock'`
	queryVote     = `tm.event='Vote'`

	// if no NewBlock arrives for this long the connection is presumed dead
	// (or the chain has halted) — we reconnect and let the stall alarm speak.
	wsIdleTimeout = 90 * time.Second
)

// wsLoop owns the websocket subscription for a chain until it breaks.
func (c *Chain) wsLoop(ctx context.Context) error {
	c.mu.Lock()
	endpoint := c.clientURL
	c.mu.Unlock()
	if endpoint == "" {
		return errors.New("no rpc client")
	}

	// prefer a healthy node that isn't marked disable_vote for the
	// subscription (endpoints that only serve REST shouldn't carry ws)
	c.mu.Lock()
	for _, n := range c.nodes {
		if !n.down && !n.cfg.DisableVote {
			endpoint = n.cfg.URL
			break
		}
	}
	// find the node config for this endpoint to get headers/TLS options
	var nodeCfg = struct {
		headers     map[string]string
		insecureTLS bool
	}{}
	for _, n := range c.nodes {
		if n.cfg.URL == endpoint {
			nodeCfg.headers = n.cfg.Headers
			nodeCfg.insecureTLS = n.cfg.InsecureTLS
		}
	}
	c.mu.Unlock()

	ws, err := rpc.DialWS(ctx, endpoint, &rpc.WSOptions{
		Headers:     nodeCfg.headers,
		InsecureTLS: nodeCfg.insecureTLS,
		PingEvery:   30 * time.Second,
	})
	if err != nil {
		return err
	}
	defer ws.Close()

	for _, q := range []string{queryNewBlock, queryVote} {
		if err := ws.Subscribe(ctx, q); err != nil {
			return fmt.Errorf("subscribing %s: %w", q, err)
		}
	}
	c.log.Info("watching for NewBlock and Vote events", "endpoint", ws.URL())

	// Reads happen on a helper goroutine so ctx cancellation can interrupt the
	// blocking socket read: on return, the deferred ws.Close() unblocks it.
	type readResult struct {
		ev  *rpc.WSEvent
		err error
	}
	reads := make(chan readResult, 4)
	go func() {
		for {
			ev, err := ws.ReadEvent()
			select {
			case reads <- readResult{ev, err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()

	idle := time.NewTicker(15 * time.Second)
	defer idle.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-idle.C:
			c.mu.Lock()
			silent := !c.lastBlockTime.IsZero() && time.Since(c.lastBlockTime) > wsIdleTimeout
			c.mu.Unlock()
			if silent {
				return errors.New("no NewBlock events for 90s — reconnecting")
			}
		case rr := <-reads:
			if rr.err != nil {
				return rr.err
			}
			switch rr.ev.Type() {
			case "tendermint/event/NewBlock":
				c.handleBlock(rr.ev.Value())
			case "tendermint/event/Vote":
				c.handleVote(rr.ev.Value())
			}
		}
	}
}

// handleVote records prevote/precommit observations per validator+height.
func (c *Chain) handleVote(raw []byte) {
	v, err := decodeVote(raw)
	if err != nil || v.Vote.ValidatorAddress == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if t, ok := c.byAddr[v.Vote.ValidatorAddress]; ok {
		t.vt.observe(int64(v.Vote.Height), v.state())
	}
}

// handleBlock classifies the commit for every monitored validator, updates
// stats/rings/metrics, and pushes a dashboard status.
func (c *Chain) handleBlock(raw []byte) {
	b, err := decodeBlock(raw)
	if err != nil {
		c.log.Debug("could not decode block event", "err", err)
		return
	}
	height := b.height()
	now := time.Now()

	c.mu.Lock()
	sincePrev := now.Sub(c.lastBlockTime).Seconds()
	c.lastBlockTime = now
	c.lastHeight = height
	chainID := c.cfg.ChainID
	targets := c.targets
	c.mu.Unlock()

	for _, t := range targets {
		c.mu.Lock()
		if t.info == nil || t.info.ConsHex == "" {
			c.mu.Unlock()
			continue
		}
		st, ratio := classify(b, t.info.ConsHex, t.vt)
		bonded := t.info.Bonded
		if !bonded {
			// still record, but as unknown so the grid shows grey not red
			st = SignUnknown
		}
		t.blocks = append([]int{int(st)}, t.blocks[:len(t.blocks)-1]...)
		switch st {
		case SignMissed:
			t.miss++
			t.consec++
		case SignPrevote:
			t.pvMiss++
			t.miss++
			t.consec++
		case SignPrecommit:
			t.pcMiss++
			t.miss++
			t.consec++
		case SignSigned:
			t.signs++
			t.consec = 0
		case SignProposed:
			t.props++
			t.signs++
			t.consec = 0
		}
		missedNow := st < SignSigned && bonded
		moniker := t.info.Moniker
		valcons := t.info.Valcons
		missed, window := t.info.Missed, t.info.Window
		tomb, jailed := t.info.Tombstoned, t.info.Jailed
		consec := t.consec
		c.mu.Unlock()

		if missedNow {
			c.log.Warn("missed block",
				"moniker", moniker, "height", height-1, "state", st.String())
		}
		if c.met != nil {
			c.met.BlockResult(c.name, chainID, valcons, moniker, st, consec)
			c.met.SignatureRatio(c.name, chainID, ratio)
		}
		c.publishStatus(t, height, missedNow, st, moniker, valcons, bonded, missed, window, tomb, jailed)
	}
	if c.met != nil {
		c.met.LastBlock(c.name, chainID, height, sincePrev)
	}
	if height%20 == 0 {
		c.log.Info("block", "height", height)
	}
}

func (c *Chain) publishStatus(t *Target, height int64, missedNow bool, st SignState, moniker, valcons string, bonded bool, missed, window int64, tomb, jailed bool) {
	if c.hub == nil {
		return
	}
	hideLogs := c.rootFn().HideLogs // before c.mu — lock ordering (s.mu → c.mu)
	c.mu.Lock()
	nodes := len(c.nodes)
	healthy := 0
	var errMsgs string
	for _, n := range c.nodes {
		if !n.down {
			healthy++
		} else if !hideLogs && n.lastMsg != "" {
			errMsgs += "\n - " + n.cfg.URL + ": " + n.lastMsg
		}
	}
	blocks := append([]int{}, t.blocks...)
	chainID := c.cfg.ChainID
	c.mu.Unlock()

	info := ""
	for _, k := range c.eng.ActiveAlerts(c.name) {
		info += "🚨 " + k + "\n"
	}
	if missedNow {
		info += fmt.Sprintf("missed block %d (%s)\n", height-1, st)
	}
	if tomb {
		info += "validator is tombstoned\n"
	} else if jailed {
		info += "validator is jailed\n"
	}
	info += errMsgs

	c.hub.PublishStatus(&Status{
		MsgType:      "status",
		Name:         c.name,
		ChainID:      chainID,
		Moniker:      moniker,
		Validator:    valcons,
		Bonded:       bonded,
		Jailed:       jailed,
		Tombstoned:   tomb,
		Missed:       missed,
		Window:       window,
		Nodes:        nodes,
		HealthyNodes: healthy,
		ActiveAlerts: c.eng.ActiveCount(c.name),
		Height:       height,
		LastError:    info,
		Blocks:       blocks,
	})
}
