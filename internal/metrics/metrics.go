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

	evmHeight     *prometheus.GaugeVec
	evmLag        *prometheus.GaugeVec
	evmDown       *prometheus.GaugeVec
	evmSyncing    *prometheus.GaugeVec
	notifyResults *prometheus.CounterVec

	mempoolTxs     *prometheus.GaugeVec
	mempoolBytes   *prometheus.GaugeVec
	consensusRound *prometheus.GaugeVec
	evmTxPending   *prometheus.GaugeVec
	evmTxQueued    *prometheus.GaugeVec
	evmGasRatio    *prometheus.GaugeVec
	nodeCPUPct     *prometheus.GaugeVec
	nodeMemBytes   *prometheus.GaugeVec

	valJailed      *prometheus.GaugeVec
	valTombstoned  *prometheus.GaugeVec
	valBonded      *prometheus.GaugeVec
	valJailedUntil *prometheus.GaugeVec
	valTokens      *prometheus.GaugeVec
	valPower       *prometheus.GaugeVec
	valPriority    *prometheus.GaugeVec
	nodeInfo       *prometheus.GaugeVec
	evmGasPrice    *prometheus.GaugeVec
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
		evmHeight: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "cometduty_evm_block_height",
			Help: "latest executed EVM block reported by the configured evm_rpc endpoint",
		}, []string{"name", "chain_id"}),
		evmLag: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "cometduty_evm_lag_blocks",
			Help: "consensus height minus EVM-executed height — execution lag; nonzero means transactions aren't executing",
		}, []string{"name", "chain_id"}),
		evmDown: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "cometduty_evm_endpoint_down_seconds",
			Help: "how long the configured evm_rpc endpoint has been unreachable",
		}, []string{"name", "chain_id", "endpoint"}),
		evmSyncing: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "cometduty_evm_syncing",
			Help: "1 while eth_syncing reports an in-progress sync",
		}, []string{"name", "chain_id"}),
		notifyResults: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "cometduty_notify_total",
			Help: "notifier delivery attempts by destination and result — alert on result=error",
		}, []string{"dest", "result"}),
		mempoolTxs: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "cometduty_mempool_txs",
			Help: "unconfirmed txs in the node's consensus mempool (num_unconfirmed_txs)",
		}, []string{"name", "chain_id", "endpoint"}),
		mempoolBytes: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "cometduty_mempool_txs_bytes",
			Help: "total bytes of unconfirmed txs in the node's consensus mempool",
		}, []string{"name", "chain_id", "endpoint"}),
		consensusRound: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "cometduty_consensus_round",
			Help: "current consensus round — sustained elevation = leader churn",
		}, []string{"name", "chain_id", "endpoint"}),
		evmTxPending: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "cometduty_evm_txpool_pending",
			Help: "executable transactions waiting in the EVM txpool (txpool_status.pending)",
		}, []string{"name", "chain_id"}),
		evmTxQueued: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "cometduty_evm_txpool_queued",
			Help: "gapped-nonce transactions queued in the EVM txpool (txpool_status.queued)",
		}, []string{"name", "chain_id"}),
		evmGasRatio: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "cometduty_evm_gas_used_ratio",
			Help: "gasUsed/gasLimit of the latest EVM block — sustained ~1.0 is saturation",
		}, []string{"name", "chain_id"}),
		nodeCPUPct: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "cometduty_node_cpu_percent",
			Help: "node process CPU usage percent, from its own metrics endpoint (metrics_url)",
		}, []string{"name", "chain_id", "endpoint"}),
		nodeMemBytes: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "cometduty_node_memory_bytes",
			Help: "node process resident memory in bytes, from its own metrics endpoint (metrics_url)",
		}, []string{"name", "chain_id", "endpoint"}),
		valJailed: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "cometduty_validator_jailed",
			Help: "1 while the validator is jailed (staking module state)",
		}, chainLabels),
		valTombstoned: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "cometduty_validator_tombstoned",
			Help: "1 when the validator is tombstoned — permanent, cannot unjail",
		}, chainLabels),
		valBonded: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "cometduty_validator_bonded",
			Help: "1 while the validator is in the bonded (active) set",
		}, chainLabels),
		valJailedUntil: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "cometduty_slashing_jailed_until_seconds",
			Help: "unix timestamp when the validator may unjail (signing_info.jailed_until); 0 when not jailed",
		}, chainLabels),
		valTokens: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "cometduty_validator_bonded_tokens",
			Help: "bonded stake in the chain's base denom (staking module tokens)",
		}, chainLabels),
		valPower: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "cometduty_validator_voting_power",
			Help: "consensus voting power from /validators — determines propose/sign weight",
		}, chainLabels),
		valPriority: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "cometduty_validator_proposer_priority",
			Help: "proposer priority from /validators — relative chance to propose next",
		}, chainLabels),
		nodeInfo: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "cometduty_node_info",
			Help: "node identity from /status — always 1, labels carry moniker/version/network",
		}, []string{"name", "chain_id", "endpoint", "moniker", "version", "network"}),
		evmGasPrice: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "cometduty_evm_gas_price",
			Help: "eth_gasPrice in wei — fee pressure on the execution layer",
		}, []string{"name", "chain_id", "endpoint"}),
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

// EvmHealth implements monitor.MetricsSink — execution-layer telemetry.
func (e *Exporter) EvmHealth(name, chainID, endpoint string, height, lag int64, downSeconds float64, syncing bool) {
	l := prometheus.Labels{"name": name, "chain_id": chainID}
	e.evmHeight.With(l).Set(float64(height))
	e.evmLag.With(l).Set(float64(lag))
	e.evmDown.With(prometheus.Labels{"name": name, "chain_id": chainID, "endpoint": endpoint}).Set(downSeconds)
	sync := 0.0
	if syncing {
		sync = 1
	}
	e.evmSyncing.With(l).Set(sync)
}

// NodeInternals exports per-node consensus internals: mempool backlog and the
// live consensus round.
func (e *Exporter) NodeInternals(name, chainID, endpoint, label string, mempoolTxs, mempoolBytes, consensusRound float64) {
	l := prometheus.Labels{"name": name, "chain_id": chainID, "endpoint": endpoint}
	e.mempoolTxs.With(l).Set(mempoolTxs)
	e.mempoolBytes.With(l).Set(mempoolBytes)
	e.consensusRound.With(l).Set(consensusRound)
}

// EvmInternals exports execution-layer internals: txpool depth and block fullness.
func (e *Exporter) EvmInternals(name, chainID, endpoint string, txpoolPending, txpoolQueued int64, gasUsedRatio float64) {
	l := prometheus.Labels{"name": name, "chain_id": chainID}
	e.evmTxPending.With(l).Set(float64(txpoolPending))
	e.evmTxQueued.With(l).Set(float64(txpoolQueued))
	e.evmGasRatio.With(l).Set(gasUsedRatio)
}

// NodeSysstats exports host stats scraped off the node's own metrics endpoint.
func (e *Exporter) NodeSysstats(name, chainID, endpoint string, cpuPct, memBytes float64) {
	l := prometheus.Labels{"name": name, "chain_id": chainID, "endpoint": endpoint}
	e.nodeCPUPct.With(l).Set(cpuPct)
	e.nodeMemBytes.With(l).Set(memBytes)
}

// NotifyResult records one notifier delivery outcome. Wire it to the alert
// engine's observer so cometduty_notify_total tracks every attempt — a
// destination that silently fails is the worst kind of monitoring bug.
func (e *Exporter) NotifyResult(dest string, ok bool) {
	res := "ok"
	if !ok {
		res = "error"
	}
	e.notifyResults.With(prometheus.Labels{"dest": dest, "result": res}).Inc()
}

// Serve starts the metrics HTTP server. ready, when non-nil, backs /readyz —
// a readiness probe that reports whether the monitor is actually observing
// blocks (vs /healthz which only proves the process is up). It returns on
// shutdown or fatal error.
func Serve(ctxDone <-chan struct{}, bind string, port int, ready func() (bool, []string)) error {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	if ready != nil {
		mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
			ok, reasons := ready()
			if ok {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("ready"))
				return
			}
			w.WriteHeader(http.StatusServiceUnavailable)
			for _, why := range reasons {
				_, _ = w.Write([]byte(why + "\n"))
			}
		})
	}
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

func (e *Exporter) ValidatorState(name, chainID, validator, moniker string, jailed, tombstoned, bonded bool, jailedUntil int64, bondedTokens float64) {
	l := prometheus.Labels{"name": name, "chain_id": chainID, "validator": validator, "moniker": moniker}
	b2f := func(b bool) float64 {
		if b {
			return 1
		}
		return 0
	}
	e.valJailed.With(l).Set(b2f(jailed))
	e.valTombstoned.With(l).Set(b2f(tombstoned))
	e.valBonded.With(l).Set(b2f(bonded))
	e.valJailedUntil.With(l).Set(float64(jailedUntil))
	e.valTokens.With(l).Set(bondedTokens)
}

func (e *Exporter) VotingPower(name, chainID, validator, moniker string, power, proposerPriority int64) {
	l := prometheus.Labels{"name": name, "chain_id": chainID, "validator": validator, "moniker": moniker}
	e.valPower.With(l).Set(float64(power))
	e.valPriority.With(l).Set(float64(proposerPriority))
}

func (e *Exporter) NodeInfo(name, chainID, endpoint, moniker, version, network string) {
	e.nodeInfo.With(prometheus.Labels{
		"name": name, "chain_id": chainID, "endpoint": endpoint,
		"moniker": moniker, "version": version, "network": network,
	}).Set(1)
}

func (e *Exporter) EvmGasPrice(name, chainID, endpoint string, wei float64) {
	e.evmGasPrice.With(prometheus.Labels{"name": name, "chain_id": chainID, "endpoint": endpoint}).Set(wei)
}
