# Local observability stack

Prometheus + Grafana + Loki pointed at a cometduty instance and each
validator's own CometBFT and EVM metrics, with the bundled dashboard
provisioned.

## Quick start (docker)

1. **Point Prometheus at your setup.** The shipped `prometheus.yml` assumes
   the simplest layout — one cometduty instance on its default `:28686` and
   one node on the stock ports, all published on the docker host:

   | Job | Default target | Change it when |
   |---|---|---|
   | `cometduty` | `:28686` | you run the sidecar deployment (`deploy/sidecar/`) — one target per sidecar, pattern in the file |
   | `cometbft` | `:26660` | you have more nodes or remapped ports — add one target per node, labelled `validator:` |
   | `evm` | `:8100` | same as above; drop the job on non-EVM chains |
   | `node-exporter` | bundled `node-exporter` service | you have validator hosts other than the docker host — add their node_exporter |

   Several nodes on one host must publish metrics on distinct host ports
   (e.g. `26660`, `26670`, …) — use whatever your node compose maps.

2. **Start it** (cometduty and the nodes must already be running):

   ```sh
   docker compose -f deploy/grafana/stack/compose.yml up -d
   ```

   | Service    | URL                                         |
   |------------|---------------------------------------------|
   | Grafana    | http://localhost:3000 — `admin` / `cometduty` |
   | Prometheus | http://localhost:9091                       |
   | Loki       | http://localhost:3100                       |
   | Alloy      | http://localhost:12345 (component graph / debug UI) |

   Prometheus is published on **9091**, not 9090: a Cosmos node on the same
   host already owns 9090 (gRPC). Every host port is overridable —
   `GRAFANA_PORT`, `PROMETHEUS_PORT`, `LOKI_PORT`, `ALLOY_PORT`:

   ```sh
   GRAFANA_PORT=3300 docker compose -f deploy/grafana/stack/compose.yml up -d
   ```

3. **Verify.** Open http://localhost:9091/targets — every job you kept should
   be **UP**. Then Grafana → Dashboards → *Cometduty* → *Cometduty Validator
   Monitoring*; the `validator` dropdown should list your monikers.

After editing `prometheus.yml` later: `docker compose -f
deploy/grafana/stack/compose.yml restart prometheus`.

Containers reach the host through `host.docker.internal` — built into Docker
Desktop, and mapped via `extra_hosts: host-gateway` in `compose.yml` so the
same config works on a Linux engine.

## What each scrape job covers

- `cometduty` — validator signing, alerts, endpoint health, EVM lag/txpool/gas,
  consensus round + mempool per node
- `cometbft` — each node's own `:26660/metrics` (consensus votes, rounds, p2p).
  Requires `prometheus = true` in the node's `config.toml`.
- `evm` — each node's geth-style exporter (`:8100` in-container). Coverage is
  partial: rpc latency/cache meters are live, but `chain_head_*`, `discover_*`,
  and `hashdb_*` meters exist in the registry without being updated — don't
  build alerts on them
- `node-exporter` — host CPU/disk/mem/load, from the `node-exporter` service
  bundled in `compose.yml` (reads the docker host's `/proc` and filesystems
  read-only; nothing to install). On Linux that is the machine itself; on
  Docker Desktop it is the VM your node containers run in — the resources
  they actually compete for. Production: one node_exporter per validator
  host, added to the job's target list. Neither CometBFT nor EVM endpoints
  carry kernel stats, and a full disk is a top validator killer.
  *Host Disk Used %* plots every real disk per device, so a separate
  chain-data volume is covered, not just `/`.

## Panels that are empty by design

Not every panel fills on every topology. Empty here does not mean broken:

| Panel(s) | Needs |
|---|---|
| *Node CPU %*, *Node Memory* | `metrics_url` on each node in the cometduty config (the node's own `:26660` prometheus endpoint). CPU needs two probes (~90s) before it reads non-zero |
| *Log lines by level*, *Log rule matches* | `logs:` enabled on the node in the cometduty config (docker or file source) |
| *Host Memory Used %* — `darwin` series | only for a node_exporter running natively on macOS; the `linux` series covers everything else |
| *Signed vs Missed (rate)*, *Notification Deliveries* | a few minutes of samples; notifications only after an alert is sent |
| *Seconds Since Last Block* (unfinalized) | only reported while a height is pending |
| *Precommit Stake %* | a CometBFT build that exports `consensus_precommits_staking_percentage` |

## Direct-RPC panels (Infinity)

The dashboard's bottom rows ("Peers", "Validator Set", "Node Identity",
"EVM Txpool") use the **Infinity datasource** to query CometBFT/EVM RPC
endpoints directly at render time — for tabular data that doesn't belong in
metrics (peer lists, validator sets, raw RPC state).

- Docker path: `compose.yml` installs the plugin **pinned to 3.7.1**; the
  datasource is provisioned with uid `infinity`. Infinity 4.x requires
  Grafana ≥ 11.6.11 — upgrade the Grafana image and the plugin together.
- Native path: `./bin/grafana cli --pluginsDir <data>/plugins plugins
  install yesoreyeram-infinity-datasource 3.7.1`, then restart.
- Query URLs: the stack copy (`dashboards/cometduty.json`) targets
  `host.docker.internal:26657` / `:8545` (validator 0) because Grafana runs in
  a container; the importable `../cometduty-dashboard.json` targets
  `localhost`. On other deployments edit the panel query URLs (container DNS
  or real hosts).

## Logs (Loki + Alloy)

The bottom "Logs" row is served by **Loki**, fed by **Alloy** tailing docker
container logs via a read-only socket mount — every container's stdout/stderr
lands under `{job="docker-containers", container="<name>"}`.

- The split is deliberate: cometduty's `logs:` watcher decides what's
  page-worthy and counts it (`cometduty_log_matches_total`); Loki keeps full
  retention (7d default in `loki-config.yml`) for "what happened" forensics.
- Restrict collection to your validators with the commented `keep` rule in
  `alloy-config.alloy` (e.g. `regex = "/evmd.*"`).
- Panels: *Log lines by level* + *Rule matches* come from cometduty's own
  metrics; *Container logs* + *Error lines* query the provisioned `loki`
  datasource.

## Troubleshooting

| Symptom | Cause / fix |
|---|---|
| Dashboard empty, every target DOWN with `connection refused` | `prometheus.yml` targets don't match your ports — see step 1, then restart prometheus. From inside a container, `localhost` is the container itself; use `host.docker.internal`. |
| `Infinity plugin failed … 404 Not Found, loading react/jsx-runtime` | Infinity newer than the Grafana image supports (4.x on Grafana < 11.6.11). Keep the pin in `compose.yml`; if a newer version is already in the `grafana-data` volume, remove it and recreate: `docker compose exec grafana rm -rf /var/lib/grafana/plugins/yesoreyeram-infinity-datasource && docker compose up -d --force-recreate grafana` |
| Infinity panels error right after first start | Grafana downloads the plugin on boot; hard-refresh once startup finishes. |
| `Bind for 0.0.0.0:<port> failed: port is already allocated` | Another service owns that host port — override it (`PROMETHEUS_PORT=…` etc.). |
| `validator` dropdown empty | The `cometduty` target is DOWN, or cometduty hasn't processed a block yet. |
| *Node CPU %* / *Node Memory* empty | No `metrics_url` on the nodes in the cometduty config — add it and reload (`kill -HUP`). |

## Native fallback (no docker)

If image pulls don't work, run both directly — the same configs apply with
`localhost` in place of `host.docker.internal`:

```sh
# prometheus (darwin-arm64 tarball from github releases) — keep 9090 free for the node
prometheus --config.file=prometheus.yml --storage.tsdb.path=./prom-data \
  --web.listen-address=:9091

# grafana (tarball from dl.grafana.com)
GF_PATHS_PROVISIONING=$PWD/provisioning \
GF_SECURITY_ADMIN_PASSWORD=cometduty \
GF_SERVER_HTTP_PORT=3000 \
grafana-server --homepath <grafana-dir>
```

Point the datasource URL in `provisioning/datasources/prometheus.yaml` at
`http://localhost:9091` and the dashboards provider path at wherever
`dashboards/cometduty.json` lives (generated from
`../cometduty-dashboard.json` with `s/${DS_PROMETHEUS}/cometduty-prom/`).
