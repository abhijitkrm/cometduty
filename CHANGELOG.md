# Changelog

All notable changes to cometduty are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [0.1.0] — 2026-09-19

First tagged release.

Ground-up rewrite of the deprecated
[tenderduty](https://github.com/blockpane/tenderduty) v2, built Cosmos-EVM
first: `ethsecp256k1` consensus keys, EVM execution-layer checks, strict
config validation, and a race-tested suite with a fake CometBFT RPC driving
the monitor end-to-end.

### Monitoring core

- Per-block validator sign classification: **proposed**, **signed**,
  **precommit-only**, **prevote-only**, **missed** — over CometBFT
  websocket blocks + votes, plain JSON-RPC, no server-side Go dependency
- Multi-validator per chain (`validators:` list) plus single-validator
  shorthand and `valcons_override` / `cons_prefix` escapes
- Slashing-window tracking: missed-in-window counts and missed percentage
  against the chain's live `x/slashing` params
- Validator-set awareness: jailed, tombstoned, unbonded, and bonded-state
  transitions detected and reported with the reason
- Consensus internals per node: mempool backlog (`num_unconfirmed_txs`),
  live consensus round (`consensus_state`)
- Endpoint health per configured RPC: down seconds, lag behind best head,
  peer count, wrong-network and catching-up detection
- `ethsecp256k1` family key support (`/cosmos.evm.crypto.v1{,alpha1}`,
  `/ethermint.crypto.v1{,alpha1}`, `/injective.crypto.v1beta1`,
  `/stratos.crypto.v1`, `/dymension.crypto`) alongside `ed25519`,
  `secp256k1`, `secp256r1`; unknown single-key types attempted generically

### EVM execution layer

- Minimal EVM JSON-RPC client: `eth_chainId`, `eth_blockNumber`,
  `eth_syncing`, `net_peerCount`, `eth_gasPrice`, `web3_clientVersion`,
  `txpool_status`, `eth_getBlockByNumber`
- Execution-lag monitoring: consensus height vs `eth_blockNumber` — catches
  the "blocks finalize but nothing executes" outage consensus monitors miss
- EVM endpoint-down detection, sync-state flag, txpool pending/queued
  depth, and block fullness (`gasUsed/gasLimit`)

### Alerts

- **19 alert types**: `consecutive`, `window-pct`, `stalled`, `node-down`,
  `node-lag`, `catching-up`, `inactive` (jailed/tombstoned/unbonded),
  `no-servers`, `evm-down`, `evm-lag`, `evm-txpool`, `mempool-txs`,
  `consensus-round`, `validator-new`, `validator-gone`, `validator-jailed`,
  `stake-change`, `cpu-high`, `mem-high`
- Validator-set watch (`set_watch_enabled`): diffs the full staking set
  every refresh — catches ANY validator joining, leaving, or jailing,
  not just monitored ones
- Host stats: optional per-node `metrics_url` scrapes the node's own
  prometheus endpoint for `cometduty_node_cpu_percent` /
  `cometduty_node_memory_bytes` + `cpu-high`/`mem-high` alerts
- Every raise paired with a resolve — incidents auto-close on PagerDuty /
  Opsgenie, including for alerts restored from a previous run
- Engine: per-destination dedup, flap suppression, periodic still-open
  reminders, per-validator and per-chain destination overrides, latched
  transitions (no repeat-fires between refresh ticks)
- **7 destination types**: PagerDuty, Slack, Discord, Telegram, generic
  templated JSON webhook (Teams/Mattermost/Gotify/…), ntfy, Opsgenie
- Bounded retry (one retry after 2s) on failed sends; stable PagerDuty
  dedup keys
- `cometduty_notify_total{dest,result}` — every delivery attempt counted;
  `result="error"` is the "pager itself is broken" signal
- `--alert-log` — append-only JSONL audit of every raise/resolve/delivery
  (default `.cometduty-alerts.jsonl`, `0600`)
- Open alerts persisted in `--state` across restarts — no double-page, no
  forgotten resolve
- `test-alert` command to verify destinations before deploy

### CLI (daemon surface only)

- `validate` — strict schema check with line numbers; `--live` additionally
  probes nodes (reachability, chain-id, sync state, peers) and resolves
  each valoper to its valcons, reporting bonded/jailed/tombstoned state
- `example-config`, `encrypt`/`decrypt` (age), `version`, `test-alert`
- Interactive node ops (`doctor`, `status`, `unjail`, tx, upgrades) live in
  the sibling [cometcli](https://github.com/abhijitkrm/cometcli) project —
  cometduty stays a pure daemon

### Observability

- `/metrics` — ~30 `cometduty_*` series: signing, slashing window, endpoint
  health, consensus internals, EVM layer, notifier results, build info
- `/healthz` liveness and `/readyz` readiness (≥1 healthy endpoint + recent
  block per chain) on the metrics listener
- Structured `slog` logs; block/alert/resolve/delivery events at INFO
- Grafana dashboard (`deploy/grafana/`) — 12 panels + `validator`
  template variable (multi-select/All) and a fleet-overview table
- Local observability stack (`deploy/grafana/stack/`) — Prometheus scrape
  config (cometduty + CometBFT + EVM exporter + commented node-exporter),
  Grafana provisioning, compose file, native-binary fallback docs

### Configuration

- Strict YAML: unknown keys fail `validate` with line numbers
- `${ENV_VAR}` expansion; whole-file age encryption; `chains.d/` per-chain
  files; per-node `headers`, `insecure_tls`, `disable_vote`;
  `public_fallback` to chain-registry RPCs; `SIGHUP` hot reload
- tenderduty v2 field names where practical —
  [docs/MIGRATING.md](docs/MIGRATING.md) for deltas
- Removed: embedded dashboard/UI — Prometheus + Grafana are the
  visualization layer

### Deployment

- `deploy/k8s/` — single-replica Deployment (Recreate + HA caveat),
  ConfigMap, PVC for state + alert log, Prometheus ServiceMonitor,
  `/readyz` readiness probe
- `deploy/docker-compose.yml`, `deploy/cometduty.service`
- Multi-arch container image (`ghcr.io/abhijitkrm/cometduty`), distroless
  nonroot; GoReleaser pipeline building darwin/linux amd64+arm64 binaries,
  archives, checksums, and images
- `docs/RUNBOOK.md` — full alert catalog with per-alert response guidance
