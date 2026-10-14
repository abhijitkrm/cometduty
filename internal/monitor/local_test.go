package monitor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/abhijitkrm/cometduty/internal/alert"
	"github.com/abhijitkrm/cometduty/internal/config"
)

func TestParseJSONInt(t *testing.T) {
	for in, want := range map[string]int64{`"12345"`: 12345, `678`: 678, `"0"`: 0} {
		got, err := parseJSONInt(json.RawMessage(in))
		if err != nil || got != want {
			t.Fatalf("parseJSONInt(%s) = %d, %v — want %d", in, got, err, want)
		}
	}
}

// newLocalChain builds a chain with one node whose home dir is a temp dir —
// probeLocal then runs against it with no RPC needed.
func newLocalChain(t *testing.T, alertCfg config.AlertConfig) (*Chain, *nodeState, *alert.Engine, string) {
	t.Helper()
	home := t.TempDir()
	cc := &config.ChainConfig{
		ChainID: "test-1",
		Alerts:  alertCfg,
		Nodes:   []*config.NodeConfig{{URL: "http://n:26657", Name: "n", HomeDir: home}},
	}
	root := &config.Config{}
	eng := alert.NewEngine(func(*alert.Alert) []alert.ResolvedDest { return nil }, time.Minute, 0)
	c := NewChain("test", cc, func() *config.Config { return root }, eng, nil)
	return c, c.nodes[0], eng, home
}

func writeSignerState(t *testing.T, home string, height int64) {
	t.Helper()
	dir := filepath.Join(home, "data")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := []byte(`{"height":"` + strconv.FormatInt(height, 10) + `","round":0,"step":3}`)
	if err := os.WriteFile(filepath.Join(dir, "priv_validator_state.json"), content, 0o644); err != nil {
		t.Fatal(err)
	}
}

// alert dispatch is async (go c.eng.Dispatch) — poll until the engine sees it.
func waitActive(t *testing.T, eng *alert.Engine, chain string, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if eng.ActiveCount(chain) == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("ActiveCount(%s): want %d, got %d", chain, want, eng.ActiveCount(chain))
}

func TestSignerMissingFileAlerts(t *testing.T) {
	c, n, eng, _ := newLocalChain(t, config.AlertConfig{})
	c.probeLocal(n)
	if !n.signFileAlarm {
		t.Fatal("expected signer-state alarm for missing priv_validator_state.json")
	}
	waitActive(t, eng, "test", 1)
}

func TestSignerRegressionAlerts(t *testing.T) {
	c, n, eng, home := newLocalChain(t, config.AlertConfig{})
	writeSignerState(t, home, 100)
	c.probeLocal(n)
	if n.signerHeight != 100 || n.signerMax != 100 {
		t.Fatalf("signer height %d max %d, want 100/100", n.signerHeight, n.signerMax)
	}
	writeSignerState(t, home, 50) // regression — state file went backwards
	c.probeLocal(n)
	if !n.regressAlarm {
		t.Fatal("expected signer-regressed critical alert")
	}
	// recovery past the previous max resolves the regression
	writeSignerState(t, home, 150)
	c.probeLocal(n)
	if n.regressAlarm {
		t.Fatal("regression should resolve once height re-advances past the max")
	}
	waitActive(t, eng, "test", 0)
}

func TestSignerStallWhileChainAdvances(t *testing.T) {
	c, n, _, home := newLocalChain(t, config.AlertConfig{SignerStallMin: 1})
	writeSignerState(t, home, 100)
	c.probeLocal(n)
	c.mu.Lock()
	n.signerAt = time.Now().Add(-2 * time.Minute) // pretend it stalled
	c.lastHeight = 200                            // chain kept going
	c.mu.Unlock()
	c.updateSigner(n, 100) // same height again
	if !n.signerAlarm {
		t.Fatal("expected signer-stalled alert")
	}
}

func TestDiskFreeMetric(t *testing.T) {
	c, n, _, home := newLocalChain(t, config.AlertConfig{})
	writeSignerState(t, home, 10)
	c.probeLocal(n)
	if n.diskFree <= 0 {
		t.Fatal("expected positive disk-free reading for tempdir filesystem")
	}
}

func TestProbeLocalSkipsWithoutHome(t *testing.T) {
	cc := &config.ChainConfig{ChainID: "x", Nodes: []*config.NodeConfig{{URL: "http://n"}}}
	eng := alert.NewEngine(func(*alert.Alert) []alert.ResolvedDest { return nil }, time.Minute, 0)
	c := NewChain("test", cc, func() *config.Config { return &config.Config{} }, eng, nil)
	c.probeLocal(c.nodes[0]) // no home — must not touch anything
	if eng.ActiveCount("test") != 0 {
		t.Fatal("no alerts expected without a home mount")
	}
}

// Alerts about a node carry its name and labels, so receivers and silences
// can name it without knowing its URL.
func TestAlertsCarryNodeNameAndLabels(t *testing.T) {
	cc := &config.ChainConfig{ChainID: "test-1", Nodes: []*config.NodeConfig{{
		URL: "http://n:26657", Name: "val1", EvmRPC: "http://n:8545",
		Labels: map[string]string{"profile": "val1-prod"},
	}}}
	got := make(chan alert.Alert, 4)
	eng := alert.NewEngine(func(a *alert.Alert) []alert.ResolvedDest {
		got <- *a
		return nil
	}, 0, 0)
	c := NewChain("test", cc, func() *config.Config { return &config.Config{} }, eng, nil)
	c.alert("test-1", nil, "evm-down:http://n:8545", false, "warning", "evm down")
	a := <-got
	if a.Kind != "evm-down" || a.Target != "http://n:8545" || a.Node != "val1" || a.Labels["profile"] != "val1-prod" {
		t.Fatalf("alert %+v", a)
	}
}

// A node that was catching up and then stops answering must start the
// node-down clock (it used to stay zero, so node-down never fired).
func TestDownAfterCatchingUpStartsTheClock(t *testing.T) {
	c, n, _, _ := newLocalChain(t, config.AlertConfig{})
	n.down, n.syncing = true, true // as probeNode leaves a syncing node
	c.markDown(n, "down: connection refused")
	if n.downSince.IsZero() || n.syncing {
		t.Fatalf("downSince %v syncing %v", n.downSince, n.syncing)
	}
	first := n.downSince
	c.markDown(n, "down: connection refused")
	if !n.downSince.Equal(first) {
		t.Fatal("a second failure restarted the clock")
	}
}

// An archive or RPC node's signer state stays at 0: it never signs, so it
// can't stall.
func TestNeverSignedNodeDoesNotStall(t *testing.T) {
	c, n, _, home := newLocalChain(t, config.AlertConfig{SignerStallMin: 1})
	writeSignerState(t, home, 0)
	c.probeLocal(n)
	c.mu.Lock()
	n.signerAt = time.Now().Add(-10 * time.Minute)
	c.lastHeight = 500
	c.mu.Unlock()
	c.updateSigner(n, 0)
	if n.signerAlarm {
		t.Fatal("signer-stalled on a node that never signed")
	}
}

// EVM lag is what it was when the EVM height was read; the consensus head
// moving on between probes (fast chains: several blocks a second) is not lag.
func TestEvmLagMeasuredAtProbeTime(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		res := `"0x3e8"` // 1000
		if req.Method == "eth_syncing" {
			res = "false"
		}
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + string(req.ID) + `,"result":` + res + `}`))
	}))
	defer srv.Close()
	c, _, _, _ := newLocalChain(t, config.AlertConfig{})
	p := &evmProbe{url: srv.URL}
	c.mu.Lock()
	c.lastHeight = 1003
	c.mu.Unlock()
	c.probeEVM(context.Background(), p)
	c.mu.Lock()
	c.lastHeight = 1200 // the chain moved on after the probe
	lag := p.lag
	c.mu.Unlock()
	if lag != 3 {
		t.Fatalf("lag %d, want 3", lag)
	}
}
