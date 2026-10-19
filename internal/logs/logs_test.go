package logs

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/abhijitkrm/cometduty/internal/config"
)

func TestRuleWindowCountCooldown(t *testing.T) {
	r := &Rule{
		Name: "peer-churn", Re: regexp.MustCompile(`Stopping peer`),
		Count: 3, Window: time.Minute, Cooldown: 5 * time.Minute,
	}
	now := time.Now()
	if r.Match(now) || r.Match(now.Add(time.Second)) {
		t.Fatal("should not fire before Count hits")
	}
	if !r.Match(now.Add(2 * time.Second)) {
		t.Fatal("third hit inside the window should fire")
	}
	// still matching but inside cooldown → quiet
	if r.Match(now.Add(3*time.Second)) || r.Match(now.Add(4*time.Second)) {
		t.Fatal("cooldown should suppress repeats")
	}
	// past cooldown: window expired, so the count rebuilds from fresh hits
	t2 := now.Add(6 * time.Minute)
	if r.Match(t2) || r.Match(t2.Add(time.Second)) {
		t.Fatal("post-cooldown hits below threshold should not fire")
	}
	if !r.Match(t2.Add(2 * time.Second)) {
		t.Fatal("should re-fire once the count rebuilds after cooldown")
	}
}

func TestRuleWindowExpiry(t *testing.T) {
	r := &Rule{
		Name: "x", Re: regexp.MustCompile(`x`),
		Count: 2, Window: time.Minute, Cooldown: 0,
	}
	now := time.Now()
	r.Match(now)
	// second hit lands outside the window — first hit expired, no fire
	if r.Match(now.Add(2 * time.Minute)) {
		t.Fatal("expired hits must not count toward the threshold")
	}
}

func TestCompileDefaults(t *testing.T) {
	rules, err := Compile([]config.LogRule{{Name: "mine", Pattern: "abc"}})
	if err != nil || len(rules) != 1 {
		t.Fatalf("compile: %v %d", err, len(rules))
	}
	r := rules[0]
	if r.Severity != "critical" || r.Count != 1 || r.Window != 5*time.Minute || r.Cooldown != 30*time.Minute {
		t.Fatalf("defaults not applied: %+v", r)
	}
	if _, err := Compile([]config.LogRule{{Name: "bad", Pattern: "["}}); err == nil {
		t.Fatal("bad regex should fail to compile")
	}
}

// muxFrame wraps a payload in docker's 8-byte multiplexed header.
func muxFrame(stream byte, payload []byte) []byte {
	b := make([]byte, 8+len(payload))
	b[0] = stream
	binary.BigEndian.PutUint32(b[4:], uint32(len(payload)))
	copy(b[8:], payload)
	return b
}

func TestDockerDemux(t *testing.T) {
	var body bytes.Buffer
	body.Write(muxFrame(1, []byte("2026-09-25T10:00:00Z first line\npartial")))
	body.Write(muxFrame(1, []byte(" continues\n2026-09-25T10:00:01Z second\n")))
	body.Write(muxFrame(2, []byte("2026-09-25T10:00:02Z stderr line\n")))

	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/vnd.docker.multiplexed-stream")
		w.Write(body.Bytes())
	}))
	defer srv.Close()

	src, err := NewDockerSource(srv.URL, "c1", 0)
	if err != nil {
		t.Fatal(err)
	}
	out := make(chan string, 16)
	ctx := context.Background()
	done := make(chan error, 1)
	go func() { done <- src.Stream(ctx, out) }()
	var got []string
	for range [4]int{} {
		select {
		case line := <-out:
			got = append(got, line)
		case <-time.After(3 * time.Second):
			t.Fatal("timed out waiting for lines")
		}
	}
	<-done
	want := []string{"first line", "partial continues", "second", "stderr line"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("line %d: got %q want %q", i, got[i], want[i])
		}
	}
	if !src.last.Equal(time.Date(2026, 9, 25, 10, 0, 2, 0, time.UTC)) {
		t.Fatalf("last = %v, want the newest docker timestamp", src.last)
	}

	// a reconnect reads back with tail= (since= makes dockerd read a
	// json-file log from its start) and drops the lines already seen
	_ = src.Stream(ctx, out) // ends with the body (EOF)
	if q := queries[len(queries)-1]; strings.Contains(q, "since=") || !strings.Contains(q, fmt.Sprintf("tail=%d", resumeTail)) {
		t.Fatalf("reconnect query %q: want tail=%d and no since", q, resumeTail)
	}
	select {
	case line := <-out:
		if line != "partial continues" { // no timestamp of its own: can't be told apart
			t.Fatalf("reconnect replayed %q", line)
		}
	default:
	}
	select {
	case line := <-out:
		t.Fatalf("reconnect replayed %q", line)
	default:
	}
}

func TestFileSourceTail(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "node.log")
	if err := os.WriteFile(p, []byte("old line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	src, err := NewFileSource(p)
	if err != nil {
		t.Fatal(err)
	}
	out := make(chan string, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go src.Stream(ctx, out)

	// history is skipped; only new lines arrive
	time.Sleep(1200 * time.Millisecond)
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	fmt.Fprintln(f, "new one")
	f.Close()
	select {
	case line := <-out:
		if line != "new one" {
			t.Fatalf("got %q want %q", line, "new one")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out — file source didn't see appended line")
	}
	// the pre-existing line must never appear
	select {
	case line := <-out:
		t.Fatalf("unexpected historical line: %q", line)
	case <-time.After(300 * time.Millisecond):
	}
}

// The disk rules match what cometbft, the SDK and their databases log when a
// disk fills, fails or corrupts — and stay quiet on everyday lines.
func TestDiskRules(t *testing.T) {
	rules, err := Compile(DefaultRules())
	if err != nil {
		t.Fatal(err)
	}
	re := map[string]*regexp.Regexp{}
	for _, r := range rules {
		re[r.Name] = r.Re
	}
	hit := map[string][]string{
		"disk-full":     {`failed to write WAL: write /root/.evmd/data/cs.wal/wal: no space left on device`},
		"disk-io-error": {`E[2026-10-15] error on pebble: read /home/data/application.db/000123.sst: input/output error`, `blk_update_request: I/O error, dev nvme1n1, sector 2048`},
		"read-only-fs":  {`open /home/data/priv_validator_state.json: read-only file system`},
		"db-corruption": {`panic: leveldb: corrupted or incomplete meta file`, `pebble: corruption: checksum mismatch in 000042.sst`, `failed to load latest version: database is corrupt`, `cs.wal: corrupted WAL file`},
	}
	for name, lines := range hit {
		for _, l := range lines {
			if !re[name].MatchString(l) {
				t.Errorf("%s should match %q", name, l)
			}
		}
	}
	for _, l := range []string{
		`I[2026-10-15] committed state module=state height=100`,
		`INF indexed block events module=txindex height=100`,
		`ERR dial tcp 10.0.0.1:26656: connection refused module=p2p`,
	} {
		for name := range hit {
			if re[name].MatchString(l) {
				t.Errorf("%s matched an ordinary line %q", name, l)
			}
		}
	}
}

func TestKernelRules(t *testing.T) {
	rules, err := Compile(KernelRules())
	if err != nil {
		t.Fatal(err)
	}
	re := map[string]*regexp.Regexp{}
	for _, r := range rules {
		re[r.Name] = r.Re
	}
	for name, l := range map[string]string{
		"kernel-io-error":    `Oct 15 10:13:02 ip-1 kernel: blk_update_request: I/O error, dev nvme1n1, sector 2048 op 0x1:(WRITE)`,
		"kernel-fs-error":    `kernel: EXT4-fs error (device nvme1n1p1): ext4_find_entry:1455: inode #2: comm evmd: reading directory lblock 0`,
		"kernel-fs-readonly": `kernel: EXT4-fs (nvme1n1p1): Remounting filesystem read-only`,
		"kernel-nvme":        `kernel: nvme nvme1: I/O 12 QID 3 timeout, aborting`,
	} {
		if !re[name].MatchString(l) {
			t.Errorf("%s should match %q", name, l)
		}
	}
	for _, l := range []string{`kernel: EXT4-fs (nvme1n1p1): mounted filesystem with ordered data mode`, `kernel: nvme nvme0: 2/0/0 default/read/poll queues`} {
		for name, r := range re {
			if r.MatchString(l) {
				t.Errorf("%s matched an ordinary line %q", name, l)
			}
		}
	}
}

// logrotate renames the file and creates a new one: the source follows.
func TestFileSourceFollowsRename(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "kern.log")
	if err := os.WriteFile(p, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	src, _ := NewFileSource(p)
	out := make(chan string, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go src.Stream(ctx, out)
	time.Sleep(1200 * time.Millisecond)
	if err := os.Rename(p, p+".1"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("after rotation\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case line := <-out:
		if line != "after rotation" {
			t.Fatalf("got %q", line)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("the source didn't follow the rotated file")
	}
}
