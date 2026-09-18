# Cometduty Runbook

Operational reference for running cometduty in production.

## Alert catalog

Every alert has a stable `key`, a raise, and (where the condition can clear) a
paired resolve. Resolves are only delivered to destinations that received the
raise.

| Key pattern | Severity | Meaning | First response |
|---|---|---|---|
| `stalled:<chain-id>` | critical | No new block for `stalled_minutes`. The whole chain halted — not just your validator. | Check CometBFT logs on multiple validators; look for consensus deadlock, >⅓ VP offline, or an upgrade mismatch. |
| `no-nodes:<chain-id>` | critical | Every configured RPC endpoint is unreachable or catching up. | cometduty is blind — check endpoints/network first, then whether the nodes themselves are down. |
| `node-down:<url>` | `node_down_alert_severity` | One endpoint unreachable for `node_down_alert_minutes`. | Check the process/container; if intentional (maintenance), ignore or remove `alert_if_down`. |
| `node-lag:<url>` | warning | Endpoint is `lag_blocks`+ behind the best-seen height. | Node is stale or partitioned — check its peers and sync status before trusting its data. |
| `consecutive:<valcons>` | `consecutive_priority` | Validator missed `consecutive_missed` blocks in a row. | Check validator process, consensus key health, disk I/O. If it's also jailed, an `inactive:` alert follows. |
| `window-pct:<valcons>` | `percentage_priority` | Missed >`percentage_missed`% of the slashing window — approaching the chain's jail threshold. | Treat as pre-jail warning: fix the signing problem before the chain jails the validator. |
| `inactive:<valcons>` | critical | Validator left the active set: jailed, unbonded, or tombstoned (permanent — never unjails). | Jailed → fix cause, then `evmd tx slashing unjail`. Tombstoned → equivocation; the key is dead, provision a new validator. |
| `evm-lag:<chain-id>` | warning | EVM execution layer is `evm_lag_blocks`+ behind consensus height — blocks are produced but not executing. | Check the evmd/eth layer logs; a wedged EVM means the chain looks alive but processes no transactions. |
| `evm-down:<url>` | warning | The configured `evm_rpc` endpoint is unreachable. | Check the EVM RPC service; consensus may still be healthy — this is the execution layer only. |

## Files and state

| Path | Flag | Contents |
|---|---|---|
| `.cometduty-state.json` | `--state` | Open alerts + node-down timestamps for dedup across restarts. **Back this up or use a persistent volume** — losing it re-notifies open alerts. |
| `.cometduty-alerts.jsonl` | `--alert-log` | Append-only audit log: one line per delivery attempt (dest, result, attempts, error). Rotate with logrotate. Empty string disables. |

## Operations

**Reload without restart**: `kill -HUP <pid>` re-reads config. Invalid reloads
are rejected — the old config keeps running. Chain additions/removals are
diffed per-chain.

**Restart behavior**: open alerts are restored from the statefile (24h expiry).
When a previously-alerted condition has cleared during downtime, cometduty
emits the resolve on startup. `resolve_alerts_on_start: yes` instead resolves
*everything* immediately (use it after maintenance to clean downstream
incidents; still-broken conditions re-alert normally).

**Health endpoints** (on the prometheus listener):

- `/healthz` — process liveness (k8s `livenessProbe`)
- `/readyz` — true readiness: fails until every chain has ≥1 healthy RPC
  endpoint and a block seen in the last 2 minutes (k8s `readinessProbe`)
- `/metrics` — Prometheus scrape

**High availability**: run **one** replica. Alert dedup is in-process — two
live instances double-notify. For resilience use a standby (k8s `Recreate`
strategy, or systemd restart) rather than active/active.

**Notifier failures**: every delivery gets one retry after 2s, then is logged
and counted in `cometduty_notify_total{result="error"}`. Alert on that counter
— a dead webhook should page you about itself.

**Config hygiene**: `cometduty validate` checks the schema (strict — typos are
rejected with line numbers). `cometduty validate --live` additionally probes
every node and resolves every validator against the network; run it in CI or
before deploying a config change.

**Log verbosity**: `-v` for debug (per-block classification, per-destination
decisions), `--json` for machine-parseable logs.

## Common failure signatures

| Symptom | Likely cause |
|---|---|
| All validators show `missed` but chain is producing blocks | CometBFT changed signature encoding, or `valcons` mismatch — run `validate --live` and compare cons addresses. |
| `cometduty_notify_total{result="error"}` climbing | Notifier credentials expired or destination down — check `--alert-log` file for the per-attempt error. |
| `/readyz` failing after deploy | Configured nodes unreachable from the pod (DNS/policy) — `kubectl exec` and curl the RPC. |
| State file lost, alert storm on boot | Expected: no dedup memory. Set `resolve_alerts_on_start` temporarily or pre-seed the statefile. |
| Alerts fire but PagerDuty incidents never close | `dedup_key` pairing only works when raise and resolve both deliver — check the JSONL log for resolve errors. |
