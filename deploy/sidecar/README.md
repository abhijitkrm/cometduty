# cometduty sidecar deployment

Attach one cometduty **sidecar** per validator container, plus one
**chain-role** instance for the network-wide view. Each alert class is owned
by exactly one layer, so nothing pages twice.

| role | owns |
|---|---|
| `sidecar` (per validator) | node-down, catching-up, stuck sync, node lag, mempool/round, CPU/mem, EVM down/lag/txpool, disk-free, signer state, and its own validator's signing alerts (consecutive, window %, jailed, stake) |
| `chain` (one per network) | stalled chain, no-working-RPC, validator-set changes (join / leave / jail), future upgrade + governance alerts |
| `standalone` (default) | everything — one-instance deployments, unchanged behavior |

## Why sidecars

Consensus-wide signals look identical from any node — one observer is enough
and N copies of it just mean N copies of every page. Node-local signals only
exist *next to the node*: its home directory (disk free, `priv_validator_state`),
its own EVM endpoint, its own metrics port. Sidecars get those first-hand and
label every series with `validator="<moniker>"`, so the Grafana dropdown
filters the whole board per validator.

## Files

- `sidecar.yml` — config template; every `${VAR}` is expanded by cometduty,
  so one file serves all sidecars
- `compose.yml` — one sidecar per validator host (host network)
- `chain.yml` + `compose.chain.yml` — the central `role: chain` instance
- `.env.example` — the per-node settings

## Wiring

The sidecar runs on the validator's host with `network_mode: host` and
reaches the node on the ports it publishes there (`127.0.0.1:26657`, `:8545`,
`:26660` by default; change `NODE_*` for other ports). That works the same
for a node in a container, under systemd, or run by hand.

The node home is mounted read-only at `/node-home` — set `NODE_HOME` to the
host path of the node's home. That enables:

- `cometduty_node_disk_free_bytes` + `disk-low` alert
- `cometduty_node_signer_height` (last signed height from
  `priv_validator_state.json`), plus `signer-stalled`, `signer-state`
  (unreadable) and `signer-regressed` (height went backwards —
  double-sign risk) alerts

Log alerts are optional: set `LOGS_ENABLED=yes` and `NODE_CONTAINER`, and
start with `--profile logs`. That adds `docker-proxy`, a read-only socket
proxy on loopback exposing **only** container listing and log streaming.

## Run

```sh
cp .env.example .env   # fill in CHAIN_*, VALIDATOR, VALOPER, NODE_HOME
docker compose --env-file .env up -d                  # + --profile logs
docker compose -f compose.chain.yml --env-file .env up -d   # once per network
```

Then point Prometheus at each sidecar's `:28686` and the chain instance's
`:28690`.

## Connecting other tools

Both are optional and work with any tool, not just cometcli:

- **Webhook** (`WEBHOOK_*`) — each alert as a versioned JSON document,
  HMAC-signed with `WEBHOOK_SECRET`; see [docs/WEBHOOK.md](../../docs/WEBHOOK.md).
- **Silence API** (`API_TOKEN`) — Alertmanager v2 compatible, so amtool,
  Grafana or a deploy script can mute alerts during planned maintenance.

[cometcli](https://github.com/abhijitkrm/cometcli) can write these files for a
network it manages (`cometcli network export --format cometduty`) — you don't
need it to run cometduty.
