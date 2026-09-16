package state

import (
	"os"

	"github.com/cometduty/cometduty/internal/alert"
	"path/filepath"
	"testing"
	"time"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	s := Open(p)

	snap := &Snapshot{
		Alarms: map[string]map[string]alert.StoredAlert{
			"slack": {"k1": {When: time.Now().Truncate(time.Second), Alert: alert.Alert{Chain: "c", Key: "k1", Message: "m"}}},
		},
		Blocks: map[string]map[string][]int{
			"cosmoshub": {"ABCD": {0, 1, 2, 3}},
		},
		NodesDown: map[string]map[string]time.Time{
			"cosmoshub": {"https://rpc": time.Now().Truncate(time.Second)},
		},
	}
	if err := s.Save(snap); err != nil {
		t.Fatal(err)
	}
	got := s.Load()
	if got.Alarms["slack"]["k1"].When.IsZero() {
		t.Error("alarm not restored")
	}
	if len(got.Blocks["cosmoshub"]["ABCD"]) != 4 {
		t.Error("block ring not restored")
	}
	// atomic write must not leave the tmp file
	if _, err := os.Stat(p + ".tmp"); !os.IsNotExist(err) {
		t.Error("tmp file left behind")
	}
	// perms
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("perms %v want 0600", fi.Mode().Perm())
	}
}

func TestLoadMissingIsEmpty(t *testing.T) {
	s := Open(filepath.Join(t.TempDir(), "nope.json"))
	snap := s.Load()
	if snap == nil || len(snap.Alarms) != 0 {
		t.Error("missing file did not yield empty snapshot")
	}
}

func TestLoadCorruptIsEmpty(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	if err := os.WriteFile(p, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	snap := Open(p).Load()
	if len(snap.Alarms) != 0 {
		t.Error("corrupt file did not yield empty snapshot")
	}
}
