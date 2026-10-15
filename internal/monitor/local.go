package monitor

import (
	"encoding/json"
	"errors"
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
//   - filesystem free space (a full disk is a top validator killer), free
//     inodes, how fast it fills, and whether it can be read at all
//   - priv_validator_state.json — the last height/round/step the consensus
//     key signed. Height going backwards means the state file was copied or
//     restored: the classic pre-double-sign footgun. A file that exists but
//     doesn't parse is corrupt — a restart on it can't be trusted.
//
// Runs regardless of RPC health — a dead node with a full disk explains
// itself here even while its endpoints are unreachable.
func (c *Chain) probeLocal(n *nodeState) {
	home := n.cfg.HomeDir
	if home == "" || c.cfg.IsChainOnly() {
		return // no mount, or chain role — local checks are the sidecar's job
	}
	chainID := c.cfg.ChainID

	c.probeDisk(n, home)
	c.probeSignerFile(n, home)

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
	// a node whose state was never above 0 has never signed (an archive or
	// RPC node next to its own home): nothing to stall
	if a.SignerStallMin > 0 && c.lastHeight > 0 && n.signerMax > 0 && n.signerHeight < c.lastHeight &&
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

// fsStat is what the local checks need from statfs(2).
type fsStat struct {
	free  float64 // bytes available to an unprivileged writer
	files float64 // inodes in total (0 = the filesystem doesn't say)
	ffree float64 // free inodes
}

// statfs reads the filesystem holding path; a variable so tests can fake a
// full, failing or filling disk.
var statfs = func(path string) (fsStat, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return fsStat{}, err
	}
	return fsStat{
		free:  float64(st.Bavail) * float64(st.Bsize), //nolint:unconvert -- Bavail/Bsize widths differ across platforms
		files: float64(st.Files),                      //nolint:unconvert
		ffree: float64(st.Ffree),                      //nolint:unconvert
	}, nil
}

// diskErr reports errors that mean the disk itself is failing or gone, as
// opposed to a missing file or a permission problem.
func diskErr(err error) bool {
	return errors.Is(err, syscall.EIO) || errors.Is(err, syscall.ENOTCONN) ||
		errors.Is(err, syscall.ESTALE) || errors.Is(err, syscall.ENXIO) || errors.Is(err, syscall.ENODEV)
}

const (
	fillWindow  = time.Hour        // free-space samples kept for the fill rate
	fillMinSpan = 15 * time.Minute // shortest span a rate is trusted over
)

type diskSample struct {
	at   time.Time
	free float64
}

// probeDisk checks the node home's filesystem: free space, free inodes, the
// rate it fills at, and whether it answers at all.
func (c *Chain) probeDisk(n *nodeState, home string) {
	a := c.cfg.Alerts
	chainID := c.cfg.ChainID
	errKey := "disk-error:" + n.cfg.URL

	st, err := statfs(home)
	if err != nil {
		// one failure can be a remount in progress; two in a row is a disk
		// that's gone, unmounted or failing
		n.diskErrs++
		if n.diskErrs >= 2 && !n.diskErrAlarm && !c.eng.HasOpen(errKey) {
			n.diskErrAlarm = true
			c.alert(chainID, nil, errKey, false, "critical",
				fmt.Sprintf("node %s home filesystem can't be read on %s: %s — the disk failed, was unmounted or detached",
					nodeLabel(n), chainID, err))
		}
		return
	}
	n.diskErrs = 0
	free := st.free
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

	// inodes: a disk with space left but no free inodes can't create files
	// either (many small files: logs, snapshots, a wal per height)
	key = "disk-inodes:" + n.cfg.URL
	if a.InodeFreePctAlert > 0 && st.files > 0 {
		pct := 100 * st.ffree / st.files
		switch {
		case pct < float64(a.InodeFreePctAlert) && !n.inodeAlarm && !c.eng.HasOpen(key):
			n.inodeAlarm = true
			c.alert(chainID, nil, key, false, "warning",
				fmt.Sprintf("node %s home filesystem has %.1f%% inodes free (< %d%%) on %s — it can't create files once they run out",
					nodeLabel(n), pct, a.InodeFreePctAlert, chainID))
		case pct >= float64(a.InodeFreePctAlert) && (n.inodeAlarm || c.eng.HasOpen(key)):
			n.inodeAlarm = false
			c.alert(chainID, nil, key, true, "info",
				fmt.Sprintf("node %s inodes recovered on %s (%.1f%% free)", nodeLabel(n), chainID, pct))
		}
	}

	c.probeFillRate(n, free)
	c.probeReadonly(n)

	if n.diskErrAlarm || c.eng.HasOpen(errKey) {
		n.diskErrAlarm = false
		c.alert(chainID, nil, errKey, true, "info",
			fmt.Sprintf("node %s home filesystem readable again on %s", nodeLabel(n), chainID))
	}
}

// probeFillRate warns when, at the rate free space fell over the last hour,
// the disk fills within disk_fill_hours_alert — earlier than a fixed
// threshold on a fast-growing node, and quiet on a slow one.
func (c *Chain) probeFillRate(n *nodeState, free float64) {
	a := c.cfg.Alerts
	now := time.Now()
	n.diskSamples = append(n.diskSamples, diskSample{at: now, free: free})
	for len(n.diskSamples) > 1 && now.Sub(n.diskSamples[0].at) > fillWindow {
		n.diskSamples = n.diskSamples[1:]
	}
	if a.DiskFillHoursAlert <= 0 {
		return
	}
	first := n.diskSamples[0]
	span := now.Sub(first.at)
	if span < fillMinSpan {
		return
	}
	rate := (first.free - free) / span.Hours() // bytes per hour
	key := "disk-filling:" + n.cfg.URL
	chainID := c.cfg.ChainID
	eta := -1.0
	if rate > 0 {
		eta = free / rate
	}
	switch {
	case eta >= 0 && eta < float64(a.DiskFillHoursAlert) && !n.fillAlarm && !c.eng.HasOpen(key):
		n.fillAlarm = true
		c.alert(chainID, nil, key, false, "warning",
			fmt.Sprintf("node %s home filesystem is filling at %.2f GiB/h on %s — full in about %s (%.1f GiB free)",
				nodeLabel(n), rate/1073741824, chainID, fmtHours(eta), free/1073741824))
	case (eta < 0 || eta >= 1.5*float64(a.DiskFillHoursAlert)) && (n.fillAlarm || c.eng.HasOpen(key)):
		// hysteresis: clear only well past the threshold so a bursty rate
		// doesn't flap
		n.fillAlarm = false
		c.alert(chainID, nil, key, true, "info",
			fmt.Sprintf("node %s disk fill rate eased on %s (%.1f GiB free)", nodeLabel(n), chainID, free/1073741824))
	}
}

func fmtHours(h float64) string {
	if h < 1 {
		return fmt.Sprintf("%d minutes", int(h*60+0.5))
	}
	return fmt.Sprintf("%.1f hours", h)
}

// probeSignerFile reads priv_validator_state.json: its height feeds the
// signer checks, and a file that is unreadable or doesn't parse is an alert.
func (c *Chain) probeSignerFile(n *nodeState, home string) {
	chainID := c.cfg.ChainID
	stateFile := filepath.Join(home, "data", "priv_validator_state.json")
	raw, err := os.ReadFile(stateFile)
	if err != nil {
		if diskErr(err) {
			// an I/O error reading it is the disk, not the file
			errKey := "disk-error:" + n.cfg.URL
			if !n.diskErrAlarm && !c.eng.HasOpen(errKey) {
				n.diskErrAlarm = true
				c.alert(chainID, nil, errKey, false, "critical",
					fmt.Sprintf("node %s can't read its home on %s: %s — disk I/O error", nodeLabel(n), chainID, err))
			}
			return
		}
		// missing state file = fresh/empty state. On restart the node signs
		// from height 0 — if a twin exists elsewhere that is a double-sign.
		key := "signer-state:" + n.cfg.URL
		if !n.signFileAlarm && !c.eng.HasOpen(key) {
			n.signFileAlarm = true
			c.alert(chainID, nil, key, false, "warning",
				fmt.Sprintf("node %s priv_validator_state.json unreadable on %s: %s — signing state unknown",
					nodeLabel(n), chainID, err))
		}
		return
	}
	var ps struct {
		Height json.RawMessage `json:"height"`
		Round  int64           `json:"round"`
		Step   int64           `json:"step"`
	}
	h, perr := int64(0), json.Unmarshal(raw, &ps)
	if perr == nil {
		h, perr = parseJSONInt(ps.Height)
	}
	corruptKey := "signer-corrupt:" + n.cfg.URL
	if perr != nil {
		// cometbft replaces the file atomically, so a torn read is rare —
		// still, two bad reads in a row before calling it corrupt
		n.signBad++
		if n.signBad >= 2 && !n.corruptAlarm && !c.eng.HasOpen(corruptKey) {
			n.corruptAlarm = true
			c.alert(chainID, nil, corruptKey, false, "critical",
				fmt.Sprintf("node %s priv_validator_state.json is corrupt on %s (%d bytes: %s) — don't restart the validator on it; DOUBLE-SIGN risk",
					nodeLabel(n), chainID, len(raw), perr))
		}
		return
	}
	n.signBad = 0
	c.updateSigner(n, h)
	if n.corruptAlarm || c.eng.HasOpen(corruptKey) {
		n.corruptAlarm = false
		c.alert(chainID, nil, corruptKey, true, "info",
			fmt.Sprintf("node %s priv_validator_state.json parses again on %s (height %d)", nodeLabel(n), chainID, h))
	}
	key := "signer-state:" + n.cfg.URL
	if n.signFileAlarm || c.eng.HasOpen(key) {
		n.signFileAlarm = false
		c.alert(chainID, nil, key, true, "info",
			fmt.Sprintf("node %s priv_validator_state.json readable again on %s", nodeLabel(n), chainID))
	}
}

// probeReadonly reads the host's mount table (node host.mounts) for the
// filesystem holding the node home: the kernel remounts a filesystem
// read-only after disk errors, and the node can't write from then on. The
// home mount cometduty itself sees is always read-only, so only the host's
// table tells.
func (c *Chain) probeReadonly(n *nodeState) {
	hc := n.cfg.Host
	if hc == nil || hc.Mounts == "" || hc.HomePath == "" {
		return
	}
	raw, err := os.ReadFile(hc.Mounts)
	if err != nil {
		return
	}
	mnt, ro := mountOf(string(raw), hc.HomePath)
	if mnt == "" {
		return
	}
	chainID := c.cfg.ChainID
	key := "disk-readonly:" + n.cfg.URL
	switch {
	case ro && !n.roAlarm && !c.eng.HasOpen(key):
		n.roAlarm = true
		c.alert(chainID, nil, key, false, "critical",
			fmt.Sprintf("node %s home filesystem %s is mounted read-only on its host on %s — usually remounted after disk errors; the node can't write",
				nodeLabel(n), mnt, chainID))
	case !ro && (n.roAlarm || c.eng.HasOpen(key)):
		n.roAlarm = false
		c.alert(chainID, nil, key, true, "info",
			fmt.Sprintf("node %s home filesystem %s is read-write again on %s", nodeLabel(n), mnt, chainID))
	}
}

// mountOf finds the mount holding path in a /proc/<pid>/mounts table (the
// longest mount point that contains it) and whether it is read-only.
func mountOf(table, path string) (string, bool) {
	best, ro := "", false
	for _, line := range strings.Split(table, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		mp := strings.ReplaceAll(f[1], "\\040", " ")
		if !(path == mp || mp == "/" || strings.HasPrefix(path, strings.TrimSuffix(mp, "/")+"/")) || len(mp) < len(best) {
			continue
		}
		// a later mount on the same point shadows an earlier one
		best, ro = mp, false
		for _, o := range strings.Split(f[3], ",") {
			ro = ro || o == "ro"
		}
	}
	return best, ro
}
