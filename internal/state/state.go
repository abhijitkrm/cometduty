// Package state persists dedup/alarm state and per-validator block rings to a
// JSON file. Writes are atomic (tmp + rename) and happen periodically as well as
// on shutdown, so a crash doesn't cause alert floods on restart.
package state

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/cometduty/cometduty/internal/alert"
)

// Snapshot is the serialized state.
type Snapshot struct {
	Alarms    map[string]map[string]alert.StoredAlert `json:"alarms"`     // dest kind -> key -> sent alert
	Blocks    map[string]map[string][]int             `json:"blocks"`     // chain -> valcons -> ring
	NodesDown map[string]map[string]time.Time         `json:"nodes_down"` // chain -> url -> since
}

// Store reads/writes the state file.
type Store struct {
	path string
}

// Open returns a store for path.
func Open(path string) *Store { return &Store{path: path} }

// Load reads the snapshot; returns an empty snapshot on any error.
func (s *Store) Load() *Snapshot {
	f, err := os.Open(s.path) //nolint:gosec -- operator-provided path
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("could not open state file", "path", s.path, "err", err)
		}
		return &Snapshot{}
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, 16<<20))
	if err != nil {
		slog.Warn("could not read state file", "err", err)
		return &Snapshot{}
	}
	snap := &Snapshot{}
	if err := json.Unmarshal(b, snap); err != nil {
		slog.Warn("could not parse state file, starting fresh", "err", err)
		return &Snapshot{}
	}
	return snap
}

// Save writes the snapshot atomically with 0600 perms.
func (s *Store) Save(snap *Snapshot) error {
	b, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// StartPeriodic saves collect() output every interval until ctx ends.
func (s *Store) StartPeriodic(ctx context.Context, interval time.Duration, collect func() *Snapshot) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.Save(collect()); err != nil {
				slog.Warn("periodic state save failed", "err", err)
			}
		}
	}
}

// Base returns the base name (useful for tests).
func (s *Store) Base() string { return filepath.Base(s.path) }
