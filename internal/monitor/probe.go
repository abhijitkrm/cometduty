package monitor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/abhijitkrm/cometduty/internal/config"
	"github.com/abhijitkrm/cometduty/internal/rpc"
)

// ResolvedValidator is a validator's on-chain identity as the monitor sees
// it — used by `status` to classify signatures without starting the monitor.
type ResolvedValidator struct {
	Valoper    string
	Moniker    string
	Valcons    string
	ConsHex    string
	Bonded     bool
	Jailed     bool
	Tombstoned bool
	Missed     int64
	Window     int64
	Err        error
}

// ResolveValidators resolves each configured validator's consensus address,
// moniker, and slashing record via the given client. Same code path as
// refreshValInfo; per-validator errors are reported inline, not fatal.
func ResolveValidators(ctx context.Context, cl *rpc.Client, vcs []config.ValidatorConfig) []ResolvedValidator {
	out := make([]ResolvedValidator, 0, len(vcs))
	for _, vc := range vcs {
		rv := ResolvedValidator{Valoper: vc.ValoperAddress, Moniker: vc.Label}
		if vc.ValconsOverride != "" || strings.Contains(vc.ValoperAddress, "valcons") {
			addr, valcons, err := resolveConsAddress(vc.ValoperAddress, vc.ValconsOverride, vc.ConsPrefix, nil)
			if err != nil {
				rv.Err = err
				out = append(out, rv)
				continue
			}
			rv.Valcons, rv.ConsHex, rv.Bonded = valcons, strings.ToUpper(fmt.Sprintf("%X", addr)), true
			out = append(out, rv)
			continue
		}
		qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		consAddr, moniker, jailed, bonded, _, err := getValidatorRecord(qctx, cl, vc.ValoperAddress)
		cancel()
		if err != nil {
			rv.Err = err
			out = append(out, rv)
			continue
		}
		addr, valcons, err := resolveConsAddress(vc.ValoperAddress, vc.ValconsOverride, vc.ConsPrefix, consAddr)
		if err != nil {
			rv.Err = err
			out = append(out, rv)
			continue
		}
		rv.ConsHex = strings.ToUpper(fmt.Sprintf("%X", addr))
		rv.Valcons = valcons
		rv.Bonded, rv.Jailed = bonded, jailed
		if rv.Moniker == "" {
			rv.Moniker = moniker
		}
		sctx, scancel := context.WithTimeout(ctx, 10*time.Second)
		tomb, missed, _, serr := getSigningInfo(sctx, cl, valcons)
		scancel()
		if serr == nil {
			rv.Tombstoned, rv.Missed = tomb, missed
			wctx, wcancel := context.WithTimeout(ctx, 10*time.Second)
			if w, werr := getSlashingWindow(wctx, cl); werr == nil {
				rv.Window = w
			}
			wcancel()
		}
		out = append(out, rv)
	}
	return out
}

// ProbeResult is one line of a `validate --live` report.
type ProbeResult struct {
	Subject string // node URL or valoper address
	OK      bool
	Detail  string // "height 123, 4 peers, in sync" or the failure reason
}

// ProbeChain live-checks a chain config without starting the monitor: every
// node's reachability/chain-id/sync state, then every validator's staking and
// slashing record via the first healthy endpoint. It exercises the same code
// paths the monitor itself uses, so a green report means the monitor will work.
func ProbeChain(ctx context.Context, cc *config.ChainConfig) []ProbeResult {
	var out []ProbeResult

	var client *rpc.Client
	for _, n := range cc.Nodes {
		cl, st, err := probeEndpoint(ctx, n, cc.ChainID)
		switch {
		case err != nil && st != nil:
			out = append(out, ProbeResult{n.URL, false, err.Error()})
		case err != nil:
			out = append(out, ProbeResult{n.URL, false, err.Error()})
		default:
			peers := int64(-1)
			pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			if ni, e := cl.NetInfo(pctx); e == nil {
				peers = int64(ni.NPeers)
			}
			cancel()
			detail := fmt.Sprintf("height %d, in sync", st.SyncInfo.LatestBlockHeight)
			if peers >= 0 {
				detail = fmt.Sprintf("height %d, %d peers, in sync", st.SyncInfo.LatestBlockHeight, peers)
			}
			out = append(out, ProbeResult{n.URL, true, detail})
			if client == nil {
				client = cl
			}
		}
	}
	if len(cc.Nodes) == 0 {
		out = append(out, ProbeResult{"nodes", false, "no nodes configured"})
	}

	if client == nil {
		out = append(out, ProbeResult{"validators", false, "skipped — no healthy endpoint"})
		return out
	}

	for _, vc := range cc.Validators {
		subj := vc.ValoperAddress
		if vc.Label != "" {
			subj = vc.Label + " (" + vc.ValoperAddress + ")"
		}
		if vc.ValconsOverride != "" || strings.Contains(vc.ValoperAddress, "valcons") {
			_, valcons, err := resolveConsAddress(vc.ValoperAddress, vc.ValconsOverride, vc.ConsPrefix, nil)
			if err != nil {
				out = append(out, ProbeResult{subj, false, "bad valcons: " + err.Error()})
			} else {
				out = append(out, ProbeResult{subj, true, "valcons " + valcons + " (direct)"})
			}
			continue
		}
		qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		consAddr, moniker, jailed, bonded, _, err := getValidatorRecord(qctx, client, vc.ValoperAddress)
		cancel()
		if err != nil {
			out = append(out, ProbeResult{subj, false, err.Error()})
			continue
		}
		_, valcons, err := resolveConsAddress(vc.ValoperAddress, vc.ValconsOverride, vc.ConsPrefix, consAddr)
		if err != nil {
			out = append(out, ProbeResult{subj, false, "resolving cons address: " + err.Error()})
			continue
		}
		status := "bonded"
		if !bonded {
			status = "NOT BONDED"
		}
		if jailed {
			status += ", jailed"
		}
		detail := fmt.Sprintf("%s → %s (%s)", moniker, valcons, status)

		// slashing info is optional — enrich when the module exists
		sctx, scancel := context.WithTimeout(ctx, 10*time.Second)
		tomb, missed, _, serr := getSigningInfo(sctx, client, valcons)
		scancel()
		switch {
		case errors.Is(serr, errNoSlashingModule):
			detail += ", no slashing module"
		case serr == nil:
			detail += fmt.Sprintf(", missed %d", missed)
			if tomb {
				detail += ", TOMBSTONED"
			}
		}
		// A jailed/unbonded validator resolves fine and is worth monitoring —
		// report it as OK with the state in the detail (it's a finding, not a
		// config error).
		out = append(out, ProbeResult{subj, true, detail})
	}
	return out
}
