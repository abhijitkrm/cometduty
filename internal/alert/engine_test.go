package alert

import (
	"context"
	"sync"
	"testing"
	"time"
)

// fakeNotifier records every alert it is asked to send.
type fakeNotifier struct {
	kind string
	mu   sync.Mutex
	got  []Alert
}

func (f *fakeNotifier) Kind() string { return f.kind }
func (f *fakeNotifier) Send(_ context.Context, a *Alert, _ any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, *a)
	return nil
}
func (f *fakeNotifier) Test(context.Context, any) error { return nil }
func (f *fakeNotifier) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.got)
}
func (f *fakeNotifier) last() Alert {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.got[len(f.got)-1]
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func newTestEngine(fn *fakeNotifier, flapMin time.Duration) *Engine {
	e := NewEngine(func(*Alert) []ResolvedDest {
		return []ResolvedDest{{Kind: fn.kind, Cfg: nil}}
	}, flapMin, 0)
	e.Register(fn)
	return e
}

func alertMsg(key, msg string) Alert {
	return Alert{Chain: "chain", ChainID: "chain-1", Key: key, Message: msg, Severity: "warning"}
}

func TestDedupSuppressesRepeatAlert(t *testing.T) {
	fn := &fakeNotifier{kind: "fake"}
	e := newTestEngine(fn, 10*time.Minute)
	ctx := context.Background()

	e.Dispatch(ctx, alertMsg("k1", "val missed 5 blocks"))
	e.Dispatch(ctx, alertMsg("k1", "val missed 5 blocks"))
	e.Dispatch(ctx, alertMsg("k1", "val missed 5 blocks"))
	waitFor(t, "first send", func() bool { return fn.count() >= 1 })
	time.Sleep(50 * time.Millisecond) // let any erroneous extra sends land
	if fn.count() != 1 {
		t.Fatalf("dedup failed: %d sends", fn.count())
	}
}

func TestResolvePairsAndResends(t *testing.T) {
	fn := &fakeNotifier{kind: "fake"}
	e := newTestEngine(fn, 10*time.Minute)
	ctx := context.Background()

	// resolve with no open alert → suppressed
	a := alertMsg("k2", "node down")
	a.Resolved = true
	e.Dispatch(ctx, a)
	time.Sleep(50 * time.Millisecond)
	if fn.count() != 0 {
		t.Fatal("resolve without open alert was sent")
	}

	// open then resolve → two sends
	e.Dispatch(ctx, alertMsg("k2", "node down"))
	waitFor(t, "alert send", func() bool { return fn.count() == 1 })
	e.Dispatch(ctx, a)
	waitFor(t, "resolve send", func() bool { return fn.count() == 2 })
	if !fn.last().Resolved {
		t.Error("resolve notification not marked resolved")
	}
}

func TestFlapSuppression(t *testing.T) {
	fn := &fakeNotifier{kind: "fake"}
	e := newTestEngine(fn, time.Hour)
	ctx := context.Background()

	e.Dispatch(ctx, alertMsg("k3", "flapping"))
	a := alertMsg("k3", "flapping")
	a.Resolved = true
	e.Dispatch(ctx, a)
	waitFor(t, "resolve", func() bool { return fn.count() == 2 })

	// re-alert inside flap window → suppressed
	e.Dispatch(ctx, alertMsg("k3", "flapping"))
	time.Sleep(60 * time.Millisecond)
	if fn.count() != 2 {
		t.Fatalf("flap suppression failed: %d sends", fn.count())
	}

	// move the flap mark back beyond the window → next alert goes through
	e.mu.Lock()
	e.flapped["fake"]["k3"] = time.Now().Add(-2 * time.Hour)
	e.mu.Unlock()
	e.Dispatch(ctx, alertMsg("k3", "flapping"))
	waitFor(t, "post-flap send", func() bool { return fn.count() == 3 })
}

func TestRemindersBypassDedup(t *testing.T) {
	fn := &fakeNotifier{kind: "fake"}
	e := newTestEngine(fn, time.Hour)
	e.remindMinutes = 40 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	e.Dispatch(ctx, alertMsg("k4", "still down"))
	waitFor(t, "initial send", func() bool { return fn.count() == 1 })
	go e.RunReminders(ctx)
	waitFor(t, "reminder send", func() bool { return fn.count() >= 2 })
	if got := fn.last().Message; got[:10] != "(reminder)" {
		t.Errorf("reminder not prefixed: %q", got)
	}
}

func TestActiveCountAndAlerts(t *testing.T) {
	fn := &fakeNotifier{kind: "fake"}
	e := newTestEngine(fn, 0)
	ctx := context.Background()

	e.Dispatch(ctx, alertMsg("a", "one"))
	e.Dispatch(ctx, alertMsg("b", "two"))
	if n := e.ActiveCount("chain"); n != 2 {
		t.Fatalf("active=%d want 2", n)
	}
	r := alertMsg("a", "one")
	r.Resolved = true
	e.Dispatch(ctx, r)
	if n := e.ActiveCount("chain"); n != 1 {
		t.Fatalf("after resolve active=%d want 1", n)
	}

}

func TestSnapshotRestore(t *testing.T) {
	fn := &fakeNotifier{kind: "fake"}
	e := newTestEngine(fn, 0)
	e.Dispatch(context.Background(), alertMsg("k5", "down"))
	waitFor(t, "send", func() bool { return fn.count() == 1 })

	snap := e.Snapshot()
	if len(snap["fake"]) != 1 {
		t.Fatalf("snapshot: %v", snap)
	}

	// restore into a fresh engine: a duplicate alert must be suppressed
	e2 := newTestEngine(fn, 0)
	e2.Restore(snap, time.Hour)
	e2.Dispatch(context.Background(), alertMsg("k5", "down"))
	time.Sleep(50 * time.Millisecond)
	if fn.count() != 1 {
		t.Fatal("restored dedup state did not suppress repeat alert")
	}

	// a restored alert can be resolved after restart (resolve_alerts_on_start)
	r := alertMsg("k5", "down")
	r.Resolved = true
	e2.Dispatch(context.Background(), r)
	waitFor(t, "resolve of restored alert", func() bool { return fn.count() == 2 })

	// stale entries are dropped
	stale := map[string]map[string]StoredAlert{
		"fake": {"old": {When: time.Now().Add(-72 * time.Hour)}},
	}
	e3 := newTestEngine(fn, 0)
	e3.Restore(stale, time.Hour)
	e3.Dispatch(context.Background(), alertMsg("old", "ancient"))
	waitFor(t, "stale key alerts fresh", func() bool { return fn.count() == 3 })
}

// Different keys are independent.
func TestDifferentKeysNotDeduped(t *testing.T) {
	fn := &fakeNotifier{kind: "fake"}
	e := newTestEngine(fn, 0)
	ctx := context.Background()
	e.Dispatch(ctx, alertMsg("x1", "one"))
	e.Dispatch(ctx, alertMsg("x2", "two"))
	waitFor(t, "two sends", func() bool { return fn.count() == 2 })
}
