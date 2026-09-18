// Package history provides an append-only JSONL record of alert delivery
// outcomes — one line per notifier attempt. Unlike the statefile (which only
// tracks currently-open alerts for dedup), this is the durable audit trail:
// "did the page go out, when, where, did it fail".
//
// Rotate with standard tooling (logrotate, etc.); records are small (~300B)
// and only written on delivery attempts.
package history

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/abhijitkrm/cometduty/internal/alert"
)

// Rec is one JSONL line. OK=false rows still record the attempt count and
// error so failures are auditable rather than invisible.
type Rec struct {
	Ts       time.Time `json:"ts"`
	Chain    string    `json:"chain"`
	ChainID  string    `json:"chain_id"`
	Key      string    `json:"key"`
	Resolved bool      `json:"resolved"`
	Severity string    `json:"severity"`
	Message  string    `json:"message"`
	Moniker  string    `json:"moniker,omitempty"`
	Valcons  string    `json:"valcons,omitempty"`
	Dest     string    `json:"dest"`
	OK       bool      `json:"ok"`
	Attempts int       `json:"attempts"`
	Err      string    `json:"err,omitempty"`
}

// FromNotify converts an engine delivery result into a record.
func FromNotify(r alert.NotifyResult) Rec {
	rec := Rec{
		Ts:       time.Now().UTC(),
		Chain:    r.Alert.Chain,
		ChainID:  r.Alert.ChainID,
		Key:      r.Alert.Key,
		Resolved: r.Alert.Resolved,
		Severity: r.Alert.Severity,
		Message:  r.Alert.Message,
		Moniker:  r.Alert.Moniker,
		Valcons:  r.Alert.Valcons,
		Dest:     r.Dest,
		OK:       r.Err == nil,
		Attempts: r.Attempts,
	}
	if r.Err != nil {
		rec.Err = r.Err.Error()
	}
	return rec
}

// Sink appends records to a JSONL file. Safe for concurrent use.
type Sink struct {
	mu sync.Mutex
	f  *os.File
}

// Open creates/append-opens path (0600 — it can carry alert payloads).
// An empty path returns a nil sink — history disabled.
func Open(path string) (*Sink, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("alert log %s: %w", path, err)
	}
	return &Sink{f: f}, nil
}

// Record writes one line. Errors are returned to the caller (logged upstream);
// the sink stays usable after a failed write.
func (s *Sink) Record(r Rec) error {
	if s == nil {
		return nil
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err = s.f.Write(append(b, '\n'))
	return err
}

func (s *Sink) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.f.Close()
}
