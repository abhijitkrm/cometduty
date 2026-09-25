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
- `chain.yml` — the central `role: chain` config
- `compose.primium.yml` — overlay for a primium-1-style localnet
- `.env.example` — required per-validator env vars

## Wiring

Sidecars join the validators' docker network (`primium-local`, external) and
reach the node by its container DNS name — no `network_mode` sharing needed,
so each sidecar publishes its own metrics port (`:28700`+).

The node home is mounted read-only at `/node-home` — set `VAL{N}_HOME` to the
same host path the validator uses. That enables:

- `cometduty_node_disk_free_bytes` + `disk-low` alert
- `cometduty_node_signer_height` (last signed height from
  `priv_validator_state.json`), plus `signer-stalled`, `signer-state`
  (unreadable) and `signer-regressed` (height went backwards —
  double-sign risk) alerts

`docker-proxy` is a read-only socket proxy exposing **only** container
listing and log streaming — the Phase 2 log watcher uses it. Leave it out if
log alerts aren't wanted yet.

## Run

```sh
cp .env.example .env   # fill VAL{N}_HOME + VAL{N}_OPER
docker compose -f compose.primium.yml --env-file .env up -d
```

Then point Prometheus at `:28700`–`:28704` on the host.
