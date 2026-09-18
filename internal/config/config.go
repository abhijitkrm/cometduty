// Package config loads and validates cometduty configuration. The YAML schema
// stays compatible with tenderduty v2 where practical (same field names), while
// adding multi-validator support, new notifiers, per-node auth headers, and
// lag-in-blocks alerting.
package config

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"time"
)

// Config is the root configuration object.
type Config struct {

	// NodeDownMin is how long a node must be unreachable before alerting.
	NodeDownMin      int    `yaml:"node_down_alert_minutes"`
	NodeDownSeverity string `yaml:"node_down_alert_severity"`

	// Prometheus exporter.
	PrometheusEnabled    bool   `yaml:"prometheus_enabled"`
	PrometheusListenPort int    `yaml:"prometheus_listen_port"`
	PrometheusBind       string `yaml:"prometheus_bind"` // new: defaults to 0.0.0.0

	// Global notifier settings. A notifier must be enabled here AND on the
	// chain's alert config for notifications to flow.
	Pagerduty PagerdutyConfig `yaml:"pagerduty"`
	Discord   DiscordConfig   `yaml:"discord"`
	Telegram  TelegramConfig  `yaml:"telegram"`
	Slack     SlackConfig     `yaml:"slack"`
	Webhook   WebhookConfig   `yaml:"webhook"`  // new: generic JSON webhook
	Ntfy      NtfyConfig      `yaml:"ntfy"`     // new: ntfy.sh / self-hosted push
	Opsgenie  OpsgenieConfig  `yaml:"opsgenie"` // new

	// Dead-man's-switch ping for the monitor itself.
	Healthcheck HealthcheckConfig `yaml:"healthcheck"`

	// Alerting behaviour knobs.
	FlapMinutes    int  `yaml:"flap_suppression_minutes"` // new: suppress re-alerts this soon after resolve
	RemindMinutes  int  `yaml:"reminder_minutes"`         // new: re-notify while unresolved; 0 = off
	ResolveOnStart bool `yaml:"resolve_alerts_on_start"`  // new: treat restart as fresh (don't resend)

	// Chains to monitor, keyed by a friendly name.
	Chains map[string]*ChainConfig `yaml:"chains"`
}

// PagerdutyConfig holds V2 Events API settings.
type PagerdutyConfig struct {
	Enabled         bool   `yaml:"enabled"`
	APIKey          string `yaml:"api_key"`
	DefaultSeverity string `yaml:"default_severity"`
}

// DiscordConfig holds a channel webhook and mention targets.
type DiscordConfig struct {
	Enabled  bool     `yaml:"enabled"`
	Webhook  string   `yaml:"webhook"`
	Mentions []string `yaml:"mentions"`
}

// TelegramConfig holds bot credentials.
type TelegramConfig struct {
	Enabled  bool     `yaml:"enabled"`
	APIKey   string   `yaml:"api_key"`
	Channel  string   `yaml:"channel"`
	Mentions []string `yaml:"mentions"`
}

// SlackConfig holds an incoming-webhook URL and mentions.
type SlackConfig struct {
	Enabled  bool     `yaml:"enabled"`
	Webhook  string   `yaml:"webhook"`
	Mentions []string `yaml:"mentions"`
}

// WebhookConfig is a generic JSON webhook with a Go text/template body. The
// template is rendered with the alert fields (see notifiers/webhook.go for the
// available keys). Empty TemplateBody uses a sensible default payload.
type WebhookConfig struct {
	Enabled      bool              `yaml:"enabled"`
	URL          string            `yaml:"url"`
	Headers      map[string]string `yaml:"headers"`
	TemplateBody string            `yaml:"template_body"` // Go template producing the POST body
	Method       string            `yaml:"method"`        // default POST
}

// NtfyConfig posts to an ntfy.sh-compatible topic.
type NtfyConfig struct {
	Enabled  bool   `yaml:"enabled"`
	URL      string `yaml:"url"`      // e.g. https://ntfy.sh or self-hosted
	Topic    string `yaml:"topic"`    // required
	Priority string `yaml:"priority"` // e.g. "high", "urgent"
	Token    string `yaml:"token"`    // optional bearer token
}

// OpsgenieConfig posts alerts to the Opsgenie v2 alerts API.
type OpsgenieConfig struct {
	Enabled bool   `yaml:"enabled"`
	APIKey  string `yaml:"api_key"`
	APIURL  string `yaml:"api_url"` // default https://api.opsgenie.com
	Team    string `yaml:"team"`
}

// HealthcheckConfig pings a URL on an interval so external services can detect
// a dead monitor.
type HealthcheckConfig struct {
	Enabled  bool          `yaml:"enabled"`
	PingURL  string        `yaml:"ping_url"`
	PingRate time.Duration `yaml:"ping_rate"` // seconds
}

// ChainConfig describes one chain and the validators to watch on it.
type ChainConfig struct {
	ChainID string `yaml:"chain_id"`

	// Single-validator shorthand (v2 compat). If Validators is also set, both
	// are used.
	ValoperAddress  string `yaml:"valoper_address"`
	ValconsOverride string `yaml:"valcons_override"` // skip pubkey lookup; give the valcons address directly
	ConsPrefix      string `yaml:"cons_prefix"`      // explicit bech32 prefix for the derived valcons address
	Label           string `yaml:"label"`            // display label for the single-validator form

	// Multi-validator monitoring on the same chain.
	Validators []ValidatorConfig `yaml:"validators"`

	// SlashingEnabled lets users disable slashing-module queries for chains
	// without x/slashing (e.g. beacon-kit chains). nil means auto-detect.
	SlashingEnabled *bool `yaml:"slashing_enabled"`

	// Alerts is the per-chain alert policy.
	Alerts AlertConfig `yaml:"alerts"`

	// PublicFallback allows using chain-registry public RPCs when all
	// configured nodes fail. Not recommended for paging paths.
	PublicFallback bool `yaml:"public_fallback"`

	// EvmRPC is an optional EVM JSON-RPC endpoint for the same chain. When set,
	// the monitor also watches execution-layer health: evm block height vs
	// consensus height (execution lag), syncing, and reachability.
	EvmRPC string `yaml:"evm_rpc"`

	// Nodes are the RPC endpoints to use, tried in order.
	Nodes []*NodeConfig `yaml:"nodes"`

	// internal bookkeeping
	Name string `yaml:"-"`
}

// ValidatorConfig is one validator target within a chain.
type ValidatorConfig struct {
	ValoperAddress  string       `yaml:"valoper_address"`
	ValconsOverride string       `yaml:"valcons_override"`
	ConsPrefix      string       `yaml:"cons_prefix"`
	Label           string       `yaml:"label"`
	Alerts          *AlertConfig `yaml:"alerts"` // optional per-validator overrides
}

// AlertConfig is the alert policy for a chain or validator.
type AlertConfig struct {
	StalledMinutes int  `yaml:"stalled_minutes"`
	StalledEnabled bool `yaml:"stalled_enabled"`

	ConsecutiveMissed   int    `yaml:"consecutive_missed"`
	ConsecutivePriority string `yaml:"consecutive_priority"`
	ConsecutiveEnabled  bool   `yaml:"consecutive_enabled"`

	WindowPct          int    `yaml:"percentage_missed"`
	PercentagePriority string `yaml:"percentage_priority"`
	PercentageEnabled  bool   `yaml:"percentage_enabled"`

	AlertIfInactive  bool `yaml:"alert_if_inactive"`
	AlertIfNoServers bool `yaml:"alert_if_no_servers"`

	// New: alert when a configured RPC node falls more than lag_blocks behind.
	LagBlocks  int  `yaml:"lag_blocks"`
	LagEnabled bool `yaml:"lag_enabled"`

	// EVM execution-layer alerts (only meaningful when chain evm_rpc is set).
	EvmLagBlocks   int  `yaml:"evm_lag_blocks"` // consensus height minus evm height
	EvmLagEnabled  bool `yaml:"evm_lag_enabled"`
	EvmDownEnabled bool `yaml:"evm_down_enabled"` // evm_rpc unreachable

	// Consensus/EVM internals. Metrics are always exported; a nonzero value
	// here also raises an alert when the threshold is exceeded.
	MempoolTxsAlert      int `yaml:"mempool_txs_alert"`       // node mempool backlog > N txs
	ConsensusRoundAlert  int `yaml:"consensus_round_alert"`   // node consensus round > N (leader churn)
	EvmTxpoolQueuedAlert int `yaml:"evm_txpool_queued_alert"` // evm txpool queued > N (execution wedge)

	// Validator-set watch: alert when ANY validator joins, leaves, or jails —
	// not just the monitored ones.
	SetWatchEnabled bool `yaml:"set_watch_enabled"`

	// Stake movement on monitored validators: alert when bonded tokens shift
	// more than N% between refreshes (delegations, unbondings, slash events).
	// 0 = disabled.
	StakeChangePct int `yaml:"stake_change_pct"`

	// Host stats via the node's own prometheus endpoint (node metrics_url).
	// 0 = disabled.
	CpuPctAlert   int   `yaml:"cpu_pct_alert"`   // cpu seconds/sec > N percent
	MemBytesAlert int64 `yaml:"mem_bytes_alert"` // resident memory > N bytes

	// Per-chain/per-validator destination overrides. The Enabled flag can
	// selectively disable a destination for this scope, and the credential
	// fields fall back to the global values when blank.
	Pagerduty PagerdutyConfig `yaml:"pagerduty"`
	Discord   DiscordConfig   `yaml:"discord"`
	Telegram  TelegramConfig  `yaml:"telegram"`
	Slack     SlackConfig     `yaml:"slack"`
	Webhook   WebhookConfig   `yaml:"webhook"`
	Ntfy      NtfyConfig      `yaml:"ntfy"`
	Opsgenie  OpsgenieConfig  `yaml:"opsgenie"`
}

// NodeConfig is one RPC endpoint.
type NodeConfig struct {
	URL         string            `yaml:"url"`
	Name        string            `yaml:"name"`        // friendly label shown instead of the raw URL
	MetricsURL  string            `yaml:"metrics_url"` // optional: node's own prometheus endpoint for host stats
	AlertIfDown bool              `yaml:"alert_if_down"`
	InsecureTLS bool              `yaml:"insecure_tls"` // allow self-signed certs
	Headers     map[string]string `yaml:"headers"`      // extra HTTP headers, e.g. Authorization
	DisableVote bool              `yaml:"disable_vote"` // don't use this node for vote subscriptions
}

// ValidatorTargets flattens the single-validator shorthand and the Validators
// list into one normalized slice.
func (cc *ChainConfig) ValidatorTargets() []ValidatorConfig {
	out := make([]ValidatorConfig, 0, 1+len(cc.Validators))
	if cc.ValoperAddress != "" || cc.ValconsOverride != "" {
		out = append(out, ValidatorConfig{
			ValoperAddress:  cc.ValoperAddress,
			ValconsOverride: cc.ValconsOverride,
			ConsPrefix:      cc.ConsPrefix,
			Label:           cc.Label,
		})
	}
	out = append(out, cc.Validators...)
	return out
}

var pdOAuthRex = regexp.MustCompile(`[+_-]`)

// Validate performs non-fatal and fatal checks. Returns (fatal, problems).
func Validate(c *Config) (fatal bool, problems []string) {
	if c.PrometheusEnabled && (c.PrometheusListenPort < 1 || c.PrometheusListenPort > 65535) {
		problems = append(problems, "error: prometheus_listen_port is not a valid TCP port")
		fatal = true
	}
	if c.Pagerduty.Enabled && pdOAuthRex.MatchString(c.Pagerduty.APIKey) {
		problems = append(problems, "error: the pagerduty key looks like an OAuth token, not a V2 Events API key")
		fatal = true
	}
	if c.NodeDownMin < 3 {
		problems = append(problems, "warn: node_down_alert_minutes < 3 can cause false alarms")
	}
	if c.Webhook.Enabled && c.Webhook.URL == "" {
		problems = append(problems, "error: webhook enabled but no url configured")
		fatal = true
	}
	if c.Ntfy.Enabled && (c.Ntfy.URL == "" || c.Ntfy.Topic == "") {
		problems = append(problems, "error: ntfy enabled but url or topic missing")
		fatal = true
	}
	if c.Opsgenie.Enabled && c.Opsgenie.APIKey == "" {
		problems = append(problems, "error: opsgenie enabled but api_key missing")
		fatal = true
	}

	names := make([]string, 0, len(c.Chains))
	for name := range c.Chains {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		v := c.Chains[name]
		if v.ChainID == "" {
			problems = append(problems, fmt.Sprintf("error: %s has no chain_id", name))
			fatal = true
		}
		targets := v.ValidatorTargets()
		if len(targets) == 0 {
			problems = append(problems, fmt.Sprintf("warn: %s has no validators configured (valoper_address or validators:)", name))
		}
		for _, t := range targets {
			if t.ValoperAddress == "" && t.ValconsOverride == "" {
				problems = append(problems, fmt.Sprintf("error: %s validator needs valoper_address or valcons_override", name))
				fatal = true
			}
		}
		if len(v.Nodes) == 0 && !v.PublicFallback {
			problems = append(problems, fmt.Sprintf("warn: %s has no nodes and public_fallback is off; it cannot be monitored", name))
		}
		a := v.Alerts
		if !a.ConsecutiveEnabled && !a.PercentageEnabled && !a.AlertIfInactive && !a.AlertIfNoServers && !a.StalledEnabled && !a.LagEnabled {
			problems = append(problems, fmt.Sprintf("warn: %s has no alert types configured", name))
		}
	}
	if len(c.Chains) == 0 {
		problems = append(problems, "error: no chains configured")
		fatal = true
	}
	return fatal, problems
}

// ExpandEnv substitutes ${VAR} and $VAR references in the raw config text,
// letting secrets live in the environment instead of the file.
func ExpandEnv(b []byte) []byte {
	return []byte(os.Expand(string(b), func(key string) string {
		if v, ok := os.LookupEnv(key); ok {
			return v
		}
		return "${" + key + "}" // leave unknown vars untouched for visibility
	}))
}
