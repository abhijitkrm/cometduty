# cometduty

**Know the moment your Cosmos-EVM validator misses a block — before the chain
jails it.**

cometduty is an open-source monitoring and alerting daemon for validators on
[cosmos/evm](https://github.com/cosmos/evm) chains and other CometBFT networks.
It watches your validator's consensus participation in real time and pages you
on the failures that cost you rewards or your validator's seat: missed blocks,
downtime, jailing, chain stalls, and unhealthy RPC endpoints.

It is a ground-up rewrite of the deprecated
[tenderduty](https://github.com/blockpane/tenderduty) v2, built for
Cosmos-EVM first: `ethsecp256k1` consensus keys, EVM-aware operations, and a
dependency-light codebase with a real test suite.

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
                     dashboard + Prometheus      Slack · PagerDuty ·
                     (block grid, health)        Discord · Telegram ·
                                               webhook · ntfy · Opsgenie
```

For every block, cometduty classifies your validator as **proposed**, **signed**
(commit included), **precommit-only**, **prevote-only**, or **missed** — the
same states the dashboard grid renders, oldest-to-newest at a glance.

## Quick start

```sh
go install github.com/abhijitkrm/cometduty/cmd/cometduty@latest

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
cometduty validate -f config.yml        # checks schema, connectivity, keys
cometduty test-alert discord -f config.yml   # verify your pager works
cometduty -f config.yml                 # run
```

Dashboard on `:8888`, Prometheus metrics on `:28686/metrics`.

Docker (multi-arch images on `ghcr.io/abhijitkrm/cometduty`):

```sh
docker run -v $PWD/config.yml:/config/config.yml:ro -v cd-data:/data \
  -p 8888:8888 -p 28686:28686 ghcr.io/abhijitkrm/cometduty:latest
```

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

Alerts carry validator moniker, chain id, and the condition. Every raise is
paired with a resolve when the condition clears, so PagerDuty/Opsgenie
incidents auto-close.

## Alert destinations

PagerDuty · Slack · Discord · Telegram · generic templated JSON **webhook**
(Teams, Mattermost, Gotify, Home Assistant…) · **ntfy** · **Opsgenie**.

The engine handles dedup per destination, flap suppression, periodic
reminders for still-open alerts, per-validator destination overrides, and
persists open alerts across restarts (`--state`, so a reboot doesn't
double-page you or forget to resolve).

## Metrics

`cometduty_` prefixed, scrape `:28686/metrics`:

- `cometduty_signed_blocks` / `cometduty_missed_blocks` / `cometduty_proposed_blocks` — per validator
- `cometduty_consecutive_missed_blocks`, `cometduty_missed_blocks_for_window`, `cometduty_window_missed_percent`
- `cometduty_block_signature_ratio` — share of the whole set that signed the last block (network-wide early warning; <⅔ means trouble)
- `cometduty_endpoint_lag_blocks`, `cometduty_endpoint_down_seconds`, `cometduty_endpoint_peers` — per-RPC health
- `cometduty_last_block_height`, `cometduty_time_since_last_block`, `cometduty_active_alerts`

## Configuration notes

- Field names match tenderduty v2 where practical — see
  [docs/MIGRATING.md](docs/MIGRATING.md) for the deltas.
- New in v3: `validators:` list (multi-validator per chain), lag alerts,
  per-validator alert overrides, `webhook`/`ntfy`/`opsgenie`, dashboard basic
  auth + bind address, `${ENV_VAR}` expansion, `chains.d/` per-chain files,
  per-node `headers`/`insecure_tls`/`disable_vote`, `public_fallback` to
  chain-registry RPCs.
- YAML is parsed **strictly**: a misspelled key fails `validate` with the
  line number instead of silently disabling a feature.
- Secrets: `${VAR}` expansion or whole-file age encryption
  (`cometduty encrypt`, `--password`/`PASSWORD`).
- Hot reload: `SIGHUP` re-reads the config in place.

## Commands

```
cometduty                      run the monitor (default command)
cometduty validate             check config, exit non-zero on problems
cometduty example-config       print the annotated reference config
cometduty test-alert [kind]    fire a test notification at one/all destinations
cometduty encrypt | decrypt    age-encrypt/decrypt the config file
cometduty version              build info
```

Global flags: `-f/--config` (file or https:// URL), `--chains-dir`,
`--state`, `-v` (debug logging).

## Compatibility

| Layer | Support |
|---|---|
| Consensus | CometBFT v0.34 → v1.x and legacy Tendermint — plain JSON-RPC + `/websocket`, no server-side Go dependency. Verified live on cosmos/evm (HotStuff-derived) testnets. |
| Validator keys | `ed25519`, `secp256k1`, `secp256r1`, and the `ethsecp256k1` family: `/cosmos.evm.crypto.v1{,alpha1}`, `/ethermint.crypto.v1{,alpha1}`, `/injective.crypto.v1beta1`, `/stratos.crypto.v1`, `/dymension.crypto`. Unknown single-key types are attempted generically. |
| Chain modules | `x/staking` + `x/slashing` for validator/window data. Chains without slashing (e.g. beacon-kit) degrade gracefully — block signing and node health still monitored. |
| Go | 1.26+ (see `go.mod`) |

## Development

```sh
go build ./... && go test -race ./...   # full suite, race-enabled
gofmt -l . && go vet ./...
```

The test suite includes a fake CometBFT RPC + websocket server that drives the
monitor end-to-end (block classification, alerts, dashboard state) without a
live chain.

```
cmd/cometduty        cobra CLI
internal/config      v3 YAML schema, env expansion, chains.d, age, validation
internal/rpc         minimal JSON-RPC + websocket client
internal/monitor     per-chain supervisor: sign machine, health, val info
internal/alert       dedup/resolve/flap/reminder engine + notifiers
internal/state       atomic JSON persistence
internal/metrics     prometheus exporter
internal/dashboard   embedded UI, ws fan-out, /healthz, optional basic auth
internal/consensus   pubkey-any → consensus address derivation
internal/bech32      self-contained BIP-0173 codec
internal/protowire   minimal protobuf wire reader
```

## Status & contributing

Early v3 — interfaces are still settling; pin a tagged release rather than
`@latest` for production. Bug reports, feature requests, and PRs welcome —
see [CONTRIBUTING.md](CONTRIBUTING.md). MIT licensed.
