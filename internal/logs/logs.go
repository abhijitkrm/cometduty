// Package logs tails a node's container/file logs and matches lines against
// alert rules — the "page me on CONSENSUS FAILURE" layer. It is deliberately
// NOT a log shipper: full log retention and search belong to Loki (or an
// existing log platform); cometduty only watches for known bad patterns.
package logs

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"github.com/abhijitkrm/cometduty/internal/config"
)

// Source is a tail of new log lines. Stream blocks until ctx ends or the
// source breaks; the caller reconnects — sources keep their own position.
type Source interface {
	Stream(ctx context.Context, out chan<- string) error
	String() string
}

// NewSource builds the configured source.
func NewSource(lc *config.LogConfig) (Source, error) {
	switch lc.Source {
	case "", "docker":
		return NewDockerSource(lc.DockerHost, lc.Container, lc.Tail)
	case "file":
		return NewFileSource(lc.File)
	default:
		return nil, fmt.Errorf("unknown logs source %q", lc.Source)
	}
}

// Rule is a compiled alert rule: fire when Pattern matches Count lines within
// Window, then stay quiet for Cooldown.
type Rule struct {
	Name     string
	Re       *regexp.Regexp
	Severity string
	Count    int
	Window   time.Duration
	Cooldown time.Duration

	hits      []time.Time // timestamps inside the window
	lastAlert time.Time
}

// Match records a hit and reports whether the rule should fire now. Rolling
// window: hits older than Window are dropped; the alert requires Count hits
// inside it, and re-fires at most every Cooldown while it stays hot.
func (r *Rule) Match(now time.Time) bool {
	r.hits = append(r.hits, now)
	cut := now.Add(-r.Window)
	keep := r.hits[:0]
	for _, t := range r.hits {
		if !t.Before(cut) {
			keep = append(keep, t)
		}
	}
	r.hits = keep
	if len(r.hits) < r.Count {
		return false
	}
	if !r.lastAlert.IsZero() && now.Sub(r.lastAlert) < r.Cooldown {
		return false
	}
	r.lastAlert = now
	return true
}

// Compile turns config rules into executable Rules, applying defaults.
func Compile(rs []config.LogRule) ([]*Rule, error) {
	out := make([]*Rule, 0, len(rs))
	for _, r := range rs {
		if r.Name == "" || r.Pattern == "" {
			continue
		}
		re, err := regexp.Compile(r.Pattern)
		if err != nil {
			return nil, err
		}
		sev, count, win, cool := r.Severity, r.Count, r.WindowMin, r.CooldownMin
		if sev == "" {
			sev = "critical"
		}
		if count <= 0 {
			count = 1
		}
		if win <= 0 {
			win = 5
		}
		if cool <= 0 {
			cool = 30
		}
		out = append(out, &Rule{
			Name: r.Name, Re: re, Severity: sev, Count: count,
			Window: time.Duration(win) * time.Minute, Cooldown: time.Duration(cool) * time.Minute,
		})
	}
	return out, nil
}

// DefaultRules is the built-in Cosmos-EVM / CometBFT pack. These patterns
// are the known page-worthy signatures; operators add their own under
// node logs.rules.
func DefaultRules() []config.LogRule {
	return []config.LogRule{
		{Name: "consensus-failure", Pattern: `CONSENSUS FAILURE`, Severity: "critical"},
		{Name: "apphash-mismatch", Pattern: `wrong Block\.Header\.AppHash|AppHash mismatch`, Severity: "critical"},
		{Name: "upgrade-halt", Pattern: `UPGRADE "[^"]*" NEEDED at height`, Severity: "critical"},
		{Name: "panic", Pattern: `\bpanic:|fatal error:`, Severity: "critical"},
		{Name: "double-sign", Pattern: `double sign|conflicting votes|DuplicateVote`, Severity: "critical"},
		{Name: "signer-error", Pattern: `failed to sign|SignBytes failed|remote signer.*(error|refused|timeout)`, Severity: "critical"},
		{Name: "disk-full", Pattern: `no space left on device`, Severity: "critical"},
		{Name: "fd-exhaustion", Pattern: `too many open files`, Severity: "warning"},
		{Name: "peer-churn", Pattern: `Stopping peer for error`, Severity: "warning", Count: 10, WindowMin: 2},
		{Name: "evm-error", Pattern: `module=evm.*level=error|evm.*panic`, Severity: "warning", Count: 10, WindowMin: 5},
	}
}
