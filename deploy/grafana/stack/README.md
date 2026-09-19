# Local observability stack

Prometheus + Grafana pointed at a cometduty instance and each validator's own
CometBFT metrics.

## Docker (preferred)

```sh
docker compose -f deploy/grafana/stack/compose.yml up -d
# Grafana    http://localhost:3000  (admin / cometduty)
# Prometheus http://localhost:9090
```

cometduty runs on the host — `host.docker.internal` bridges it into the
containers. Adjust `prometheus.yml` targets for your ports.

## What each scrape job covers

- `cometduty` — validator signing, alerts, endpoint health, EVM lag/txpool/gas,
  consensus round + mempool per node
- `cometbft` — each node's own `:26660/metrics` (consensus votes, rounds, p2p)
- `evm` — each node's geth-style exporter (`:8100` in-container). Coverage is
  partial: rpc latency/cache meters are live, but `chain_head_*`, `discover_*`,
  and `hashdb_*` meters exist in the registry without being updated — don't
  build alerts on them
- `node-exporter` — host CPU/disk/mem/load. The job points at
  `host.docker.internal:9101`; run the exporter on the docker host:

  ```sh
  node_exporter --web.listen-address=:9101   # brew install node_exporter
  ```

  Production: one node_exporter per validator host — swap the target list.
  Neither CometBFT nor EVM endpoints carry kernel stats, and a full disk is
  a top validator killer.

## Direct-RPC panels (Infinity)

The dashboard's bottom rows ("Peers", "Validator Set", "Node Identity",
"EVM Txpool") use the **Infinity datasource** to query CometBFT/EVM RPC
endpoints directly at render time — for tabular data that doesn't belong in
metrics (peer lists, validator sets, raw RPC state).

- Docker path: `GF_INSTALL_PLUGINS=yesoreyeram-infinity-datasource` is set
  in `compose.yml`; the datasource is provisioned with uid `infinity`.
- Native path: `./bin/grafana cli --pluginsDir <data>/plugins plugins
  install yesoreyeram-infinity-datasource`, then restart.
- The shipped panels target `localhost` URLs — on a different deployment
  edit the panel query URLs (container DNS or real hosts).

## Native fallback (no docker)

If image pulls don't work, run both directly — the same configs apply with
`localhost` targets:

```sh
# prometheus (darwin-arm64 tarball from github releases)
prometheus --config.file=prometheus.yml --storage.tsdb.path=./prom-data

# grafana (tarball from dl.grafana.com)
GF_PATHS_PROVISIONING=$PWD/provisioning \
GF_SECURITY_ADMIN_PASSWORD=cometduty \
GF_SERVER_HTTP_PORT=3100 \
grafana-server --homepath <grafana-dir>
```

Point the datasource URL in `provisioning/datasources/prometheus.yaml` at
`http://localhost:9090` and the dashboards provider path at wherever
`dashboards/cometduty.json` lives (generated from
`../cometduty-dashboard.json` with `s/${DS_PROMETHEUS}/cometduty-prom/`).
