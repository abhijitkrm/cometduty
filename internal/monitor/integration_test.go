package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/abhijitkrm/cometduty/internal/alert"
	"github.com/abhijitkrm/cometduty/internal/bech32"
	"github.com/abhijitkrm/cometduty/internal/config"
)

// fakeNode is a minimal CometBFT endpoint: JSON-RPC over POST / and an event
// websocket at /websocket.
type fakeNode struct {
	srv *httptest.Server

	mu        sync.Mutex
	height    int64
	network   string
	subs      []string
	wsUp      *websocket.Upgrader
	pushBlock func(rawBlockJSON string)
}

func newFakeNode(t *testing.T, network string) *fakeNode {
	t.Helper()
	fn := &fakeNode{network: network, height: 1000, wsUp: &websocket.Upgrader{}}

	mux := http.NewServeMux()
	mux.HandleFunc("/websocket", fn.serveWS)
	mux.HandleFunc("/", fn.serveRPC)
	fn.srv = httptest.NewServer(mux)
	t.Cleanup(fn.srv.Close)
	return fn
}

func (fn *fakeNode) serveRPC(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", 400)
		return
	}
	fn.mu.Lock()
	height := fn.height
	fn.mu.Unlock()

	write := func(result any) {
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
	}
	switch req.Method {
	case "status":
		write(map[string]any{
			"node_info": map[string]any{"network": fn.network, "moniker": "fake"},
			"sync_info": map[string]any{
				"latest_block_height": fmt.Sprint(height),
				"latest_block_time":   time.Now().UTC().Format(time.RFC3339),
				"catching_up":         false,
			},
		})
	case "net_info":
		write(map[string]any{"n_peers": "7"})
	case "abci_query":
		// simulate a chain without x/slashing
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"response":{"code":1,"log":"unknown query path","height":"1000"}}}`))
	default:
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"method not found"}}`))
	}
}

// serveWS upgrades, records subscribe queries, then streams whatever the test
// pushes through pushBlock.
func (fn *fakeNode) serveWS(w http.ResponseWriter, r *http.Request) {
	conn, err := fn.wsUp.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	var wmu sync.Mutex
	write := func(v any) bool {
		wmu.Lock()
		defer wmu.Unlock()
		return conn.WriteJSON(v) == nil
	}
	push := make(chan string, 16)
	fn.mu.Lock()
	fn.pushBlock = func(b string) { push <- b }
	fn.mu.Unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			var sub struct {
				Method string `json:"method"`
				Params struct {
					Query string `json:"query"`
				} `json:"params"`
			}
			if err := conn.ReadJSON(&sub); err != nil {
				return
			}
			if sub.Method == "subscribe" {
				fn.mu.Lock()
				fn.subs = append(fn.subs, sub.Params.Query)
				fn.mu.Unlock()
				write(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{}})
			}
		}
	}()
	for {
		select {
		case <-done:
			return
		case raw := <-push:
			ev := map[string]any{
				"jsonrpc": "2.0", "id": 2,
				"result": map[string]any{
					"query": "tm.event='NewBlock'",
					"data":  map[string]any{"type": "tendermint/event/NewBlock", "value": json.RawMessage(raw)},
				},
			}
			if !write(ev) {
				return
			}
		}
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// recordingHub captures dashboard status updates.
type recordingHub struct {
	mu  sync.Mutex
	got []*Status
}

func (h *recordingHub) PublishStatus(s *Status) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.got = append(h.got, s)
}
func (h *recordingHub) Log(any) {}
func (h *recordingHub) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.got)
}
func (h *recordingHub) last() *Status {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.got[len(h.got)-1]
}

func testChain(t *testing.T, fn *fakeNode, consAddr []byte, hub *recordingHub) *Chain {
	t.Helper()
	valcons, err := bech32.Encode("testvalcons", consAddr)
	if err != nil {
		t.Fatal(err)
	}
	root := &config.Config{NodeDownMin: 5}
	cc := &config.ChainConfig{
		ChainID:         "test-1",
		ValconsOverride: valcons,
		Nodes:           []*config.NodeConfig{{URL: fn.srv.URL, AlertIfDown: true}},
		Alerts:          config.AlertConfig{ConsecutiveEnabled: true, ConsecutiveMissed: 3},
	}
	eng := alert.NewEngine(func(*alert.Alert) []alert.ResolvedDest { return nil }, time.Minute, 0)
	c := NewChain("test", cc, func() *config.Config { return root }, eng, nil, hub)
	c.log = slog.Default()
	return c
}

var testConsAddr = []byte{
	0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0xaa,
	0xbb, 0xcc, 0xdd, 0xee, 0xff, 0x00, 0x01, 0x12, 0x23, 0x34,
}

func testConsHex() string {
	return strings.ToUpper(fmt.Sprintf("%X", testConsAddr))
}

func newBlockEvent(height int64, proposer, signerHex string, signed bool) string {
	flag := 1
	sig := ""
	if signed {
		flag, sig = 2, "AA=="
	}
	return fmt.Sprintf(`{"block":{"header":{"height":"%d","proposer_address":"%s"},
"last_commit":{"height":"%d","signatures":[{"block_id_flag":%d,"validator_address":"%s","signature":"%s"}]}}}`,
		height, proposer, height-1, flag, signerHex, sig)
}

func TestEndToEndBlockFlow(t *testing.T) {
	fn := newFakeNode(t, "test-1")
	hub := &recordingHub{}
	c := testChain(t, fn, testConsAddr, hub)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 1. endpoint selection against the fake
	if err := c.pickClient(ctx); err != nil {
		t.Fatalf("pickClient: %v", err)
	}
	if c.client == nil {
		t.Fatal("no client selected")
	}

	// 2. validator info via valcons_override — no staking query needed, and the
	// fake's "unknown query path" abci response should disable slashing checks.
	c.refreshValInfo(ctx, true)
	tg := c.targets[0]
	if tg.info == nil || tg.info.ConsHex != testConsHex() {
		t.Fatalf("val info not resolved: %+v", tg.info)
	}
	if c.slashingOK {
		t.Error("slashing should be disabled after unknown-query-path error")
	}

	// 3. feed blocks: one missed, one signed, one proposed
	missed := newBlockEvent(1001, "DEADBEEF", testConsHex(), false)
	signed := newBlockEvent(1002, "DEADBEEF", testConsHex(), true)
	proposed := newBlockEvent(1003, testConsHex(), testConsHex(), true)

	for _, raw := range []string{missed, signed, proposed} {
		b, err := decodeBlock([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		c.handleBlock([]byte(raw))
		_ = b
	}

	if tg.consec != 0 || tg.miss != 1 || tg.signs != 2 || tg.props != 1 {
		t.Errorf("stats: miss=%d signs=%d props=%d consec=%d", tg.miss, tg.signs, tg.props, tg.consec)
	}
	if hub.count() != 3 {
		t.Fatalf("hub got %d statuses", hub.count())
	}
	last := hub.last()
	if last.Height != 1003 || last.Moniker == "" {
		t.Errorf("status: %+v", last)
	}
}

func TestWSLoopStreamsBlocks(t *testing.T) {
	fn := newFakeNode(t, "test-1")
	hub := &recordingHub{}
	c := testChain(t, fn, testConsAddr, hub)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := c.pickClient(ctx); err != nil {
		t.Fatalf("pickClient: %v", err)
	}
	c.refreshValInfo(ctx, true)

	done := make(chan error, 1)
	go func() { done <- c.wsLoop(ctx) }()

	// wait for the subscription then push two blocks
	waitFor(t, "ws subscribe", func() bool {
		fn.mu.Lock()
		defer fn.mu.Unlock()
		return len(fn.subs) == 2
	})
	waitFor(t, "push fn ready", func() bool {
		fn.mu.Lock()
		defer fn.mu.Unlock()
		return fn.pushBlock != nil
	})
	fn.pushBlock(newBlockEvent(1001, "DEADBEEF", testConsHex(), true))
	fn.pushBlock(newBlockEvent(1002, "DEADBEEF", testConsHex(), false))
	waitFor(t, "blocks handled", func() bool { return hub.count() >= 2 })

	last := hub.last()
	if last.Height != 1002 {
		t.Errorf("height %d want 1002", last.Height)
	}
	tg := c.targets[0]
	if tg.miss != 1 || tg.signs != 1 {
		t.Errorf("stats: miss=%d signs=%d", tg.miss, tg.signs)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Error("wsLoop did not exit on cancel")
	}
}

func TestPickClientRejectsWrongChain(t *testing.T) {
	fn := newFakeNode(t, "different-9")
	hub := &recordingHub{}
	c := testChain(t, fn, testConsAddr, hub)
	if err := c.pickClient(context.Background()); err == nil {
		t.Error("wrong chain-id accepted")
	}
}

func TestResolveConsAddress(t *testing.T) {
	// override path
	vc, _ := bech32.Encode("xvalcons", testConsAddr)
	addr, s, err := resolveConsAddress("", vc, "", nil)
	if err != nil || s != vc || string(addr) != string(testConsAddr) {
		t.Errorf("override: %x %q %v", addr, s, err)
	}
	// valcons in valoper field
	addr, s, err = resolveConsAddress(vc, "", "", nil)
	if err != nil || s != vc {
		t.Errorf("valcons-as-valoper: %q %v", s, err)
	}
	// prefix derivation from valoper
	addr, s, err = resolveConsAddress("cosmosvaloper1xyz", "", "", testConsAddr)
	if err != nil {
		t.Fatal(err)
	}
	if hrp, _, e := bech32.Decode(s); e != nil || hrp != "cosmosvalcons" {
		t.Errorf("derived prefix: %q", s)
	}
	// explicit prefix
	_, s, err = resolveConsAddress("iva1xyz", "", "icavkcons", testConsAddr)
	if err != nil {
		t.Fatal(err)
	}
	if hrp, _, _ := bech32.Decode(s); hrp != "icavkcons" {
		t.Errorf("explicit prefix: %q", s)
	}
	// unknown hrp without valoper suffix → error
	if _, _, err = resolveConsAddress("weird1xyz", "", "", testConsAddr); err == nil {
		t.Error("underivable prefix accepted")
	}
	// bad override
	if _, _, err = resolveConsAddress("", "notbech32", "", nil); err == nil {
		t.Error("bad override accepted")
	}
}
