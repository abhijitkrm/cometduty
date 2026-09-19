# Installing cometduty

## Install script (recommended)

Downloads the latest release for your OS/arch, verifies the sha256 checksum
against the release's `checksums.txt`, and installs the binary:

```sh
curl -fsSL https://raw.githubusercontent.com/abhijitkrm/cometduty/main/scripts/install.sh | sh
```

Options (pass after `sh -s --`):

```sh
# specific version and/or install dir
... | sh -s -- --version v0.1.0 --dir /usr/local/bin
```

Install dir defaults to `/usr/local/bin` when writable, else `~/.local/bin`.

## Prebuilt binaries

Grab the tarball for your platform from
[releases](https://github.com/abhijitkrm/cometduty/releases) and verify it:

```sh
# linux amd64 example
curl -fLO https://github.com/abhijitkrm/cometduty/releases/download/v0.1.0/cometduty_0.1.0_linux_amd64.tar.gz
curl -fLO https://github.com/abhijitkrm/cometduty/releases/download/v0.1.0/checksums.txt
grep cometduty_0.1.0_linux_amd64.tar.gz checksums.txt | shasum -a 256 -c -
tar -xzf cometduty_0.1.0_linux_amd64.tar.gz cometduty
```

Supported targets: `linux/amd64`, `linux/arm64`, `darwin/amd64`,
`darwin/arm64`.

## go install

Requires Go 1.26+ (see `go.mod` — `cosmossdk.io/api` sets the floor):

```sh
go install github.com/abhijitkrm/cometduty/cmd/cometduty@latest
```

## Docker

Multi-arch images are published to GHCR on every release:

```sh
docker run -d --name cometduty \
  -v $PWD/config.yml:/config/config.yml:ro \
  -v cd-data:/data \
  -p 28686:28686 \
  ghcr.io/abhijitkrm/cometduty:latest
```

The image runs as nonroot; `/data` holds the state file and JSONL alert log.
Kubernetes manifests live in `deploy/k8s/`.

### No-registry-pull fallback

If base-image pulls stall (proxy/offline), package a locally built binary —
no registry access needed:

```sh
CGO_ENABLED=0 go build -o cometduty ./cmd/cometduty
docker build -f deploy/Dockerfile.local -t cometduty:local .
```

## From source

```sh
git clone https://github.com/abhijitkrm/cometduty.git
cd cometduty
go build -o cometduty ./cmd/cometduty
```

## Next steps

```sh
cometduty example-config > config.yml   # annotated reference config
cometduty validate --live -f config.yml # probe nodes, resolve validators
cometduty test-alert discord -f config.yml
cometduty -f config.yml                 # run — metrics on :28686
```

See the [README](../README.md#quick-start) for a minimal config and
[docs/RUNBOOK.md](RUNBOOK.md) for operating it in production.
