<div align="center">
  <img src="cometduty.jpg" alt="cometduty" width="1200" />
  <h1>cometduty</h1>
  <p><b>Monitoring and alerting for Cosmos-EVM validators</b></p>
  <p>Know the moment your validator misses a block — before the chain jails it.</p>
</div>

<div align="center">
  <a href="https://github.com/abhijitkrm/cometduty/actions/workflows/ci.yml">
    <img alt="CI" src="https://github.com/abhijitkrm/cometduty/actions/workflows/ci.yml/badge.svg" />
  </a>
  <a href="https://github.com/abhijitkrm/cometduty/releases">
    <img alt="Release" src="https://img.shields.io/github/v/release/abhijitkrm/cometduty" />
  </a>
  <a href="https://github.com/abhijitkrm/cometduty/blob/main/LICENSE">
    <img alt="License" src="https://img.shields.io/github/license/abhijitkrm/cometduty.svg" />
  </a>
  <a href="https://goreportcard.com/report/github.com/abhijitkrm/cometduty">
    <img alt="Go Report Card" src="https://goreportcard.com/badge/github.com/abhijitkrm/cometduty" />
  </a>
  <a href="https://pkg.go.dev/github.com/abhijitkrm/cometduty">
    <img alt="Go Reference" src="https://pkg.go.dev/badge/github.com/abhijitkrm/cometduty.svg" />
  </a>
</div>

<br/>

cometduty is an open-source monitoring daemon and operations CLI for
validators on [cosmos/evm](https://github.com/cosmos/evm) chains and other
CometBFT networks. It watches consensus participation in real time and pages
you on the failures that cost rewards or your validator's seat: missed
blocks, downtime, jailing, chain stalls, wedged execution, and unhealthy RPC
endpoints.

- **Watch** — per-block sign classification (proposed / signed / prevote-only /
  missed), consecutive-miss tracking, slashing-window percentage, jailed and
  tombstoned detection
- **Alert** — 19 alert types to PagerDuty, Slack, Discord, Telegram, webhook,
  ntfy, or Opsgenie; dedup, flap suppression, auto-resolve, durable JSONL audit
- **Observe** — Prometheus metrics for signing, endpoints, consensus internals
  (mempool, round), and the EVM layer (lag, txpool, gas); `/healthz` +
  `/readyz`; provisioned Grafana dashboard
- **Verify** — `validate --live` probes every configured node and resolves
  each valoper before you deploy; `test-alert` proves the pager works

It is a ground-up rewrite of the deprecated
[tenderduty](https://github.com/blockpane/tenderduty) v2, built for
Cosmos-EVM first: `ethsecp256k1` consensus keys, EVM execution-layer checks,
and a dependency-light codebase with a real test suite. Field names stay
compatible where practical — see [docs/MIGRATING.md](docs/MIGRATING.md).

## How it works

```
CometBFT RPC (websocket + polling)
        │
        ▼
┌──────────────────┐    ┌──────────────┐    ┌─────────────────────────┐
│  block watcher   │───▶│ sign machine │───▶│ alert engine            │
│  NewBlock + Vote │    │ per validator│    │ dedup · resolve · flap  │
└──────────────────┘    └──────┬───────┘    └──────────┬──────────────┘
                               ▼                       ▼
                     Prometheus metrics        Slack · PagerDuty ·
                                               Discord · Telegram ·
                                               webhook · ntfy · Opsgenie
```

For every block, cometduty classifies your validator as **proposed**,
**signed**, **precommit-only**, **prevote-only**, or **missed** — exported to
Prometheus so you can graph signing streaks in Grafana or your stack of
choice.

## Install

```sh
# script — verifies checksums, picks your os/arch
curl -fsSL https://raw.githubusercontent.com/abhijitkrm/cometduty/main/scripts/install.sh | sh

# from source (Go 1.26+)
go install github.com/abhijitkrm/cometduty/cmd/cometduty@latest

# or docker (multi-arch images on ghcr)
docker run -v $PWD/config.yml:/config/config.yml:ro -v cd-data:/data \
  -p 28686:28686 ghcr.io/abhijitkrm/cometduty:latest
```

Full guide — binaries, verification, docker, building from source:
[docs/INSTALL.md](docs/INSTALL.md).

Behind a broken registry proxy? `deploy/Dockerfile.local` packages a
natively built static binary on alpine — no base-image pull needed:

```sh
CGO_ENABLED=0 go build -o cometduty ./cmd/cometduty
docker build -f deploy/Dockerfile.local -t cometduty:local .
```

## Quick start

```sh
cometduty example-config > config.yml   # annotated reference config
```

A minimal working config:

```yaml
chains:
  MyEvmChain:
    chain_id: my-evm-1
    validators:
      - valoper_address: cosmosvaloper1...   # your operator address
    alerts:
      consecutive_enabled: yes
      consecutive_missed: 5       # page me after 5 missed blocks in a row
      percentage_enabled: yes
      percentage_missed: 5        # and at >5% of the slashing window
      stalled_enabled: yes
      stalled_minutes: 5          # no new block for 5 min = chain stalled
      alert_if_inactive: yes      # jailed / tombstoned / unbonded
      alert_if_no_servers: yes    # all RPC endpoints unreachable
    nodes:
      - url: http://localhost:26657
        alert_if_down: yes
    discord:
      enabled: yes
      webhook: ${DISCORD_WEBHOOK_URL}
```

Then:

```sh
cometduty validate -f config.yml             # schema + key checks
cometduty validate --live -f config.yml      # also probe nodes, resolve valopers
cometduty test-alert discord -f config.yml   # verify your pager works
cometduty -f config.yml                      # run
```

Prometheus metrics on `:28686/metrics`, plus `/healthz` and `/readyz` for
probes.

## What it alerts on

| Alert | Trigger | Typical cause |
|---|---|---|
| `consecutive` | N missed blocks in a row | sig-relay down, key mismatch, host crash |
| `window-pct` | missed > X% of the slashing window | creeping toward the jail threshold |
| `stalled` | no new block for `stalled_minutes` | consensus halt, upgrade bug |
| `node-down` | configured RPC unreachable `node_down_alert_minutes` | dead sentry, bad LB rule |
| `lag` | endpoint falls `lag_blocks` behind head | catching up, partitioned peer |
| `inactive` | validator jailed / tombstoned / unbonded | slashing event, unbonding |
| `no-servers` | every configured RPC is down | total monitoring blindness |
| `evm-down` | configured `evm_rpc` unreachable | dead execution-layer RPC |
| `evm-lag` | EVM `eth_blockNumber` `evm_lag_blocks`+ behind consensus | execution wedged — blocks finalize but no txs run |
| `mempool-txs` | node mempool backlog > `mempool_txs_alert` (0 = metric only) | CheckTx/execution wedge, tx flood |
| `consensus-round` | node consensus round > `consensus_round_alert` (0 = metric only) | leader churn — proposals timing out |
| `evm-txpool` | EVM txpool queued > `evm_txpool_queued_alert` (0 = metric only) | nonce gap or execution stall |
| `catching-up` | configured node is syncing (`catching_up`) | restart after outage, state-sync, partition healing |
| `validator-new` | any validator joins the set (`set_watch_enabled`) | new operator onboarded, post-upgrade set rotation |
| `validator-gone` | any validator leaves the set (`set_watch_enabled`) | unbond, jail eviction, key rotation |
| `validator-jailed` | any validator jails (`set_watch_enabled`) | downtime/double-sign slash — even unmonitored validators |
| `stake-change` | monitored validator's bonded stake moves > `stake_change_pct`% | big delegation/undelegation, slash event |
| `cpu-high` | node CPU > `cpu_pct_alert`% (needs node `metrics_url`) | load spike, runaway process, under-provisioned host |
| `mem-high` | node RSS > `mem_bytes_alert` (needs node `metrics_url`) | leak, state growth, OOM runway |

Alerts carry validator moniker, chain id, and the condition. Every raise is
paired with a resolve when the condition clears, so PagerDuty/Opsgenie
incidents auto-close — even across restarts, since open alerts persist in
`--state`.

## Alert destinations

PagerDuty · Slack · Discord · Telegram · generic templated JSON **webhook**
(Teams, Mattermost, Gotify, Home Assistant…) · **ntfy** · **Opsgenie**.

The engine handles dedup per destination, flap suppression, periodic
reminders for still-open alerts, per-validator destination overrides, and one
bounded retry on failed sends. Every delivery attempt is counted in
`cometduty_notify_total{dest,result}` and appended to a JSONL audit log
(`--alert-log`) — the pager failing is itself observable.

## Metrics

`cometduty_` prefixed, scrape `:28686/metrics`:

- `cometduty_signed_blocks` / `cometduty_missed_blocks` / `cometduty_proposed_blocks` — per validator
- `cometduty_consecutive_missed_blocks`, `cometduty_missed_blocks_for_window`, `cometduty_window_missed_percent`
- `cometduty_block_signature_ratio` — share of the whole set that signed the last block (network-wide early warning; <⅔ means trouble)
- `cometduty_endpoint_lag_blocks`, `cometduty_endpoint_down_seconds`, `cometduty_endpoint_peers` — per-RPC health
- `cometduty_evm_block_height`, `cometduty_evm_lag_blocks`, `cometduty_evm_endpoint_down_seconds`, `cometduty_evm_syncing` — execution layer (when `evm_rpc` is set)
- `cometduty_evm_txpool_pending`, `cometduty_evm_txpool_queued`, `cometduty_evm_gas_used_ratio` — EVM internals via `txpool_status` / `eth_getBlockByNumber`
- `cometduty_mempool_txs`, `cometduty_mempool_txs_bytes`, `cometduty_consensus_round` — per-node consensus internals via `num_unconfirmed_txs` / `consensus_state`
- `cometduty_node_cpu_percent`, `cometduty_node_memory_bytes` — host stats scraped off each node's own prometheus endpoint (per-node `metrics_url`)
- `cometduty_notify_total{dest,result}` — notifier delivery attempts; alert on `result="error"`
- `cometduty_last_block_height`, `cometduty_time_since_last_block`, `cometduty_active_alerts`, `cometduty_total_(un)healthy_endpoints`

Endpoints on the same listener: `/healthz` (process liveness), `/readyz`
(≥1 healthy endpoint and a fresh block per chain — k8s readiness). A
provisioned multi-validator Grafana dashboard lives in `deploy/grafana/`,
with a runnable Prometheus+Grafana stack in `deploy/grafana/stack/`.

## Command surface

```
cometduty                      run the monitor (default command)
cometduty validate [--live]    check config; --live also probes nodes and
                               resolves validators against the network
cometduty example-config       print the annotated reference config
cometduty test-alert [kind]    fire a test notification at one/all destinations
cometduty encrypt | decrypt    age-encrypt/decrypt the config file
cometduty version              build info
```

cometduty is a daemon, not a toolbox — interactive node ops (`doctor`,
`status`, `unjail`, tx, upgrades) live in the sibling
[cometcli](https://github.com/abhijitkrm/cometcli) project.

Global flags: `-f/--config` (file or https:// URL), `--chains-dir`,
`--state`, `--alert-log` (JSONL delivery audit log, empty disables),
`-v` (debug logging).

## Configuration notes

- Field names match tenderduty v2 where practical — see
  [docs/MIGRATING.md](docs/MIGRATING.md) for the deltas.
- New in v3: `validators:` list (multi-validator per chain), lag alerts,
  per-validator alert overrides, `webhook`/`ntfy`/`opsgenie`, `${ENV_VAR}`
  expansion, `chains.d/` per-chain files, per-node `headers`/`insecure_tls`/
  `disable_vote`, `public_fallback` to chain-registry RPCs.
- Removed in v3: the embedded dashboard — Prometheus metrics and structured
  logs are the observability surface now.
- YAML is parsed **strictly**: a misspelled key fails `validate` with the
  line number instead of silently disabling a feature.
- Secrets: `${VAR}` expansion or whole-file age encryption
  (`cometduty encrypt`, `--password`/`PASSWORD`).
- Hot reload: `SIGHUP` re-reads the config in place.

## Operations model

- **Run exactly one active instance** — two monitors against the same
  validators means double pages. Active/standby is fine; the k8s manifest
  uses `Recreate` + a single replica and explains why.
- **State file** (`--state`) — open alerts persist across restarts so a
  reboot neither double-pages nor forgets to resolve.
- **Alert log** (`--alert-log`) — append-only JSONL audit of every raise,
  resolve, and delivery attempt, written `0600`.
- **`/readyz`** for k8s readiness — only reports ready when at least one
  endpoint is healthy and a recent block has been observed.

See [docs/RUNBOOK.md](docs/RUNBOOK.md) for the full alert catalog and ops
guide, and `deploy/k8s/` for Kubernetes manifests.

## Compatibility

| Layer | Support |
|---|---|
| Consensus | CometBFT v0.34 → v1.x and legacy Tendermint — plain JSON-RPC + `/websocket`, no server-side Go dependency. Verified live on cosmos/evm (HotStuff-derived) testnets. |
| Validator keys | `ed25519`, `secp256k1`, `secp256r1`, and the `ethsecp256k1` family: `/cosmos.evm.crypto.v1{,alpha1}`, `/ethermint.crypto.v1{,alpha1}`, `/injective.crypto.v1beta1`, `/stratos.crypto.v1`, `/dymension.crypto`. Unknown single-key types are attempted generically. |
| Chain modules | `x/staking` + `x/slashing` for validator/window data. Chains without slashing (e.g. beacon-kit) degrade gracefully — block signing and node health still monitored. |
| Go | 1.26+ (see `go.mod`) |

## Documentation

- [docs/RUNBOOK.md](docs/RUNBOOK.md) — alert catalog, per-alert response, ops guide
- [docs/MIGRATING.md](docs/MIGRATING.md) — tenderduty v2 config deltas
- [deploy/grafana/](deploy/grafana/) — dashboard JSON + local observability stack
- [deploy/k8s/](deploy/k8s/) — Kubernetes deployment manifests
- [CONTRIBUTING.md](CONTRIBUTING.md) · MIT licensed

## Development

```sh
go build ./... && go test -race ./...   # full suite, race-enabled
gofmt -l . && go vet ./...
```

The test suite includes a fake CometBFT RPC + websocket server that drives
the monitor end-to-end (block classification, alerts, metrics) without a
live chain.

```
cmd/cometduty        cobra CLI
internal/config      v3 YAML schema, env expansion, chains.d, age, validation
internal/rpc         minimal JSON-RPC + websocket client
internal/monitor     per-chain supervisor: sign machine, health, val info
internal/evm         minimal EVM JSON-RPC client
internal/alert       dedup/resolve/flap/reminder engine + notifiers
internal/history     JSONL alert audit log
internal/state       atomic JSON persistence
internal/metrics     prometheus exporter
internal/consensus   pubkey-any → consensus address derivation
internal/bech32      self-contained BIP-0173 codec
internal/protowire   minimal protobuf wire reader
```

## Status & contributing

v0.1.0 — first tagged release; interfaces are settling but the core is
live-tested. Bug reports, feature requests, and PRs welcome —
see [CONTRIBUTING.md](CONTRIBUTING.md).
