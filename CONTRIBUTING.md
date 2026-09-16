# Contributing

Thanks for helping keep validator monitoring boring. cometduty is MIT-licensed
open source; contributions of all kinds are welcome.

## Development

Requires Go 1.26+ (see `go.mod`).

```sh
go build ./...
go vet ./...
gofmt -l .            # must be empty
go test ./...
go test -race ./...   # please run before submitting
```

Run the monitor locally:

```sh
go run ./cmd/cometduty example-config > config.yml
go run ./cmd/cometduty -f config.yml -v
```

## Guidelines

- **No new global state.** Everything hangs off `Supervisor`/`Chain`/`Engine`
  and is injected — this is what makes the codebase testable.
- **Keep the RPC layer dependency-free.** `internal/rpc` deliberately avoids
  importing cometbft/tendermint libraries so we can talk to many server
  versions. Add endpoints by extending the hand-rolled client.
- **Notifiers are small.** Copy `internal/alert/notifiers/webhook.go` for the
  shape: a config type in `internal/config`, `Send`/`Test` methods, an
  httptest-based test.
- **Consensus keys:** extend `internal/consensus` — most single-key pubkey
  types decode with the generic path; only genuinely different wire shapes
  need new code.
- Bugs should come with a failing test when practical.
- No secrets in the tree. Use `${ENV_VAR}` expansion or `cometduty encrypt`.

## Reporting issues

Please include: cometduty version, the chain's CometBFT/Tendermint version and
SDK flavor, redacted config, and logs at `-v`. For alert-related bugs, mention
the destination and whether a resolve was expected.
