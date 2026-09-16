# cometduty

A validator monitoring and alerting daemon for **CometBFT** chains — including
Cosmos SDK chains, Cosmos-EVM chains, and legacy Tendermint networks.

cometduty is a ground-up rewrite of the (now-deprecated)
[tenderduty](https://github.com/blockpane/tenderduty) v2 monitor. It keeps the
parts that worked — the missed-block grid dashboard, multi-destination
alerting, per-chain node health tracking — and rebuilds the rest on modern
dependencies with a test suite and no global state.

## What it watches

- **Missed blocks** — consecutive-miss and slashing-window-percentage alarms,
  with prevote/precommit visibility so you can tell "never voted" from
  "prevoted but the commit didn't land".
- **Proposals** — block proposals are tracked distinctly from plain signatures.
- **Node health** — per-endpoint up/down, catching-up, peer count, and
  **block lag** (alerts when a configured RPC falls `lag_blocks` behind head).
- **Chain stalls** — no new block for `stalled_minutes`.
- **Validator transitions** — jailed, tombstoned, unbonded.
- **Network participation** — the fraction of the validator set that signed
  each block (exported to Prometheus; an early warning below ~2/3).
- **All endpoints down** — when every configured RPC is unreachable, and
  optional fallback to chain-registry public endpoints.

## Compatibility

- CometBFT v0.34 → v1.x and legacy Tendermint RPC (the client speaks plain
  JSON-RPC + `/websocket` subscriptions — no server-side Go version coupling).
- Cosmos SDK staking/slashing modules; gracefully degrades on chains without
  `x/slashing` (e.g. beacon-kit chains) — window/tombstone metrics are skipped.
- Consensus key types: `ed25519`, `secp256k1`, `secp256r1`, and the
  `ethsecp256k1` family used by Cosmos-EVM / Ethermint / Injective / Stratos /
  Dymension (`/cosmos.evm.crypto.v1.ethsecp256k1.PubKey` etc.). Unknown
  single-key types are attempted generically.

## Alert destinations

PagerDuty (v2 Events API), Slack, Discord, Telegram, a **generic templated
JSON webhook** (covers Teams, Mattermost, Gotify, Home Assistant…), **ntfy**,
and **Opsgenie**. Alerts deduplicate per destination, pair trigger/resolve
correctly, support flap suppression, reminders for still-open alerts, and
survive restarts via the state file. `cometduty test-alert` verifies wiring
before you rely on it.

## Quick start

```sh
go install github.com/abhijitkrm/cometduty/cmd/cometduty@latest

cometduty example-config > config.yml
$EDITOR config.yml
cometduty validate -f config.yml
cometduty test-alert webhook -f config.yml   # optional: verify a destination
cometduty -f config.yml                      # run (or just `cometduty`)
```

Docker (multi-arch images are published to `ghcr.io/abhijitkrm/cometduty`):

```sh
docker run -v $PWD/config.yml:/config/config.yml:ro -v cd-data:/data \
  -p 8888:8888 ghcr.io/abhijitkrm/cometduty:latest
```

The dashboard listens on `:8888` by default; Prometheus metrics on `:28686/metrics`.

## Configuration

Field names are compatible with tenderduty v2 where practical — see
[docs/MIGRATING.md](docs/MIGRATING.md). Highlights over v2:

- `validators:` list per chain (multi-validator monitoring on one chain)
- `alerts.lag_enabled` / `lag_blocks`, `alerts.stalled_*`, per-validator alert overrides
- `webhook:`, `ntfy:`, `opsgenie:` destinations, `mentions:` actually delivered
- `dashboard_bind`, `dashboard_user`/`dashboard_pass` basic auth, `prometheus_bind`
- `${ENV_VAR}` expansion and `chains.d/` per-chain config files
- `node.headers` for authenticated RPC endpoints, `node.insecure_tls`,
  `node.disable_vote` for endpoints that shouldn't carry subscriptions
- `public_fallback` to use chain-registry RPCs when all your nodes fail

Secrets can live in the environment (`${VAR}` expansion) or the whole config
file can be age-encrypted: `cometduty encrypt` / `--password` / `PASSWORD`.

Hot reload: send `SIGHUP` to re-read the config without restarting.

## Commands

```
cometduty                  run the monitor (default)
cometduty validate         check config and exit non-zero on fatal problems
cometduty example-config   print annotated config.yml
cometduty test-alert [kind]  send a test notification
cometduty encrypt          age-encrypt config.yml → config.yml.age
cometduty decrypt          decrypt config.yml.age
cometduty version          print version
```

## Metrics

Besides the tenderduty v2 parity metrics, cometduty exports:

- `cometduty_endpoint_lag_blocks` — per-endpoint blocks behind head
- `cometduty_block_signature_ratio` — share of the set that signed each block
- `cometduty_window_missed_percent` — missed % of the slashing window
- `cometduty_total_{monitored,unhealthy}_endpoints` / `cometduty_endpoint_peers` — endpoint health/peers
- `cometduty_active_alerts` — currently-open alert count

## Architecture

```
cmd/cometduty        cobra CLI
internal/config      v3 YAML schema, env expansion, chains.d, age, validation
internal/rpc         minimal JSON-RPC + websocket client (no CometBFT dep)
internal/monitor     per-chain supervisor: sign machine, health, val info
internal/alert       dedup/resolve/flap/reminder engine + notifiers
internal/state       atomic JSON persistence (alarms, block rings, node downs)
internal/metrics     prometheus exporter
internal/dashboard   embedded UI, ws fan-out, optional basic auth, /healthz
internal/consensus   pubkey-any → consensus address (ed25519/secp256k1/eth…)
internal/bech32      self-contained BIP-0173 codec
internal/protowire   minimal protobuf wire reader
```

## Status

Early v3 line — interfaces are still settling; pin a release rather than
`@latest` if you need stability. Bug reports and PRs welcome; see
[CONTRIBUTING.md](CONTRIBUTING.md).
