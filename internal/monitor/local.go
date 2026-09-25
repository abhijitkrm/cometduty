package monitor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// probeLocal runs checks that need the node's home dir mounted read-only
// (node home: in config). None of this is reachable over RPC:
//
//   - filesystem free space (a full disk is a top validator killer)
//   - priv_validator_state.json — the last height/round/step the consensus
//     key signed. Height going backwards means the state file was copied or
//     restored: the classic pre-double-sign footgun.
//
// Runs regardless of RPC health — a dead node with a full disk explains
// itself here even while its endpoints are unreachable.
func (c *Chain) probeLocal(n *nodeState) {
	home := n.cfg.HomeDir
	if home == "" || c.cfg.IsChainOnly() {
		return // no mount, or chain role — local checks are the sidecar's job
	}
	a := c.cfg.Alerts
	chainID := c.cfg.ChainID

	// --- disk free ---
	var st syscall.Statfs_t
	if err := syscall.Statfs(home, &st); err == nil {
		free := float64(st.Bavail) * float64(st.Bsize) //nolint:unconvert -- Bavail/Bsize widths differ across platforms
		c.mu.Lock()
		n.diskFree = free
		c.mu.Unlock()
		key := "disk-low:" + n.cfg.URL
		if a.DiskFreeBytesAlert > 0 {
			switch {
			case free < float64(a.DiskFreeBytesAlert) && !n.diskAlarm && !c.eng.HasOpen(key):
				n.diskAlarm = true
				c.alert(chainID, nil, key, false, "warning",
					fmt.Sprintf("node %s home filesystem has %.1f GiB free (< %d bytes) on %s — a full disk halts the validator",
						nodeLabel(n), free/1073741824, a.DiskFreeBytesAlert, chainID))
			case free >= float64(a.DiskFreeBytesAlert) && (n.diskAlarm || c.eng.HasOpen(key)):
				n.diskAlarm = false
				c.alert(chainID, nil, key, true, "info",
					fmt.Sprintf("node %s disk pressure cleared on %s (%.1f GiB free)", nodeLabel(n), chainID, free/1073741824))
			}
		}
	}

	// --- priv_validator_state.json ---
	stateFile := filepath.Join(home, "data", "priv_validator_state.json")
	raw, err := os.ReadFile(stateFile)
	if err != nil {
		// missing state file = fresh/empty state. On restart the node signs
		// from height 0 — if a twin exists elsewhere that is a double-sign.
		key := "signer-state:" + n.cfg.URL
		if !n.signFileAlarm && !c.eng.HasOpen(key) {
			n.signFileAlarm = true
			c.alert(chainID, nil, key, false, "warning",
				fmt.Sprintf("node %s priv_validator_state.json unreadable on %s: %s — signing state unknown",
					nodeLabel(n), chainID, err))
		}
	} else {
		var ps struct {
			Height json.RawMessage `json:"height"`
			Round  int64           `json:"round"`
			Step   int64           `json:"step"`
		}
		if jerr := json.Unmarshal(raw, &ps); jerr == nil {
			h, herr := parseJSONInt(ps.Height)
			if herr == nil {
				c.updateSigner(n, h)
			}
			key := "signer-state:" + n.cfg.URL
			if n.signFileAlarm || c.eng.HasOpen(key) {
				n.signFileAlarm = false
				c.alert(chainID, nil, key, true, "info",
					fmt.Sprintf("node %s priv_validator_state.json readable again on %s", nodeLabel(n), chainID))
			}
		}
	}

	if c.met != nil {
		c.mu.Lock()
		diskFree, signerHeight := n.diskFree, n.signerHeight
		c.mu.Unlock()
		c.met.NodeLocal(c.name, chainID, n.cfg.URL, n.cfg.Validator, diskFree, float64(signerHeight))
	}
}

// updateSigner folds a new priv_validator_state height into node state and
// raises the signer alarms. Called under no lock — it takes c.mu itself.
func (c *Chain) updateSigner(n *nodeState, h int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	chainID := c.cfg.ChainID
	a := c.cfg.Alerts

	// regression: the recorded signing height moved backwards. Something
	// copied or restored priv_validator_state.json — if another copy of this
	// key is live anywhere, the next signature double-signs.
	if n.signerMax > 0 && h < n.signerMax {
		key := "signer-regressed:" + n.cfg.URL
		if !n.regressAlarm && !c.eng.HasOpen(key) {
			n.regressAlarm = true
			c.alert(chainID, nil, key, false, "critical",
				fmt.Sprintf("node %s signer state regressed on %s: height %d < previous %d — priv_validator_state was copied/restored, DOUBLE-SIGN risk if another instance signs",
					nodeLabel(n), chainID, h, n.signerMax))
		}
	} else if n.regressAlarm || (n.signerMax > 0 && c.eng.HasOpen("signer-regressed:"+n.cfg.URL)) {
		if h >= n.signerMax {
			n.regressAlarm = false
			c.alert(chainID, nil, "signer-regressed:"+n.cfg.URL, true, "info",
				fmt.Sprintf("node %s signer state re-advanced to %d on %s — verify no other instance signed meanwhile",
					nodeLabel(n), h, chainID))
		}
	}
	if h > n.signerMax {
		n.signerMax = h
	}
	if h != n.signerHeight {
		n.signerHeight = h
		n.signerAt = time.Now()
	} else if n.signerAt.IsZero() {
		n.signerAt = time.Now() // first observation — start the stall clock
	}

	// stalled signer: state file hasn't advanced while the chain has — the
	// node isn't signing (bad key, crashed signer thread, wrong height).
	if a.SignerStallMin > 0 && c.lastHeight > 0 && n.signerHeight < c.lastHeight &&
		time.Since(n.signerAt) > time.Duration(a.SignerStallMin)*time.Minute {
		key := "signer-stalled:" + n.cfg.URL
		if !n.signerAlarm && !c.eng.HasOpen(key) {
			n.signerAlarm = true
			c.alert(chainID, nil, key, false, "warning",
				fmt.Sprintf("node %s hasn't signed since height %d on %s (> %d minutes; chain is at %d) — check the consensus key",
					nodeLabel(n), n.signerHeight, chainID, a.SignerStallMin, c.lastHeight))
		}
	} else if n.signerAlarm {
		n.signerAlarm = false
		c.alert(chainID, nil, "signer-stalled:"+n.cfg.URL, true, "info",
			fmt.Sprintf("node %s resumed signing on %s (height %d)", nodeLabel(n), chainID, n.signerHeight))
	}
}

// parseJSONInt handles both JSON shapes cometbft writes for the state height:
// a quoted decimal string ("12345") and, on some versions, a bare number.
func parseJSONInt(raw json.RawMessage) (int64, error) {
	s := strings.Trim(string(raw), `"`)
	if s == "" {
		return 0, fmt.Errorf("empty height")
	}
	var n int64
	_, err := fmt.Sscan(s, &n)
	return n, err
}
