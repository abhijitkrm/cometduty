// Package alert is the notification engine: it decides whether an alert should
// be sent to each destination (dedup, resolve-pairing, flap suppression,
// reminders) and dispatches to notifier implementations.
package alert

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/abhijitkrm/cometduty/internal/config"
)

// Alert is a single notification event.
type Alert struct {
	Chain    string // friendly chain name
	ChainID  string
	Key      string // dedup key: same key pairs an alert with its later resolve
	Message  string
	Severity string // "info", "warning", "critical" (also used as PD severity)
	Resolved bool
	Moniker  string
	Valcons  string
	Time     time.Time

	// Fields carries template variables for webhook/ntfy bodies.
	Fields map[string]string

	// Scoped, when set, is a per-validator alert config that replaces the
	// chain-level policy for destination resolution (validators[].alerts).
	Scoped *config.AlertConfig
}

// ResolvedDest is a destination that applies to this alert plus the
// (already-merged, per-chain-aware) configuration for it.
type ResolvedDest struct {
	Kind string // "slack", "discord", "telegram", "pagerduty", "webhook", "ntfy", "opsgenie"
	Cfg  any
}

// Notifier is a pluggable destination.
type Notifier interface {
	Kind() string
	// Send delivers the alert. cfg is the notifier's config type (per-chain
	// merged with global by the caller).
	Send(ctx context.Context, a *Alert, cfg any) error
	// Test sends a synthetic alert — used by `cometduty test-alert`.
	Test(ctx context.Context, cfg any) error
}

// Engine owns alert state and dispatch.
type Engine struct {
	mu sync.Mutex

	notifiers map[string]Notifier
	resolve   func(a *Alert) []ResolvedDest // supplied by the supervisor

	flapMinutes   time.Duration
	remindMinutes time.Duration

	// sent[destKind][alertKey] = the alert as dispatched (kept so reminders and
	// resolves re-send the identical payload)
	sent map[string]map[string]*sentEntry
	// flapped[destKind][alertKey] = last resolve time, for flap suppression
	flapped map[string]map[string]time.Time
	// active[chain] = set of open alert keys (drives the dashboard count)
	active map[string]map[string]time.Time
}

type sentEntry struct {
	Alert Alert
	When  time.Time
}

// NewEngine builds an engine. resolveFn maps an alert to its concrete
// destinations; flapMin suppresses re-alerts raised again that soon after a
// resolve; remindMin re-notifies on still-open alerts (0 disables).
func NewEngine(resolveFn func(*Alert) []ResolvedDest, flapMin, remindMin time.Duration) *Engine {
	return &Engine{
		notifiers:     map[string]Notifier{},
		resolve:       resolveFn,
		flapMinutes:   flapMin,
		remindMinutes: remindMin,
		sent:          map[string]map[string]*sentEntry{},
		flapped:       map[string]map[string]time.Time{},
		active:        map[string]map[string]time.Time{},
	}
}

// Register adds a notifier implementation.
func (e *Engine) Register(n Notifier) { e.notifiers[n.Kind()] = n }

// SetResolver installs the destination-resolution function. It is separate from
// NewEngine because the resolver (the supervisor) needs the engine to exist.
func (e *Engine) SetResolver(f func(*Alert) []ResolvedDest) { e.resolve = f }

// Dispatch routes an alert through dedup/flap logic and fires enabled
// destinations asynchronously.
func (e *Engine) Dispatch(ctx context.Context, a Alert) {
	if a.Key == "" {
		a.Key = a.Message
	}
	if a.Time.IsZero() {
		a.Time = time.Now()
	}
	dests := e.resolve(&a)

	e.mu.Lock()
	if e.active[a.Chain] == nil {
		e.active[a.Chain] = map[string]time.Time{}
	}
	if a.Resolved {
		delete(e.active[a.Chain], a.Key)
	} else {
		e.active[a.Chain][a.Key] = a.Time
	}
	e.mu.Unlock()

	for _, d := range dests {
		n, ok := e.notifiers[d.Kind]
		if !ok {
			slog.Warn("no notifier registered", "kind", d.Kind)
			continue
		}
		if !e.shouldSend(d.Kind, &a) {
			continue
		}
		e.fire(ctx, n, d, a)
	}
}

// fire invokes a notifier in the background with a timeout.
func (e *Engine) fire(ctx context.Context, n Notifier, d ResolvedDest, a Alert) {
	go func() {
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if err := n.Send(cctx, &a, d.Cfg); err != nil {
			slog.Error("notification failed", "chain", a.Chain, "dest", d.Kind, "err", err)
		}
	}()
}

// shouldSend implements dedup, resolve pairing, and flap suppression. It runs
// under the engine lock via withMu.
func (e *Engine) shouldSend(kind string, a *Alert) bool {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.sent[kind] == nil {
		e.sent[kind] = map[string]*sentEntry{}
	}
	if e.flapped[kind] == nil {
		e.flapped[kind] = map[string]time.Time{}
	}
	_, already := e.sent[kind][a.Key]

	if a.Resolved {
		if !already {
			slog.Debug("suppressing resolve with no open alert", "dest", kind, "key", a.Key)
			return false
		}
		delete(e.sent[kind], a.Key)
		e.flapped[kind][a.Key] = time.Now()
		slog.Info("resolved alert", "chain", a.Chain, "dest", kind, "key", a.Key)
		return true
	}

	if already {
		return false
	}
	// flap suppression: re-alert too soon after a resolve gets squashed
	if last, ok := e.flapped[kind][a.Key]; ok && time.Since(last) < e.flapMinutes {
		slog.Warn("flapping alert suppressed", "chain", a.Chain, "dest", kind, "key", a.Key)
		return false
	}
	e.sent[kind][a.Key] = &sentEntry{Alert: *a, When: time.Now()}
	slog.Info("new alert", "chain", a.Chain, "dest", kind, "key", a.Key, "msg", a.Message)
	return true
}

// HasOpen reports whether any destination still holds an open alert for key.
// Callers use it to skip pointless resolve dispatches (and log noise).
func (e *Engine) HasOpen(key string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, m := range e.sent {
		if _, ok := m[key]; ok {
			return true
		}
	}
	return false
}

// ActiveCount returns open alert count for a chain (dashboard badge).
func (e *Engine) ActiveCount(chain string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.active[chain])
}

// ActiveAlerts lists open alert keys for a chain.
func (e *Engine) ActiveAlerts(chain string) []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, 0, len(e.active[chain]))
	for k := range e.active[chain] {
		out = append(out, k)
	}
	return out
}

// StoredAlert is a sent alert plus when it went out — what the state file
// persists. Keeping the payload lets reminders resend after a restart and
// lets resolve_alerts_on_start close downstream incidents.
type StoredAlert struct {
	Alert Alert     `json:"alert"`
	When  time.Time `json:"when"`
}

// Snapshot exports dedup state for persistence.
func (e *Engine) Snapshot() map[string]map[string]StoredAlert {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := map[string]map[string]StoredAlert{}
	for kind, m := range e.sent {
		out[kind] = map[string]StoredAlert{}
		for k, v := range m {
			out[kind][k] = StoredAlert{Alert: v.Alert, When: v.When}
		}
	}
	return out
}

// Restore imports dedup state, dropping entries older than maxAge.
func (e *Engine) Restore(snap map[string]map[string]StoredAlert, maxAge time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for kind, m := range snap {
		if e.sent[kind] == nil {
			e.sent[kind] = map[string]*sentEntry{}
		}
		for k, sa := range m {
			if time.Since(sa.When) > maxAge {
				slog.Debug("discarding stale restored alarm", "dest", kind, "key", k)
				continue
			}
			a := sa.Alert
			a.Key = k // key is authoritative in case payloads drift
			e.sent[kind][k] = &sentEntry{Alert: a, When: sa.When}
			slog.Debug("restored alarm state", "dest", kind, "key", k)
		}
	}
}

// RunReminders periodically re-sends still-open alerts (when remindMinutes > 0).
// It blocks until ctx is done; run it in a goroutine.
func (e *Engine) RunReminders(ctx context.Context) {
	if e.remindMinutes <= 0 {
		return
	}
	t := time.NewTicker(e.remindMinutes)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// reminders bypass dedup: the alert is open, we intentionally resend
			type dueItem struct {
				n Notifier
				d ResolvedDest
				a Alert
			}
			var due []dueItem
			e.mu.Lock()
			for kind, m := range e.sent {
				n := e.notifiers[kind]
				for _, se := range m {
					if se.Alert.Key == "" {
						continue // restored state has no payload to resend
					}
					if time.Since(se.When) >= e.remindMinutes {
						se.When = time.Now()
						a := se.Alert
						a.Message = fmt.Sprintf("(reminder) %s", a.Message)
						for _, d := range e.resolve(&a) {
							if d.Kind == kind && n != nil {
								due = append(due, dueItem{n: n, d: d, a: a})
							}
						}
					}
				}
			}
			e.mu.Unlock()
			for _, item := range due {
				e.fire(ctx, item.n, item.d, item.a)
			}
		}
	}
}
