package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/abhijitkrm/cometduty/internal/config"
	"github.com/abhijitkrm/cometduty/internal/evm"
	"github.com/abhijitkrm/cometduty/internal/monitor"
	"github.com/abhijitkrm/cometduty/internal/rpc"
)

// doctorCmd inspects a single node: CometBFT identity/health plus the EVM
// execution layer — the "what is this machine and is it sane" report.
func doctorCmd() *cobra.Command {
	var evmURL string
	c := &cobra.Command{
		Use:   "doctor <cometbft-rpc-url>",
		Short: "inspect a node: chain-id, height, peers, EVM layer health",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			failed := false

			fmt.Printf("CometBFT  %s\n", args[0])
			cl, err := rpc.New(args[0], rpc.WithTimeout(10*time.Second))
			if err != nil {
				return fmt.Errorf("bad url: %w", err)
			}
			st, err := cl.Status(ctx)
			if err != nil {
				return fmt.Errorf("status: %w", err)
			}
			cometHeight := int64(st.SyncInfo.LatestBlockHeight)
			fmt.Printf("  chain-id      %s\n", st.NodeInfo.Network)
			fmt.Printf("  height        %d (catching_up: %v)\n", cometHeight, st.SyncInfo.CatchingUp)
			fmt.Printf("  node          %s / %s\n", st.NodeInfo.Moniker, st.NodeInfo.Version)
			if st.SyncInfo.CatchingUp {
				fmt.Println("  ! still catching up")
			}
			if ni, err := cl.NetInfo(ctx); err == nil {
				fmt.Printf("  peers         %d\n", ni.NPeers)
				if ni.NPeers == 0 {
					fmt.Println("  ! zero peers — check p2p connectivity/seeds")
					failed = true
				}
			}

			// EVM side — explicit --evm, or guess :8545 on the same host
			guessed := evmURL == ""
			if guessed {
				if u, perr := url.Parse(args[0]); perr == nil {
					evmURL = fmt.Sprintf("%s://%s:8545", u.Scheme, u.Hostname())
				}
			}
			if evmURL != "" {
				fmt.Printf("\nEVM       %s", evmURL)
				if guessed {
					fmt.Print(" (guessed :8545)")
				}
				fmt.Println()
				ec := evm.New(evmURL, 10*time.Second)
				if ver, err := ec.ClientVersion(ctx); err == nil {
					fmt.Printf("  client        %s\n", ver)
				}
				if cid, err := ec.ChainID(ctx); err == nil {
					fmt.Printf("  evm chain-id  %d\n", cid)
				}
				bn, err := ec.BlockNumber(ctx)
				if err != nil {
					fmt.Printf("  ! unreachable: %s\n", err)
					if !guessed {
						failed = true
					}
				} else {
					diff := cometHeight - bn
					fmt.Printf("  block         %d (Δ comet: %+d)\n", bn, -diff)
					if diff > 10 {
						fmt.Printf("  ! EVM execution %d blocks behind consensus\n", diff)
						failed = true
					}
				}
				if syncing, ss, err := ec.Syncing(ctx); err == nil {
					if syncing && ss != nil {
						fmt.Printf("  ! syncing     %d/%d\n", ss.Current, ss.Highest)
						failed = true
					} else if !syncing {
						fmt.Println("  syncing       false")
					}
				}
				if pc, err := ec.PeerCount(ctx); err == nil {
					fmt.Printf("  evm peers     %d\n", pc)
				}
				if gp, err := ec.GasPrice(ctx); err == nil {
					fmt.Printf("  gas price     %s wei\n", gp.String())
				}
			}
			if failed {
				return errors.New("checks failed")
			}
			return nil
		},
	}
	c.Flags().StringVar(&evmURL, "evm", "", "EVM JSON-RPC URL (default: same host :8545)")
	return c
}

// statusCmd prints a one-shot block-signing grid for the configured
// validators — the terminal equivalent of the old dashboard's recent blocks.
func statusCmd() *cobra.Command {
	var blocks int
	c := &cobra.Command{
		Use:   "status",
		Short: "print a one-shot signing grid for configured validators",
		RunE: func(cmd *cobra.Command, args []string) error {
			if password == "" {
				password = os.Getenv("PASSWORD")
			}
			cfg, err := config.Load(cfgFile, chainsDir, password)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			failures := 0
			for name, cc := range cfg.Chains {
				if err := printChainStatus(ctx, name, cc, blocks); err != nil {
					fmt.Printf("%s: %s\n\n", name, err)
					failures++
				}
			}
			if failures > 0 {
				return fmt.Errorf("%d chain(s) failed", failures)
			}
			return nil
		},
	}
	c.Flags().IntVarP(&blocks, "blocks", "n", 30, "number of recent blocks to show")
	return c
}

func printChainStatus(ctx context.Context, name string, cc *config.ChainConfig, blocks int) error {
	// first healthy endpoint
	var cl *rpc.Client
	var head int64
	for _, n := range cc.Nodes {
		c, st, err := probeNode(ctx, n.URL, cc.ChainID)
		if err == nil {
			cl, head = c, int64(st.SyncInfo.LatestBlockHeight)
			break
		}
	}
	if cl == nil {
		return errors.New("no healthy RPC endpoint")
	}

	// resolve validators -> cons hex
	vals := monitor.ResolveValidators(ctx, cl, cc.ValidatorTargets())
	type val struct {
		monitor.ResolvedValidator
		missed int
	}
	grid := make([]*val, 0, len(vals))
	for _, v := range vals {
		grid = append(grid, &val{ResolvedValidator: v})
	}

	// Commit signature arrays are POSITIONAL — signatures[i] belongs to the
	// i-th member of /validators at that height, and absent entries carry an
	// empty address. Fetch the set at head; if a later block's non-empty sig
	// addresses disagree with the ordering, the set changed — refetch it.
	setPos := map[string]int{}
	setAt := func(height int64) {
		vs, err := cl.Validators(ctx, height)
		if err == nil && len(vs) > 0 {
			for k := range setPos {
				delete(setPos, k)
			}
			for i, v := range vs {
				setPos[strings.ToUpper(v.Address)] = i
			}
		}
	}
	setAt(head)

	type row struct {
		cells []byte
		miss  int
	}
	rows := make([]row, len(grid))
	for i := range rows {
		rows[i].cells = make([]byte, 0, blocks)
	}
	for h := head; h > head-int64(blocks) && h > 0; h-- {
		cm, err := cl.Commit(ctx, h)
		if err != nil {
			for i := range rows {
				rows[i].cells = append(rows[i].cells, '?')
			}
			continue
		}
		sigs := cm.SignedHeader.Commit.Signatures
		// drift check: any non-empty sig address disagreeing with setPos's
		// ordering means the set changed (or we started mid-change)
		for i, sg := range sigs {
			if sg.ValidatorAddress == "" {
				continue
			}
			if pos, ok := setPos[strings.ToUpper(sg.ValidatorAddress)]; !ok || pos != i {
				setAt(h)
				break
			}
		}
		proposer := strings.ToUpper(cm.SignedHeader.Header.ProposerAddress)
		for i, v := range grid {
			if v.ConsHex == "" {
				rows[i].cells = append(rows[i].cells, ' ')
				continue
			}
			pos, inSet := setPos[v.ConsHex]
			mark := byte(' ')
			if inSet && pos < len(sigs) {
				switch sigs[pos].BlockIDFlag {
				case 2:
					mark = '+'
				case 3:
					mark = 'n'
					rows[i].miss++ // nil-vote doesn't count as signed for slashing
				case 1:
					mark = 'x'
					rows[i].miss++
				default:
					mark = 'x'
					rows[i].miss++
				}
				// verify the entry actually belongs to this validator when
				// it carries an address (flag 1 entries don't)
				if a := strings.ToUpper(sigs[pos].ValidatorAddress); a != "" && a != v.ConsHex {
					mark = '?'
				}
			}
			if proposer == v.ConsHex && mark == '+' {
				mark = 'P'
			}
			rows[i].cells = append(rows[i].cells, mark)
		}
	}

	fmt.Printf("%s (%s) — height %d, last %d blocks (newest left)\n", name, cc.ChainID, head, blocks)
	fmt.Println("legend: P proposed  + signed  x missed/absent  n nil-vote  ? no data  ' ' not in set")
	for i, v := range grid {
		label := v.Moniker
		if label == "" {
			label = v.Valcons
		}
		if v.Err != nil {
			fmt.Printf("  %-16s  resolve failed: %s\n", label, v.Err)
			continue
		}
		state := ""
		switch {
		case v.Tombstoned:
			state = " [TOMBSTONED]"
		case v.Jailed:
			state = " [JAILED]"
		case !v.Bonded:
			state = " [unbonded]"
		}
		fmt.Printf("  %-16s  %s  missed %d/%d%s\n", label, string(rows[i].cells), rows[i].miss, len(rows[i].cells), state)
	}
	fmt.Println()
	return nil
}

// probeNode is a thin local wrapper so status doesn't depend on monitor
// internals beyond what's exported.
func probeNode(ctx context.Context, rawURL, wantChainID string) (*rpc.Client, *rpc.Status, error) {
	cl, err := rpc.New(rawURL, rpc.WithTimeout(8*time.Second))
	if err != nil {
		return nil, nil, err
	}
	st, err := cl.Status(ctx)
	if err != nil {
		return nil, nil, err
	}
	if st.NodeInfo.Network != wantChainID {
		return nil, nil, fmt.Errorf("wrong chain %s (want %s)", st.NodeInfo.Network, wantChainID)
	}
	return cl, st, nil
}

// unjailCmd shells out to the chain binary: `evmd tx slashing unjail` needs the
// validator operator key, which lives on the validator host — cometduty only
// wraps the call so operators get the right flags and a pre-flight jail check.
func unjailCmd() *cobra.Command {
	var evmdBin, from, node, chainID, home, keyring string
	c := &cobra.Command{
		Use:   "unjail",
		Short: "unjail a validator via evmd tx slashing unjail",
		Long: `Wraps 'evmd tx slashing unjail' — must run where the validator's
operator key lives (keyring). Warns if the validator is tombstoned, since
unjailing can never succeed there.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if from == "" {
				return errors.New("--from <key> is required (the validator operator key in your keyring)")
			}
			bin := evmdBin
			if bin == "" {
				var err error
				bin, err = exec.LookPath("evmd")
				if err != nil {
					return errors.New("evmd not on PATH — pass --evmd /path/to/evmd")
				}
			}
			argv := []string{"tx", "slashing", "unjail",
				"--from", from,
				"--node", node,
				"--chain-id", chainID,
				"--keyring-backend", keyring,
				"--broadcast-mode", "sync", "-y",
			}
			if home != "" {
				argv = append(argv, "--home", home)
			}
			argv = append(argv, args...) // pass-through extra evmd flags
			fmt.Printf("$ %s %s\n\n", bin, strings.Join(argv, " "))
			ex := exec.CommandContext(cmd.Context(), bin, argv...)
			ex.Stdout, ex.Stderr, ex.Stdin = os.Stdout, os.Stderr, os.Stdin
			return ex.Run()
		},
	}
	c.Flags().StringVar(&evmdBin, "evmd", "", "path to evmd binary (default: PATH lookup)")
	c.Flags().StringVar(&from, "from", "", "validator operator key name in keyring")
	c.Flags().StringVar(&node, "node", "tcp://localhost:26657", "CometBFT RPC for broadcast")
	c.Flags().StringVar(&chainID, "chain-id", "", "chain id (required)")
	c.Flags().StringVar(&home, "home", "", "evmd home dir")
	c.Flags().StringVar(&keyring, "keyring-backend", "test", "keyring backend")
	_ = c.MarkFlagRequired("chain-id")
	return c
}

// debugCmd produces a per-validator diagnostic bundle: on-chain identity,
// slashing state, recent signing pattern, and the health of the node serving
// the data. The answer to "is my validator broken and why".
func debugCmd() *cobra.Command {
	var blocks int
	c := &cobra.Command{
		Use:   "debug <valoper|moniker>",
		Short: "diagnose one validator: identity, jail state, signing pattern",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if password == "" {
				password = os.Getenv("PASSWORD")
			}
			cfg, err := config.Load(cfgFile, chainsDir, password)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			want := args[0]
			for name, cc := range cfg.Chains {
				var cl *rpc.Client
				var head int64
				for _, n := range cc.Nodes {
					c, st, err := probeNode(ctx, n.URL, cc.ChainID)
					if err == nil {
						cl, head = c, int64(st.SyncInfo.LatestBlockHeight)
						break
					}
				}
				if cl == nil {
					fmt.Printf("%s: no healthy RPC endpoint\n", name)
					continue
				}
				vals := monitor.ResolveValidators(ctx, cl, cc.ValidatorTargets())
				for _, v := range vals {
					if !strings.EqualFold(v.Valoper, want) &&
						!strings.EqualFold(v.Moniker, want) &&
						!strings.EqualFold(v.Valcons, want) {
						continue
					}
					fmt.Printf("== %s (%s) ==\n", v.Moniker, name)
					fmt.Printf("valoper    %s\nvalcons    %s\nconsensus  %s\n", v.Valoper, v.Valcons, v.ConsHex)
					switch {
					case v.Err != nil:
						fmt.Printf("ERROR      %s\n", v.Err)
						continue
					}
					fmt.Printf("bonded     %v   jailed %v   tombstoned %v\n", v.Bonded, v.Jailed, v.Tombstoned)
					if v.Window > 0 {
						fmt.Printf("slashing   missed %d / %d window (%.2f%%)\n", v.Missed, v.Window, 100*float64(v.Missed)/float64(v.Window))
					} else {
						fmt.Printf("slashing   missed %d (window unknown)\n", v.Missed)
					}
					// recent signing pattern
					printDebugGrid(ctx, cl, head, v, blocks)
					return nil
				}
			}
			return fmt.Errorf("no validator matching %q in configured chains", want)
		},
	}
	c.Flags().IntVarP(&blocks, "blocks", "n", 30, "recent blocks to inspect")
	return c
}

// printDebugGrid shows the validator's last N blocks with the block's proposer
// and its signature state — same positional-signature logic as `status`.
func printDebugGrid(ctx context.Context, cl *rpc.Client, head int64, v monitor.ResolvedValidator, blocks int) {
	setPos := map[string]int{}
	if vs, err := cl.Validators(ctx, head); err == nil {
		for i, sv := range vs {
			setPos[strings.ToUpper(sv.Address)] = i
		}
	}
	var cells []byte
	miss := 0
	for h := head; h > head-int64(blocks) && h > 0; h-- {
		cm, err := cl.Commit(ctx, h)
		if err != nil {
			cells = append(cells, '?')
			continue
		}
		sigs := cm.SignedHeader.Commit.Signatures
		pos, inSet := setPos[v.ConsHex]
		mark := byte(' ')
		if inSet && pos < len(sigs) {
			switch sigs[pos].BlockIDFlag {
			case 2:
				mark = '+'
			case 3:
				mark = 'n'
				miss++
			case 1:
				mark = 'x'
				miss++
			default:
				mark = 'x'
				miss++
			}
			if a := strings.ToUpper(sigs[pos].ValidatorAddress); a != "" && a != v.ConsHex {
				mark = '?'
			}
			if strings.ToUpper(cm.SignedHeader.Header.ProposerAddress) == v.ConsHex && mark == '+' {
				mark = 'P'
			}
		}
		cells = append(cells, mark)
	}
	fmt.Printf("recent     %s  missed %d/%d\n", string(cells), miss, len(cells))
	fmt.Println("           (P proposed  + signed  x missed  n nil-vote  ? drift  ' ' not in set)")
}

// spinupCmd emits the exact bootstrap sequence for joining a cosmos-evm chain
// as a validator — cometduty can't hold keys, so it generates the runbook.
func spinupCmd() *cobra.Command {
	var chainID, moniker, home, peers, genesis, binary string
	c := &cobra.Command{
		Use:   "spinup",
		Short: "print the validator bootstrap sequence for a cosmos-evm chain",
		RunE: func(cmd *cobra.Command, args []string) error {
			if chainID == "" || moniker == "" {
				return errors.New("--chain-id and --moniker are required")
			}
			if home == "" {
				home = "$HOME/.evmd"
			}
			if binary == "" {
				binary = "evmd"
			}
			fmt.Printf(`# cometduty spinup — %s as %q
# Run each step on the validator host. Stop and check before continuing on error.

# 1. initialize home (creates priv_validator_key.json — BACK IT UP)
%s init %s --chain-id %s --home %s

# 2. fetch the network genesis
`, binary, moniker, binary, shellQuote(moniker), shellQuote(chainID), home)
			if genesis != "" {
				fmt.Printf("curl -sSfL %s -o %s/config/genesis.json\n", shellQuote(genesis), home)
			} else {
				fmt.Printf("#   (place the network genesis.json at %s/config/genesis.json)\n", home)
			}
			if peers != "" {
				fmt.Printf("\n# 3. persistent peers\nsed -i.bak 's/^persistent_peers =.*/persistent_peers = \"%s\"/' %s/config/config.toml\n", peers, home)
			}
			fmt.Printf(`
# 4. create the operator key (or import an existing one)
%s keys add validator --keyring-backend file --home %s

# 5. start the node and wait for catch-up
%s start --home %s --chain-id %s
#    check: curl -s localhost:26657/status | jq .result.sync_info.catching_up

# 6. once synced, self-delegate to join the active set
%s tx staking create-validator \
  --amount 1000000adex \
  --pubkey "$(%s tendermint show-validator --home %s)" \
  --moniker %s \
  --chain-id %s \
  --commission-rate 0.10 --commission-max-rate 0.20 --commission-max-change-rate 0.01 \
  --min-self-delegation 1 \
  --from validator --keyring-backend file --home %s -y

# 7. verify: cometduty doctor http://localhost:26657 --evm http://localhost:8545
#    monitor: add this validator's valoper to cometduty config.yml
`, binary, home,
				binary, home, shellQuote(chainID),
				binary, binary, home, shellQuote(moniker), shellQuote(chainID), home)
			return nil
		},
	}
	c.Flags().StringVar(&chainID, "chain-id", "", "chain id (required)")
	c.Flags().StringVar(&moniker, "moniker", "", "validator moniker (required)")
	c.Flags().StringVar(&home, "home", "", "evmd home dir (default ~/.evmd)")
	c.Flags().StringVar(&binary, "binary", "", "chain binary name (default evmd)")
	c.Flags().StringVar(&genesis, "genesis-url", "", "URL to fetch genesis.json from")
	c.Flags().StringVar(&peers, "peers", "", "persistent_peers list for config.toml")
	return c
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if !strings.ContainsAny(s, " \t'\"$`\\") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}
