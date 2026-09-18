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
