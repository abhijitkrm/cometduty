package rpc

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestStatusParsesStringNumbers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req rpcRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		if req.Method != "status" {
			t.Errorf("method %q", req.Method)
		}
		if r.Header.Get("X-Auth") != "tok" {
			t.Error("custom header missing")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{
			"node_info":{"network":"cosmoshub-4","moniker":"test"},
			"sync_info":{"latest_block_height":"12345678","latest_block_time":"2024-01-01T00:00:00Z","catching_up":false}
		}}`))
	}))
	defer srv.Close()

	c, err := New(srv.URL, WithHeader("X-Auth", "tok"), WithTimeout(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	st, err := c.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.NodeInfo.Network != "cosmoshub-4" {
		t.Errorf("network %q", st.NodeInfo.Network)
	}
	if int64(st.SyncInfo.LatestBlockHeight) != 12345678 {
		t.Errorf("height %d", st.SyncInfo.LatestBlockHeight)
	}
}

func TestABCIQueryDecodesBase64(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string                 `json:"method"`
			Params map[string]interface{} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Method != "abci_query" {
			t.Errorf("method %q", req.Method)
		}
		if req.Params["path"] != "/cosmos.staking.v1beta1.Query/Validator" {
			t.Errorf("path %v", req.Params["path"])
		}
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"response":{
			"code":0,"value":"aGVsbG8=","height":"42"
		}}}`))
	}))
	defer srv.Close()

	c, _ := New(srv.URL)
	resp, err := c.ABCIQuery(context.Background(), "/cosmos.staking.v1beta1.Query/Validator", []byte{0x01})
	if err != nil {
		t.Fatal(err)
	}
	if string(resp.Value) != "hello" {
		t.Errorf("value %q", resp.Value)
	}
	if int64(resp.Height) != 42 {
		t.Errorf("height %d", resp.Height)
	}
}

func TestRPCError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"method not found"}}`))
	}))
	defer srv.Close()
	c, _ := New(srv.URL)
	if _, err := c.Status(context.Background()); err == nil {
		t.Error("rpc error not surfaced")
	}
}

func TestHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	c, _ := New(srv.URL)
	if _, err := c.Status(context.Background()); err == nil {
		t.Error("http error not surfaced")
	}
}

func TestSchemeNormalization(t *testing.T) {
	for in, want := range map[string]string{
		"tcp://host:26657":  "http://host:26657",
		"ws://host:26657":   "http://host:26657",
		"wss://host:26657":  "https://host:26657",
		"https://host:443/": "https://host:443",
	} {
		c, err := New(in)
		if err != nil {
			t.Errorf("%s: %v", in, err)
			continue
		}
		if c.Endpoint() != want {
			t.Errorf("%s → %s want %s", in, c.Endpoint(), want)
		}
	}
	if _, err := New("ftp://x"); err == nil {
		t.Error("bad scheme accepted")
	}
	if _, err := New(""); err == nil {
		t.Error("empty url accepted")
	}
}
