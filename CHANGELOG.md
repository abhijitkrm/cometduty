# Changelog

All notable changes to cometduty are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

### Fixed

- Loading a config path that is a directory now fails with an actionable
  error instead of a bare `read ...: is a directory` — the usual cause is a
  docker bind mount whose host file didn't exist. A missing config file now
  points at `cometduty example-config`.
- Docker images could not write their state dir: every image ran as a
  non-root user but `/data` was root-owned (the release image never created
  it; the source image created it for the wrong uid, 65534, and `COPY --from`
  dropped the ownership anyway). State saves and the alert log failed with
  `permission denied`, losing dedup memory on restart. `/data` now ships
  owned by uid 65532 in all images (the local alpine image moves from 65534
  to 65532 to match, so a volume works across images). Existing root-owned
  volumes need a one-time `chown` — see `docs/INSTALL.md`
- The source-built image now writes the alert log (`--alert-log`) like the
  release image
- systemd unit: `StateDirectory=` creates `/var/lib/cometduty` owned by the
  service user, and the alert log is enabled
- `cometduty_time_since_last_block_unfinalized` was registered but never
  set (its `Tick` updater had no caller), leaving the dashboard's *Seconds
  Since Last Block* panel empty. It's now updated every 2s from the watch
  loop, so it keeps climbing through a stall or a websocket reconnect
- Grafana stack: bundled `node-exporter` service (pinned v1.8.2), so the
  *Host* row has data without installing anything on the host
- Dashboard *Host Memory Used %* was always empty on Linux (divided by the
  macOS-only `node_memory_total_bytes`; Linux is `node_memory_MemTotal_bytes`)
- Dashboard *Host CPU Used %* averaged every host into one line; now one per
  instance
- Dashboard *Host Disk Used %* only watched `/`, which misses a separate
  chain-data disk (and is a read-only image on Docker Desktop); it now plots
  every real disk per device
- Grafana stack: pin the Infinity plugin to 3.7.1 — the unpinned install
  pulled 4.x, which needs Grafana ≥ 11.6.11 and failed to load on the
  shipped 11.4 (`404 … react/jsx-runtime`)
- Grafana stack: Prometheus published on host port 9091 instead of 9090,
  which collides with a Cosmos node's gRPC port on the same host; every
  host port is now overridable (`GRAFANA_PORT`, `PROMETHEUS_PORT`,
  `LOKI_PORT`, `ALLOY_PORT`)
- Grafana stack: `extra_hosts: host-gateway` so `host.docker.internal`
  resolves on Linux engines, not just Docker Desktop
- Grafana stack: `prometheus.yml` defaults to a generic single-instance
  layout (cometduty `:28686`, node `:26660` / `:8100`) that works out of the
  box; the old defaults (`27660`/`10100` node ports) matched no published
  setup. Multi-node and sidecar targets are sketched in comments

### Documentation

- `docs/INSTALL.md`: systemd service setup (dedicated user, config
  permissions) and non-root / volume-ownership notes for docker
- Grafana stack README: quick start with target selection and a verify
  step, a table of panels that are empty by design (and what fills them),
  and troubleshooting for the common failure modes

- README, `docs/INSTALL.md` and `deploy/docker-compose.yml` now say to create
  `config.yml` before starting the container, and show how to generate and
  validate it using the image alone; runbook lists the failure signature.

## [0.2.0] — 2026-09-25

Sidecar deployment mode, catching-up alert suite, per-node log alerting,
and a Loki + Alloy log pipeline — tested live against a 4-validator
Cosmos-EVM localnet.

### Sidecar deployment

- **Node↔validator linkage + per-node EVM** — optional `validator:` on each
  node attaches its moniker to all node/EVM/log/host series; per-node
  `evm_rpc` gives every validator its own execution-layer probe
- **`role:` field** — `standalone` (default, unchanged), `sidecar`
  (node-local + monitored-validator alerts), `chain` (network-wide alerts:
  stalled, no-servers, set-watch) — multiple instances on one network no
  longer duplicate pages
- **Local checks** (`role=sidecar`, node `home:` read-only mount): disk free
  (`disk-low`), `priv_validator_state.json` readability (`signer-state`),
  signer stall (`signer-stalled`), signer height regression
  (`signer-regressed` — double-sign tripwire)
- `deploy/sidecar/` — example compose overlay for a 4-validator localnet
  (container-DNS mode), sidecar + chain configs, env template
- Sidecar/chain templates take **full-URL envs** (`NODE_RPC`/`NODE_EVM`/
  `NODE_METRICS`) — the same config works attached to container DNS or
  against host-published ports (native/development runs)

### Catching-up

- `catching_up_enabled` / `catching_up_severity` / `catching_up_grace_minutes`
  — configurable instead of an always-on warning
- **`node-down` suppressed while syncing** — one problem, one page
- `catching-up-stuck` alert — syncing but height unmoved for
  `catching_up_stuck_minutes`
- `cometduty_endpoint_catching_up`, `cometduty_endpoint_sync_blocks_behind`,
  `cometduty_endpoint_sync_rate_blocks_per_sec` — sync progress + ETA

### Log alerting

- Per-node `logs:` watcher — `docker` source (container logs via the Docker
  API; point it at a read-only socket proxy) and `file` source (tail a shared
  volume or host path). CometBFT/EVM log level parsed for both JSON and
  console formats; docker stream headers demuxed correctly
- Pattern-rule engine: `match` regex, `count`/`within` thresholds,
  `cooldown`, per-rule severity; matches auto-resolve after cooldown with the
  offending line attached (truncated, no secrets)
- Built-in rule pack: `panic`, `consensus-failure`, `apphash-mismatch`,
  `upgrade-halt`, `double-sign-guard`, `signer-error`, `disk-full`,
  `fd-exhaustion`, `peer-churn`, `evm-errors` — extendable in config
- `cometduty_log_lines_total{level}` / `cometduty_log_matches_total{rule,severity}`
- ANSI escape sequences stripped before matching — console-colored logs
  (`\x1b[32mINF\x1b[0m`, colored `key=value` pairs) parse and match
  correctly; short console levels (`INF`/`ERR`/`DBG`/`WRN`) recognized

### Logs in Grafana

- Loki + Grafana Alloy join the stack — Alloy tails docker logs over a
  read-only socket mount into Loki (`{job="docker-containers",
  container="<name>"}`, optional validator regex filter); 7d retention
- `loki` datasource provisioned; dashboard gained a Logs row: lines by
  level, rule matches, per-container log stream, error lines

### Metrics

- `cometduty_validator_voting_power` / `cometduty_validator_proposer_priority`
  — consensus set weight and propose likelihood via `/validators`, matched to
  monitored validators by consensus address
- `cometduty_validator_jailed` / `cometduty_validator_tombstoned` /
  `cometduty_validator_bonded` / `cometduty_validator_bonded_tokens` /
  `cometduty_slashing_jailed_until_seconds` — staking + slashing state per
  monitored validator (`jailed_until` = when unjail becomes possible)
- `cometduty_node_info{moniker,version,network}` — per-endpoint identity
  info metric; version skew across the fleet is one query away
- `cometduty_evm_gas_price` — `eth_gasPrice` in wei
- New `Validators` RPC client method (paged `/validators` fetch)

### Grafana

- 15 new dashboard panels: precommit stake %, consensus vote anomalies
  (missing/byzantine/dup/late), block interval, CometBFT peers, validator
  voting power / set state / proposer priority, node CPU+memory, EVM gas
  price, go goroutines — all from already-scraped `:26660` series plus the
  new cometduty metrics
- Host row (disk/cpu/mem/load) fed by `node_exporter` — the compose stack's
  `node-exporter` job is now live against `host.docker.internal:9101`
- Infinity datasource (`yesoreyeram-infinity-datasource`) provisioned in the
  stack + four direct-RPC table panels: peers, validator set, node identity,
  EVM txpool (POST `txpool_status`). Panels ship with `localhost` URLs —
  edit per deployment. Explicit `url_options.method` on all queries —
  compatible with Infinity 3.x (Grafana ≤11.5) and 4.x

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
- `scripts/install.sh` — checksum-verified release installer;
  `docs/INSTALL.md` covering script, binaries, `go install`, docker
- Multi-arch container image (`ghcr.io/abhijitkrm/cometduty`), distroless
  nonroot; GoReleaser pipeline building darwin/linux amd64+arm64 binaries,
  archives, checksums, and images
- `docs/RUNBOOK.md` — full alert catalog with per-alert response guidance
