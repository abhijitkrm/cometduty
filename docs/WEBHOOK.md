# Webhook payload and silence API

Two optional interfaces let other tools work with cometduty: a webhook that
sends every alert as a versioned JSON document, and an Alertmanager-compatible
API to silence alerts during planned work. Neither is needed to run
cometduty, and neither is tied to a particular receiver.

## Webhook

```yaml
webhook:
  enabled: yes
  url: https://receiver.example.com/alerts
  secret: ${WEBHOOK_SECRET}     # optional: sign every POST
```

The chain's `alerts.webhook.enabled` must be on too, as for every
destination. Without `template_body`, each raise, resolve and reminder is
POSTed as:

```json
{
  "version": "1",
  "source": "cometduty",
  "status": "firing",
  "key": "node-down:http://127.0.0.1:26657",
  "kind": "node-down",
  "target": "http://127.0.0.1:26657",
  "severity": "critical",
  "chain": "mychain",
  "chain_id": "mychain-1",
  "node": "validator-1",
  "moniker": "",
  "valcons": "",
  "labels": {"profile": "validator-1"},
  "message": "validator-1 (http://127.0.0.1:26657) has been down for 3 minutes",
  "resolved": false,
  "reminder": false,
  "time": "2026-10-14T09:12:00Z"
}
```

| field | meaning |
|---|---|
| `version` | payload version. Fields are only added within a version; ignore ones you don't know |
| `status` | `firing` or `resolved`. A resolve carries the same `key` as its raise |
| `key` | `<kind>:<target>` — identifies the condition; pair raises and resolves on it |
| `kind` | the alert type: `node-down`, `stalled`, `consecutive`, `signer-regressed`, `log-<rule>`, … (the README's alert table) |
| `target` | what it is about: a node URL, a valcons address, a valoper address, or the chain id |
| `node` | the node's `name` from the config, when the alert is about a node |
| `labels` | the `labels:` of that node or validator in the config — use them to name it in your own tool |
| `reminder` | `true` on a re-send of a still-open alert (`reminder_minutes`) |

`message` is human-readable text. Some of it comes from outside cometduty —
`log-<rule>` alerts quote node log lines — so a receiver should decide what
to do from `kind`, `target` and `labels`, and treat `message` as untrusted
text.

### Signatures

With `secret` set, every POST carries:

```
X-Cometduty-Timestamp: 1791969120
X-Cometduty-Signature: sha256=<hex HMAC-SHA256(secret, "<timestamp>.<body>")>
```

To verify: recompute the HMAC over the timestamp, a `.`, and the raw body,
compare in constant time, and refuse timestamps more than a few minutes old
(replays). In Go:

```go
mac := hmac.New(sha256.New, []byte(secret))
mac.Write([]byte(r.Header.Get("X-Cometduty-Timestamp") + "."))
mac.Write(body)
ok := hmac.Equal([]byte("sha256="+hex.EncodeToString(mac.Sum(nil))),
	[]byte(r.Header.Get("X-Cometduty-Signature")))
```

## Silence API

```yaml
prometheus_enabled: yes
api_token: ${COMETDUTY_API_TOKEN}   # 16+ characters; empty = no API
```

Served on the prometheus listener. It is the Alertmanager v2 silence API, so
`amtool`, Grafana and scripts written for Alertmanager work unchanged:

| request | does |
|---|---|
| `POST /api/v2/silences` | create (or replace, with `id`) → `{"silenceID": "…"}` |
| `GET /api/v2/silences` | list, with `status.state` = pending / active / expired |
| `GET /api/v2/silence/{id}` | one silence |
| `DELETE /api/v2/silence/{id}` | expire it now |

Every request needs `Authorization: Bearer <api_token>`.

```sh
curl -s -X POST http://validator-1:28686/api/v2/silences \
  -H "Authorization: Bearer $COMETDUTY_API_TOKEN" -H 'Content-Type: application/json' \
  -d '{"matchers":[{"name":"node","value":"validator-1","isRegex":false}],
       "startsAt":"2026-10-14T09:00:00Z","endsAt":"2026-10-14T09:30:00Z",
       "createdBy":"ops","comment":"binary upgrade"}'
```

Matchers select on the alert's labels: `alertname` and `kind` (the same
value), `target`, `node`, `chain`, `chain_id`, `moniker`, `valcons`,
`severity`, plus the node's or validator's own `labels`. `isRegex` matches
the whole value; `isEqual: false` negates. At least one matcher must select
something — a silence matching every alert is refused — and one silence lasts
at most 7 days.

What a silence does:

- A raise it matches is **held**, not dropped. If the condition is still
  open when the silence ends, the alert is sent then.
- A condition that clears during the silence is never sent at all — planned
  downtime doesn't page.
- Reminders for an alert that was already open are paused.
- Resolves are never silenced, so incidents opened before the silence still
  close.

Silences survive restarts (they are saved in `--state`), and
`cometduty_silences_active` counts the ones in effect — worth an alert of its
own, since a silence left on is a monitor gone blind.
