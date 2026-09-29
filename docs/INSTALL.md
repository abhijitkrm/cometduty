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

Multi-arch images are published to GHCR on every release.

Create the config file **before** starting the container. If the host path in
a `-v` bind mount doesn't exist, docker silently creates an empty directory
there instead, and cometduty fails with `config /config/config.yml is a
directory`. The image can generate the reference config itself:

```sh
docker run --rm --entrypoint /cometduty ghcr.io/abhijitkrm/cometduty:latest \
  example-config > config.yml
# edit config.yml, then validate it inside the image:
docker run --rm -v $PWD/config.yml:/config/config.yml:ro \
  --entrypoint /cometduty ghcr.io/abhijitkrm/cometduty:latest \
  validate -f /config/config.yml
```

Then run:

```sh
docker run -d --name cometduty \
  -v $PWD/config.yml:/config/config.yml:ro \
  -v cd-data:/data \
  -p 28686:28686 \
  ghcr.io/abhijitkrm/cometduty:latest
```

If you already hit the error, the stray directory is on the host — remove it
(`rmdir config.yml`), create the file as above, and start the container again.

The image runs as a non-root user (uid **65532**) — never as root; `/data`
holds the state file and JSONL alert log, and ships owned by that uid so a
fresh named volume is writable.

- **Bind-mounting a host directory** instead of a named volume: make it
  writable by uid 65532 first — `sudo chown 65532:65532 ./cd-data`.
- **Volume first used with an image from v0.2.0 or earlier** (root-owned, logs `periodic
  state save failed … permission denied`): fix it once —
  `docker run --rm -v cd-data:/data alpine chown 65532:65532 /data` — then
  restart the container.

For docker compose, see `deploy/docker-compose.yml` (same rule: create
`config.yml` next to it first).
Kubernetes manifests live in `deploy/k8s/`.

### No-registry-pull fallback

If base-image pulls stall (proxy/offline), package a locally built binary —
no registry access needed:

```sh
CGO_ENABLED=0 go build -o cometduty ./cmd/cometduty
docker build -f deploy/Dockerfile.local -t cometduty:local .
```

## Run as a service (systemd, Linux)

The unit in `deploy/cometduty.service` runs cometduty as a dedicated
unprivileged `cometduty` user with a hardened sandbox. systemd creates the
state directory (`/var/lib/cometduty`) with the right ownership; you create
the account and the config:

```sh
sudo useradd --system --no-create-home --shell /usr/sbin/nologin cometduty
sudo install -d -m 0750 -o root -g cometduty /etc/cometduty
sudo install -m 0640 -o root -g cometduty config.yml /etc/cometduty/config.yml
# secrets referenced as ${VAR} in the config go in /etc/cometduty/env (0640, same owner)
sudo cp deploy/cometduty.service /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable --now cometduty
journalctl -u cometduty -f
```

The config is root-owned and only group-readable, so the service can read
but never rewrite it. Reload after a config change with
`sudo systemctl reload cometduty` (SIGHUP).

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
