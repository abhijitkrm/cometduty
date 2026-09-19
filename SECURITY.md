# Security Policy

## Supported versions

| Version | Supported |
|---|---|
| 0.1.x   | yes       |

## Reporting a vulnerability

Please **do not** open a public issue for security reports. Use GitHub's
private vulnerability reporting on this repository, or email the maintainer
listed on the repository profile.

Include: affected version/commit, reproduction steps, and impact assessment.
Expect an acknowledgement within a few days.

## Scope notes

- cometduty reads node RPC/REST endpoints and sends outbound alert
  notifications. Its threat surface is primarily config handling (YAML,
  `${ENV}` expansion, age decryption), the alert-webhook templating engine,
  and dependency hygiene.
- The monitor never receives validator key material — config files carry
  valoper *addresses* only, never private keys. Keep notifier credentials
  (PagerDuty keys, webhook URLs, bot tokens) out of committed configs; use
  `${ENV}` expansion or `cometduty encrypt`.
