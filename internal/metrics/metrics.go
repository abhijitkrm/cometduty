// Package metrics exposes prometheus gauges. The monitor calls it directly —
// the prometheus client is goroutine-safe, so no channel plumbing is needed.
// Metric names use the cometduty_ prefix (tenderduty's were tenderduty_*; a
// migration note lives in docs/).
package metrics

import (
	"fmt"
	"net/http"
	"runtime"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/abhijitkrm/cometduty/internal/monitor"
)

var chainLabels = []string{"name", "chain_id", "validator", "moniker"}
var endpointLabels = []string{"name", "chain_id", "endpoint", "label"}
var infoLabels = []string{"version", "go_version"}

// Exporter implements monitor.MetricsSink.
type Exporter struct {
	signed       *prometheus.GaugeVec
	proposed     *prometheus.GaugeVec
	missed       *prometheus.GaugeVec
	prevote      *prometheus.GaugeVec
	precommit    *prometheus.GaugeVec
	consecutive  *prometheus.GaugeVec
	windowMissed *prometheus.GaugeVec
	windowSize   *prometheus.GaugeVec
	windowPct    *prometheus.GaugeVec
	lastBlockSec *prometheus.GaugeVec
	lastUnfinal  *prometheus.GaugeVec
	lastHeight   *prometheus.GaugeVec
	sigRatio     *prometheus.GaugeVec
	activeAlerts *prometheus.GaugeVec

	nodesTotal     *prometheus.GaugeVec
	nodesUnhealthy *prometheus.GaugeVec
	endpointDown   *prometheus.GaugeVec
	endpointLag    *prometheus.GaugeVec
	endpointPeers  *prometheus.GaugeVec
}

// New registers all metrics.
func New(version string) *Exporter {
	e := &Exporter{
		signed: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "cometduty_signed_blocks",
			Help: "count of blocks signed since cometduty started",
		}, chainLabels),
		proposed: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "cometduty_proposed_blocks",
			Help: "count of blocks proposed since cometduty started",
		}, chainLabels),
		missed: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "cometduty_missed_blocks",
			Help: "count of blocks missed since cometduty started",
		}, chainLabels),
		prevote: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "cometduty_missed_blocks_prevote_present",
			Help: "missed blocks where a prevote was observed",
		}, chainLabels),
		precommit: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "cometduty_missed_blocks_precommit_present",
			Help: "missed blocks where a precommit was observed",
		}, chainLabels),
		consecutive: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "cometduty_consecutive_missed_blocks",
			Help: "current consecutive missed blocks",
		}, chainLabels),
		windowMissed: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "cometduty_missed_blocks_for_window",
			Help: "chain-reported missed blocks in the slashing window",
		}, chainLabels),
		windowSize: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "cometduty_missed_block_window",
			Help: "the slashing signed-blocks window",
		}, chainLabels),
		windowPct: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "cometduty_window_missed_percent",
			Help: "percent of the slashing window currently missed",
		}, chainLabels),
		lastBlockSec: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "cometduty_time_since_last_block",
			Help: "block-to-block latency observed at last finalized block",
		}, []string{"name", "chain_id"}),
		lastUnfinal: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "cometduty_time_since_last_block_unfinalized",
			Help: "seconds since any block was last seen — updated continuously, useful for stall detection",
		}, []string{"name", "chain_id"}),
		lastHeight: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "cometduty_last_block_height",
			Help: "most recent observed block height",
		}, []string{"name", "chain_id"}),
		sigRatio: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "cometduty_block_signature_ratio",
			Help: "fraction of the validator set that signed the last block (network participation)",
		}, []string{"name", "chain_id"}),
		activeAlerts: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "cometduty_active_alerts",
			Help: "open alerts for this chain",
		}, []string{"name", "chain_id"}),
		nodesTotal: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "cometduty_total_monitored_endpoints",
			Help: "configured endpoints for the chain",
		}, []string{"name", "chain_id"}),
		nodesUnhealthy: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "cometduty_total_unhealthy_endpoints",
			Help: "unhealthy endpoints for the chain",
		}, []string{"name", "chain_id"}),
		endpointDown: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "cometduty_endpoint_down_seconds",
			Help: "how long an endpoint has been marked unhealthy",
		}, endpointLabels),
		endpointLag: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "cometduty_endpoint_lag_blocks",
			Help: "how many blocks behind the best-seen height this endpoint is",
		}, endpointLabels),
		endpointPeers: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "cometduty_endpoint_peers",
			Help: "peer count reported by the endpoint",
		}, endpointLabels),
	}
	promauto.NewGauge(prometheus.GaugeOpts{
		Name: "cometduty_info",
		Help: "build information",
		ConstLabels: prometheus.Labels{
			"version":    version,
			"go_version": runtime.Version(),
		},
	}).Set(1)
	return e
}

// BlockResult implements monitor.MetricsSink.
func (e *Exporter) BlockResult(name, chainID, validator, moniker string, st monitor.SignState, consecutive int64) {
	l := prometheus.Labels{"name": name, "chain_id": chainID, "validator": validator, "moniker": moniker}
	e.consecutive.With(l).Set(float64(consecutive))
	switch st {
	case monitor.SignProposed:
		e.proposed.With(l).Inc()
		e.signed.With(l).Inc()
	case monitor.SignSigned:
		e.signed.With(l).Inc()
	case monitor.SignPrecommit:
		e.missed.With(l).Inc()
		e.precommit.With(l).Inc()
	case monitor.SignPrevote:
		e.missed.With(l).Inc()
		e.prevote.With(l).Inc()
	case monitor.SignMissed:
		e.missed.With(l).Inc()
	}
}

// LastBlock implements monitor.MetricsSink.
func (e *Exporter) LastBlock(name, chainID string, height int64, sincePrev float64) {
	l := prometheus.Labels{"name": name, "chain_id": chainID}
	e.lastHeight.With(l).Set(float64(height))
	if sincePrev > 0 && sincePrev < 3600 {
		e.lastBlockSec.With(l).Set(sincePrev)
	}
}

// Tick updates the continuously-updating unfinalized gauge; call from the
// chain's watch loop.
func (e *Exporter) Tick(name, chainID string, lastBlockTime time.Time) {
	if lastBlockTime.IsZero() {
		return
	}
	e.lastUnfinal.With(prometheus.Labels{"name": name, "chain_id": chainID}).Set(time.Since(lastBlockTime).Seconds())
}

// SignatureRatio implements monitor.MetricsSink.
func (e *Exporter) SignatureRatio(name, chainID string, ratio float64) {
	e.sigRatio.With(prometheus.Labels{"name": name, "chain_id": chainID}).Set(ratio)
}

// Window implements monitor.MetricsSink.
func (e *Exporter) Window(name, chainID, validator, moniker string, missed, window int64) {
	l := prometheus.Labels{"name": name, "chain_id": chainID, "validator": validator, "moniker": moniker}
	e.windowMissed.With(l).Set(float64(missed))
	e.windowSize.With(l).Set(float64(window))
	if window > 0 {
		e.windowPct.With(l).Set(100 * float64(missed) / float64(window))
	}
}

// NodeHealth implements monitor.MetricsSink.
func (e *Exporter) NodeHealth(name, chainID, endpoint, label string, downSeconds, lagBlocks, peers float64) {
	l := prometheus.Labels{"name": name, "chain_id": chainID, "endpoint": endpoint, "label": label}
	e.endpointDown.With(l).Set(downSeconds)
	e.endpointLag.With(l).Set(lagBlocks)
	e.endpointPeers.With(l).Set(peers)
}

// NodeCount records totals per chain.
func (e *Exporter) NodeCount(name, chainID string, total, unhealthy int) {
	l := prometheus.Labels{"name": name, "chain_id": chainID}
	e.nodesTotal.With(l).Set(float64(total))
	e.nodesUnhealthy.With(l).Set(float64(unhealthy))
}

// ActiveAlerts implements monitor.MetricsSink.
func (e *Exporter) ActiveAlerts(name, chainID string, n int) {
	e.activeAlerts.With(prometheus.Labels{"name": name, "chain_id": chainID}).Set(float64(n))
}

// Serve starts the metrics HTTP server. It returns on shutdown or fatal error.
func Serve(ctxDone <-chan struct{}, bind string, port int) error {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	srv := &http.Server{
		Addr:              fmt.Sprintf("%s:%d", bind, port),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       20 * time.Second,
		WriteTimeout:      20 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		<-ctxDone
		_ = srv.Close()
	}()
	return srv.ListenAndServe()
}
