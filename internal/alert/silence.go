package alert

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"time"
)

// MaxSilence caps how long one silence may last: a forgotten silence is a
// monitor that has gone blind.
const MaxSilence = 7 * 24 * time.Hour

// Matcher selects alerts by one label, Alertmanager-style. IsEqual nil means
// true (Alertmanager's default).
type Matcher struct {
	Name    string `json:"name"`
	Value   string `json:"value"`
	IsRegex bool   `json:"isRegex"`
	IsEqual *bool  `json:"isEqual,omitempty"`

	re *regexp.Regexp
}

func (m *Matcher) equal() bool { return m.IsEqual == nil || *m.IsEqual }

func (m *Matcher) compile() error {
	if m.Name == "" {
		return errors.New("matcher without a name")
	}
	if !m.IsRegex {
		return nil
	}
	re, err := regexp.Compile("^(?:" + m.Value + ")$")
	if err != nil {
		return fmt.Errorf("matcher %s: %w", m.Name, err)
	}
	m.re = re
	return nil
}

func (m *Matcher) matches(labels map[string]string) bool {
	v := labels[m.Name]
	var hit bool
	if m.IsRegex {
		hit = m.re != nil && m.re.MatchString(v)
	} else {
		hit = v == m.Value
	}
	return hit == m.equal()
}

// Silence mutes the alerts its matchers all select between StartsAt and EndsAt.
// Raises are held, not dropped: one still open when the silence ends is sent then.
type Silence struct {
	ID        string    `json:"id"`
	Matchers  []Matcher `json:"matchers"`
	StartsAt  time.Time `json:"startsAt"`
	EndsAt    time.Time `json:"endsAt"`
	CreatedBy string    `json:"createdBy"`
	Comment   string    `json:"comment"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// State is Alertmanager's silence state: pending, active or expired.
func (s *Silence) State(now time.Time) string {
	switch {
	case now.Before(s.StartsAt):
		return "pending"
	case now.Before(s.EndsAt):
		return "active"
	}
	return "expired"
}

func (s *Silence) mutes(labels map[string]string, now time.Time) bool {
	if s.State(now) != "active" {
		return false
	}
	for i := range s.Matchers {
		if !s.Matchers[i].matches(labels) {
			return false
		}
	}
	return true
}

// Labels are the names a silence matcher can use: the alert's own fields, the
// operator's labels from the config, and alertname (= kind) for Alertmanager tools.
func (a *Alert) LabelSet() map[string]string {
	l := map[string]string{}
	for k, v := range a.Labels {
		l[k] = v
	}
	for k, v := range map[string]string{
		"alertname": a.Kind, "kind": a.Kind, "target": a.Target, "node": a.Node,
		"chain": a.Chain, "chain_id": a.ChainID, "moniker": a.Moniker,
		"valcons": a.Valcons, "severity": a.Severity,
	} {
		if v != "" {
			l[k] = v
		}
	}
	return l
}

// AddSilence validates and stores a silence (replacing one with the same id),
// returning its id.
func (e *Engine) AddSilence(s Silence) (string, error) {
	now := time.Now()
	if s.StartsAt.IsZero() {
		s.StartsAt = now
	}
	if !s.EndsAt.After(s.StartsAt) || !s.EndsAt.After(now) {
		return "", errors.New("endsAt must be after startsAt and in the future")
	}
	if s.EndsAt.Sub(s.StartsAt) > MaxSilence {
		return "", fmt.Errorf("a silence may last at most %s", MaxSilence)
	}
	if len(s.Matchers) == 0 {
		return "", errors.New("at least one matcher is required")
	}
	selective := false
	for i := range s.Matchers {
		if err := s.Matchers[i].compile(); err != nil {
			return "", err
		}
		// Alertmanager's rule: some matcher must not match the empty string,
		// or the silence would mute every alert.
		if !s.Matchers[i].matches(map[string]string{}) {
			selective = true
		}
	}
	if !selective {
		return "", errors.New("the matchers would silence every alert — add one that selects")
	}
	if s.ID == "" {
		b := make([]byte, 8)
		_, _ = rand.Read(b)
		s.ID = hex.EncodeToString(b)
	}
	s.UpdatedAt = now
	e.mu.Lock()
	e.silences[s.ID] = &s
	e.mu.Unlock()
	slog.Info("silence added", "id", s.ID, "until", s.EndsAt.Format(time.RFC3339), "by", s.CreatedBy, "comment", s.Comment)
	return s.ID, nil
}

// ExpireSilence ends a silence now and releases what it held.
func (e *Engine) ExpireSilence(ctx context.Context, id string) error {
	e.mu.Lock()
	s, ok := e.silences[id]
	if ok {
		now := time.Now()
		if s.EndsAt.After(now) {
			s.EndsAt, s.UpdatedAt = now, now
		}
	}
	e.mu.Unlock()
	if !ok {
		return fmt.Errorf("no silence %s", id)
	}
	slog.Info("silence expired", "id", id)
	e.releaseHeld(ctx)
	return nil
}

// Silences lists every silence, newest first.
func (e *Engine) Silences() []Silence {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Silence, 0, len(e.silences))
	for _, s := range e.silences {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out
}

// ActiveSilences counts silences in effect now.
func (e *Engine) ActiveSilences() int {
	now := time.Now()
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, s := range e.silences {
		if s.State(now) == "active" {
			n++
		}
	}
	return n
}

// RestoreSilences loads persisted silences, dropping ones long expired.
func (e *Engine) RestoreSilences(list []Silence) {
	for _, s := range list {
		if time.Since(s.EndsAt) > 24*time.Hour {
			continue
		}
		ok := true
		for i := range s.Matchers {
			if s.Matchers[i].compile() != nil {
				ok = false
			}
		}
		if ok {
			s := s
			e.mu.Lock()
			e.silences[s.ID] = &s
			e.mu.Unlock()
		}
	}
}

// silencedLocked reports whether an active silence mutes a. Caller holds e.mu.
func (e *Engine) silencedLocked(a *Alert) bool {
	if len(e.silences) == 0 {
		return false
	}
	now, labels := time.Now(), a.LabelSet()
	for _, s := range e.silences {
		if s.mutes(labels, now) {
			return true
		}
	}
	return false
}

// RunSilences drops long-expired silences and sends held alerts that are
// still open once nothing mutes them. Blocks until ctx is done.
func (e *Engine) RunSilences(ctx context.Context) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			e.mu.Lock()
			for id, s := range e.silences {
				if time.Since(s.EndsAt) > 24*time.Hour {
					delete(e.silences, id)
				}
			}
			e.mu.Unlock()
			e.releaseHeld(ctx)
		}
	}
}

func (e *Engine) releaseHeld(ctx context.Context) {
	var due []Alert
	e.mu.Lock()
	for key, a := range e.held {
		if e.silencedLocked(&a) {
			continue
		}
		delete(e.held, key)
		if _, open := e.active[a.Chain][key]; open {
			due = append(due, a)
		}
	}
	e.mu.Unlock()
	for _, a := range due {
		slog.Info("silence over — sending held alert", "key", a.Key)
		e.Dispatch(ctx, a)
	}
}

// splitKey derives an alert's kind and target from its key ("node-down:<url>").
func splitKey(key string) (kind, target string) {
	kind, target, _ = strings.Cut(key, ":")
	return kind, target
}
