# Migrating from tenderduty v2

cometduty keeps tenderduty v2's YAML field names wherever practical. A v2
`config.yml` will mostly load as-is; this document lists the differences and
the new knobs.

## Renamed / moved

| v2 | cometduty | notes |
|---|---|---|
| `enabled` (top level) | — | removed; the daemon only runs when started |
| `enable_dashboard` / `listen_port` / `hide_logs` / `dashboard_*` | — | **removed** — the built-in UI is gone in v3; use Prometheus metrics + logs, or point a frontend at your monitoring stack |
| `node_down_alert_minutes` | `node_down_alert_minutes` | unchanged |
| `prometheus_enabled` / `prometheus_listen_port` | same + `prometheus_bind` | unchanged |
| `alerts.consecutive_missed` etc. | same | unchanged |
| `alerts.percentage_missed` | `alerts.percentage_missed` | unchanged (a percent, not a fraction) |
| `chains.<name>.valoper_address` | same | unchanged |
| `chains.<name>.valcons_override` | same | unchanged |
| `chains.<name>.nodes[].url` / `alert_if_down` | same | unchanged |

## Removed

- `discord.users` / `telegram.users` — both became `mentions:` (lists of
  mention strings) and are now actually delivered.
- Custom AES encryption — replaced by `age` passphrase encryption
  (`cometduty encrypt`, `--password` / `PASSWORD`). Re-encrypt remote configs.
- The embedded dashboard/UI — removed in v3 (Prometheus metrics and structured logs are the observability surface).

## New

- `validators:` — list of `{valoper_address|valcons_override, label, alerts}`
  to monitor several validators on one chain.
- `alerts.stalled_enabled` / `stalled_minutes`, `alerts.lag_enabled` /
  `lag_blocks`, per-validator `alerts:` overrides.
- `webhook:` (templated JSON POST), `ntfy:`, `opsgenie:` destinations.
- `flap_suppression_minutes`, `reminder_minutes`, `resolve_alerts_on_start`
  (sends resolve notifications for alerts that were open when the process
  stopped — cleans up PagerDuty/Opsgenie incidents across restarts).
- `node.headers`, `node.insecure_tls`, `node.disable_vote`, `node.name`.
- `public_fallback` — use chain-registry public RPCs when all nodes fail.
- `chains.d/` directory of per-chain files (`--chains-dir`).
- `${ENV_VAR}` expansion in config; `SIGHUP` hot-reload.
- `cometduty test-alert`, `validate`, `example-config`, `encrypt`, `decrypt`.

## Behavioural differences worth knowing

- **Alerts dedup on every destination** including Slack (v2 never dedup'd
  Slack — restarts and repeats could spam it).
- **Resolve notifications actually send** for stall and window-percentage
  alerts (both were unreachable in v2).
- **Absent commit signatures** are detected by `block_id_flag`, not by an
  empty `validator_address` — correct on chains that include the address.
- **Chains without x/slashing** (beacon-kit, Penumbra) no longer spew query
  errors; window/tombstone features disable automatically, or set
  `slashing_enabled: false` to skip detection.
- `SIGHUP` **reloads** config instead of killing the process.
