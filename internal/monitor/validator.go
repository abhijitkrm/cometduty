package monitor

import (
	"context"
	"errors"
	"fmt"
	"strings"

	queryv1beta1 "cosmossdk.io/api/cosmos/base/query/v1beta1"
	slashingv1beta1 "cosmossdk.io/api/cosmos/slashing/v1beta1"
	stakingv1beta1 "cosmossdk.io/api/cosmos/staking/v1beta1"
	"google.golang.org/protobuf/proto"

	"github.com/abhijitkrm/cometduty/internal/bech32"
	"github.com/abhijitkrm/cometduty/internal/consensus"
	"github.com/abhijitkrm/cometduty/internal/rpc"
)

// ValInfo is the periodically-refreshed on-chain state for one validator.
type ValInfo struct {
	Moniker     string
	Bonded      bool
	Jailed      bool
	Tombstoned  bool
	Tokens      string // bonded stake (delegations move this)
	Missed      int64  // chain-reported missed counter for the current window
	Window      int64  // signed blocks window
	JailedUntil int64  // unix ts until unjail is allowed; 0 when not jailed
	ConsAddr    []byte
	ConsHex     string // upper-case hex, matches vote/block signature data
	Valcons     string // bech32 consensus address
}

// Non-standard valoper→valcons prefix pairs that can't be derived mechanically.
// Users can always set cons_prefix in config instead.
var consPrefixOverrides = map[string]string{
	"iva": "ica", // IRIS hub
}

// resolveConsAddress determines the consensus address bytes + valcons bech32 for
// a validator target. v is the staking validator record (nil if we only have a
// valcons address), consPub the raw consensus pubkey bytes when known.
func resolveConsAddress(valoper, override, consPrefix string, consAddrFromQuery []byte) (addr []byte, valcons string, err error) {
	// Direct valcons path: no staking lookup needed.
	if override != "" {
		hrp, bz, e := bech32.Decode(override)
		if e != nil || len(bz) != 20 {
			return nil, "", fmt.Errorf("invalid valcons_override %q: %v", override, e)
		}
		_ = hrp
		return bz, override, nil
	}
	if strings.Contains(valoper, "valcons") {
		_, bz, e := bech32.Decode(valoper)
		if e != nil || len(bz) != 20 {
			return nil, "", fmt.Errorf("invalid valcons address %q: %v", valoper, e)
		}
		return bz, valoper, nil
	}
	if len(consAddrFromQuery) != 20 {
		return nil, "", errors.New("no consensus address available")
	}

	// Derive the bech32 prefix.
	if consPrefix == "" {
		hrp, e := bech32.HRP(valoper)
		if e != nil {
			return nil, "", fmt.Errorf("decoding valoper %q: %w", valoper, e)
		}
		switch {
		case strings.HasSuffix(hrp, "valoper"):
			consPrefix = strings.TrimSuffix(hrp, "valoper") + "valcons"
		default:
			if p, ok := consPrefixOverrides[hrp]; ok {
				consPrefix = p
			}
		}
	}
	if consPrefix == "" {
		return nil, "", fmt.Errorf("cannot derive valcons prefix from %q — set cons_prefix or valcons_override", valoper)
	}
	vc, err := bech32.Encode(consPrefix, consAddrFromQuery)
	if err != nil {
		return nil, "", err
	}
	return consAddrFromQuery, vc, nil
}

const (
	pathStakingValidator  = "/cosmos.staking.v1beta1.Query/Validator"
	pathStakingValidators = "/cosmos.staking.v1beta1.Query/Validators"
	pathSlashingInfo      = "/cosmos.slashing.v1beta1.Query/SigningInfo"
	pathSlashingParams    = "/cosmos.slashing.v1beta1.Query/Params"
)

// getValidatorRecord fetches the staking record to learn the consensus pubkey,
// moniker, jailed flag, and bond status.
func getValidatorRecord(ctx context.Context, c *rpc.Client, valoper string) (consAddr []byte, moniker string, jailed, bonded bool, tokens string, err error) {
	req := &stakingv1beta1.QueryValidatorRequest{ValidatorAddr: valoper}
	b, err := proto.Marshal(req)
	if err != nil {
		return nil, "", false, false, "", err
	}
	resp, err := c.ABCIQuery(ctx, pathStakingValidator, b)
	if err != nil {
		return nil, "", false, false, "", fmt.Errorf("querying validator %s: %w", valoper, err)
	}
	out := &stakingv1beta1.QueryValidatorResponse{}
	if err := proto.Unmarshal(resp.Value, out); err != nil {
		return nil, "", false, false, "", fmt.Errorf("decoding validator response: %w", err)
	}
	v := out.GetValidator()
	if v == nil {
		return nil, "", false, false, "", fmt.Errorf("empty validator record for %s", valoper)
	}
	pk := v.GetConsensusPubkey()
	if pk == nil {
		return nil, "", false, false, "", fmt.Errorf("no consensus pubkey for %s", valoper)
	}
	consAddr, err = consensus.AnyToAddress(pk.GetTypeUrl(), pk.GetValue())
	if err != nil {
		return nil, "", false, false, "", fmt.Errorf("decoding consensus pubkey (%s): %w", pk.GetTypeUrl(), err)
	}
	return consAddr, v.GetDescription().GetMoniker(), v.GetJailed(),
		v.GetStatus() == stakingv1beta1.BondStatus_BOND_STATUS_BONDED, v.GetTokens(), nil
}

// setEntry is one validator's membership state for set-watch diffs.
type setEntry struct {
	valoper string
	moniker string
	jailed  bool
	bonded  bool
}

// listValidators fetches the full staking set (all bond statuses, one page) —
// the set-watch baseline for join/leave/jail detection.
func listValidators(ctx context.Context, c *rpc.Client) ([]setEntry, error) {
	req := &stakingv1beta1.QueryValidatorsRequest{
		Pagination: &queryv1beta1.PageRequest{Limit: 500},
	}
	b, err := proto.Marshal(req)
	if err != nil {
		return nil, err
	}
	resp, err := c.ABCIQuery(ctx, pathStakingValidators, b)
	if err != nil {
		return nil, fmt.Errorf("listing validators: %w", err)
	}
	out := &stakingv1beta1.QueryValidatorsResponse{}
	if err := proto.Unmarshal(resp.Value, out); err != nil {
		return nil, fmt.Errorf("decoding validators response: %w", err)
	}
	entries := make([]setEntry, 0, len(out.GetValidators()))
	for _, v := range out.GetValidators() {
		entries = append(entries, setEntry{
			valoper: v.GetOperatorAddress(),
			moniker: v.GetDescription().GetMoniker(),
			jailed:  v.GetJailed(),
			bonded:  v.GetStatus() == stakingv1beta1.BondStatus_BOND_STATUS_BONDED,
		})
	}
	return entries, nil
}

// getSigningInfo fetches tombstoned, missed counter, and jailed_until.
// Returns errNoSlashingModule when the chain lacks x/slashing.
var errNoSlashingModule = errors.New("slashing module not available on this chain")

func getSigningInfo(ctx context.Context, c *rpc.Client, valcons string) (tombstoned bool, missed, jailedUntil int64, err error) {
	req := &slashingv1beta1.QuerySigningInfoRequest{ConsAddress: valcons}
	b, err := proto.Marshal(req)
	if err != nil {
		return false, 0, 0, err
	}
	resp, err := c.ABCIQuery(ctx, pathSlashingInfo, b)
	if err != nil {
		if isMissingModuleErr(err) {
			return false, 0, 0, errNoSlashingModule
		}
		return false, 0, 0, err
	}
	out := &slashingv1beta1.QuerySigningInfoResponse{}
	if err := proto.Unmarshal(resp.Value, out); err != nil {
		return false, 0, 0, err
	}
	info := out.GetValSigningInfo()
	var ju int64
	if t := info.GetJailedUntil(); t != nil {
		ju = t.AsTime().Unix()
	}
	return info.GetTombstoned(), info.GetMissedBlocksCounter(), ju, nil
}

func getSlashingWindow(ctx context.Context, c *rpc.Client) (int64, error) {
	b, err := proto.Marshal(&slashingv1beta1.QueryParamsRequest{})
	if err != nil {
		return 0, err
	}
	resp, err := c.ABCIQuery(ctx, pathSlashingParams, b)
	if err != nil {
		if isMissingModuleErr(err) {
			return 0, errNoSlashingModule
		}
		return 0, err
	}
	out := &slashingv1beta1.QueryParamsResponse{}
	if err := proto.Unmarshal(resp.Value, out); err != nil {
		return 0, err
	}
	return out.GetParams().GetSignedBlocksWindow(), nil
}

func isMissingModuleErr(err error) bool {
	s := err.Error()
	return strings.Contains(s, "unknown query path") ||
		strings.Contains(s, "no route found") ||
		strings.Contains(s, "not implemented") ||
		strings.Contains(s, "unknown request")
}
