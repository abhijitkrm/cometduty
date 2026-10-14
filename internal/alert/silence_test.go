package alert

import (
	"context"
	"testing"
	"time"
)

func silenceFor(name, value string, d time.Duration) Silence {
	return Silence{Matchers: []Matcher{{Name: name, Value: value}}, EndsAt: time.Now().Add(d), CreatedBy: "test"}
}

func TestSilenceHoldsThenReleases(t *testing.T) {
	fn := &fakeNotifier{kind: "fake"}
	e := newTestEngine(fn, 0)
	ctx := context.Background()
	id, err := e.AddSilence(silenceFor("kind", "node-down", time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	e.Dispatch(ctx, alertMsg("node-down:http://a:26657", "node a down"))
	e.Dispatch(ctx, alertMsg("stalled:chain-1", "chain stalled"))
	waitFor(t, "unsilenced alert", func() bool { return fn.count() == 1 })
	if fn.last().Kind != "stalled" || fn.last().Target != "chain-1" {
		t.Fatalf("sent %+v", fn.last())
	}
	if err := e.ExpireSilence(ctx, id); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "held alert sent when the silence ends", func() bool { return fn.count() == 2 })
	if fn.last().Kind != "node-down" {
		t.Fatalf("released %+v", fn.last())
	}
}

func TestResolvedDuringSilenceIsNeverSent(t *testing.T) {
	fn := &fakeNotifier{kind: "fake"}
	e := newTestEngine(fn, 0)
	ctx := context.Background()
	id, _ := e.AddSilence(silenceFor("node", "val1", time.Hour))
	a := alertMsg("node-down:u", "down")
	a.Node = "val1"
	e.Dispatch(ctx, a)
	a.Resolved = true
	e.Dispatch(ctx, a)
	_ = e.ExpireSilence(ctx, id)
	time.Sleep(50 * time.Millisecond)
	if fn.count() != 0 {
		t.Fatalf("planned downtime paged: %+v", fn.got)
	}
}

func TestSilenceMatchesOperatorLabels(t *testing.T) {
	fn := &fakeNotifier{kind: "fake"}
	e := newTestEngine(fn, 0)
	s := silenceFor("profile", "val.*", time.Hour)
	s.Matchers[0].IsRegex = true
	if _, err := e.AddSilence(s); err != nil {
		t.Fatal(err)
	}
	a := alertMsg("disk-low:u", "disk")
	a.Labels = map[string]string{"profile": "val2"}
	e.Dispatch(context.Background(), a)
	b := alertMsg("disk-low:v", "disk")
	b.Labels = map[string]string{"profile": "archive"}
	e.Dispatch(context.Background(), b)
	waitFor(t, "the unmatched alert", func() bool { return fn.count() == 1 })
	time.Sleep(50 * time.Millisecond)
	if fn.count() != 1 || fn.last().Labels["profile"] != "archive" {
		t.Fatalf("sent %+v", fn.got)
	}
}

func TestSilenceValidation(t *testing.T) {
	e := newTestEngine(&fakeNotifier{kind: "fake"}, 0)
	notEq := false
	for name, s := range map[string]Silence{
		"no matchers":  {EndsAt: time.Now().Add(time.Hour)},
		"matches all":  {Matchers: []Matcher{{Name: "kind", Value: ".*", IsRegex: true}}, EndsAt: time.Now().Add(time.Hour)},
		"negated only": {Matchers: []Matcher{{Name: "kind", Value: "x", IsEqual: &notEq}}, EndsAt: time.Now().Add(time.Hour)},
		"in the past":  silenceFor("kind", "x", -time.Minute),
		"too long":     silenceFor("kind", "x", MaxSilence+time.Hour),
		"bad regex":    {Matchers: []Matcher{{Name: "kind", Value: "(", IsRegex: true}}, EndsAt: time.Now().Add(time.Hour)},
	} {
		if _, err := e.AddSilence(s); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
