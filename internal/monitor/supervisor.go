package monitor

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/abhijitkrm/cometduty/internal/alert"
	"github.com/abhijitkrm/cometduty/internal/config"
)

// Supervisor owns all chains and resolves alert destinations.
type Supervisor struct {
	cfg       *config.Config
	eng       *alert.Engine
	met       MetricsSink
	parentCtx context.Context

	mu     sync.Mutex
	chains map[string]*chainRunner
}

type chainRunner struct {
	chain  *Chain
	cancel context.CancelFunc
}

// NewSupervisor builds the supervisor and wires the engine's destination
// resolver to the (reloadable) config.
func NewSupervisor(cfg *config.Config, eng *alert.Engine, met MetricsSink) *Supervisor {
	s := &Supervisor{cfg: cfg, eng: eng, met: met, chains: map[string]*chainRunner{}}
	return s
}

// ResolveDestinations maps an alert to concrete notifier destinations using the
// chain's alert config merged over the global config.
func (s *Supervisor) ResolveDestinations(a *alert.Alert) []alert.ResolvedDest {
	s.mu.Lock()
	cfg := s.cfg
	s.mu.Unlock()
	cc := cfg.Chains[a.Chain]
	if cc == nil {
		return nil
	}
	// per-validator alerts: block (attached by the monitor) overrides the
	// chain-level destination policy
	ac := cc.Alerts
	if a.Scoped != nil {
		ac = *a.Scoped
	}
	var out []alert.ResolvedDest

	if cfg.Pagerduty.Enabled && ac.Pagerduty.Enabled {
		pd := ac.Pagerduty
		if pd.APIKey == "" {
			pd.APIKey = cfg.Pagerduty.APIKey
		}
		if pd.DefaultSeverity == "" {
			pd.DefaultSeverity = cfg.Pagerduty.DefaultSeverity
		}
		out = append(out, alert.ResolvedDest{Kind: "pagerduty", Cfg: pd})
	}
	if cfg.Discord.Enabled && ac.Discord.Enabled {
		d := ac.Discord
		if d.Webhook == "" {
			d.Webhook = cfg.Discord.Webhook
		}
		if len(d.Mentions) == 0 {
			d.Mentions = cfg.Discord.Mentions
		}
		out = append(out, alert.ResolvedDest{Kind: "discord", Cfg: d})
	}
	if cfg.Telegram.Enabled && ac.Telegram.Enabled {
		t := ac.Telegram
		if t.APIKey == "" {
			t.APIKey = cfg.Telegram.APIKey
		}
		if t.Channel == "" {
			t.Channel = cfg.Telegram.Channel
		}
		if len(t.Mentions) == 0 {
			t.Mentions = cfg.Telegram.Mentions
		}
		out = append(out, alert.ResolvedDest{Kind: "telegram", Cfg: t})
	}
	if cfg.Slack.Enabled && ac.Slack.Enabled {
		sl := ac.Slack
		if sl.Webhook == "" {
			sl.Webhook = cfg.Slack.Webhook
		}
		if len(sl.Mentions) == 0 {
			sl.Mentions = cfg.Slack.Mentions
		}
		out = append(out, alert.ResolvedDest{Kind: "slack", Cfg: sl})
	}
	if cfg.Webhook.Enabled && ac.Webhook.Enabled {
		w := ac.Webhook
		if w.URL == "" {
			w = cfg.Webhook
		}
		out = append(out, alert.ResolvedDest{Kind: "webhook", Cfg: w})
	}
	if cfg.Ntfy.Enabled && ac.Ntfy.Enabled {
		n := ac.Ntfy
		if n.URL == "" {
			n = cfg.Ntfy
		}
		out = append(out, alert.ResolvedDest{Kind: "ntfy", Cfg: n})
	}
	if cfg.Opsgenie.Enabled && ac.Opsgenie.Enabled {
		o := ac.Opsgenie
		if o.APIKey == "" {
			o = cfg.Opsgenie
		}
		out = append(out, alert.ResolvedDest{Kind: "opsgenie", Cfg: o})
	}
	return out
}

// Start launches a monitor for every configured chain.
func (s *Supervisor) Start(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.parentCtx = ctx
	for name, cc := range s.cfg.Chains {
		s.startLocked(ctx, name, cc)
	}
}

func (s *Supervisor) startLocked(ctx context.Context, name string, cc *config.ChainConfig) {
	rootFn := func() *config.Config {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.cfg
	}
	ch := NewChain(name, cc, rootFn, s.eng, s.met)
	cctx, cancel := context.WithCancel(ctx)
	s.chains[name] = &chainRunner{chain: ch, cancel: cancel}
	go ch.Run(cctx)
	slog.Info("started chain monitor", "chain", name, "chain_id", cc.ChainID)
}

// Reload diffs a new config against running chains: starts added, stops
// removed, and updates existing chains in place.
func (s *Supervisor) Reload(newCfg *config.Config) {
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.cfg
	s.cfg = newCfg
	for name := range s.chains {
		if newCfg.Chains[name] == nil {
			s.chains[name].cancel()
			delete(s.chains, name)
			slog.Info("stopped chain monitor (removed from config)", "chain", name)
		}
	}
	for name, cc := range newCfg.Chains {
		if r, ok := s.chains[name]; ok {
			r.chain.Update(cc)
		} else {
			s.startLocked(s.parentCtx, name, cc)
		}
	}
	_ = old
}

// Stop halts all chains.
func (s *Supervisor) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.chains {
		r.cancel()
	}
}

// SnapshotNodesDown returns map[chain][nodeURL]downSince for down nodes.
func (s *Supervisor) SnapshotNodesDown() map[string]map[string]time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]map[string]time.Time{}
	for name, r := range s.chains {
		r.chain.mu.Lock()
		for _, n := range r.chain.nodes {
			if n.down && !n.downSince.IsZero() {
				if out[name] == nil {
					out[name] = map[string]time.Time{}
				}
				out[name][n.cfg.URL] = n.downSince
			}
		}
		r.chain.mu.Unlock()
	}
	return out
}
