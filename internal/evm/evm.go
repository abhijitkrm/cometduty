// Package evm is a minimal JSON-RPC client for the EVM side of a cosmos-evm
// chain. CometBFT RPC answers "is consensus producing blocks"; these calls
// answer "is the execution layer actually running them" — a distinct failure
// mode (wedged EVM, gas oracle insane, execution lag).
package evm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type Client struct {
	url string
	hc  *http.Client
}

func New(url string, timeout time.Duration) *Client {
	return &Client{url: url, hc: &http.Client{Timeout: timeout}}
}

type rpcReq struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  []any  `json:"params"`
}

type rpcResp struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (c *Client) call(ctx context.Context, method string, params []any, out any) error {
	body, err := json.Marshal(rpcReq{JSONRPC: "2.0", ID: 1, Method: method, Params: params})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	var r rpcResp
	if err := json.Unmarshal(b, &r); err != nil {
		return fmt.Errorf("evm rpc %s: bad response: %w", method, err)
	}
	if r.Error != nil {
		return fmt.Errorf("evm rpc %s: %s", method, r.Error.Message)
	}
	return json.Unmarshal(r.Result, out)
}

func hexInt(s string) (int64, error) {
	s = strings.TrimPrefix(s, "0x")
	return strconv.ParseInt(s, 16, 64)
}

// ChainID returns the EVM chain id (eth_chainId) — distinct from the CometBFT
// chain-id string; both are reported by doctor.
func (c *Client) ChainID(ctx context.Context) (int64, error) {
	var s string
	if err := c.call(ctx, "eth_chainId", nil, &s); err != nil {
		return 0, err
	}
	return hexInt(s)
}

// BlockNumber returns the latest executed EVM block (eth_blockNumber). Compare
// against the CometBFT height to detect execution lag.
func (c *Client) BlockNumber(ctx context.Context) (int64, error) {
	var s string
	if err := c.call(ctx, "eth_blockNumber", nil, &s); err != nil {
		return 0, err
	}
	return hexInt(s)
}

// SyncStatus is eth_syncing's object form; syncing=false means fully synced.
type SyncStatus struct {
	Starting int64
	Current  int64
	Highest  int64
}

// Syncing reports eth_syncing. (false, nil) = synced; (true, *status) = mid-sync.
func (c *Client) Syncing(ctx context.Context) (bool, *SyncStatus, error) {
	var raw json.RawMessage
	if err := c.call(ctx, "eth_syncing", nil, &raw); err != nil {
		return false, nil, err
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err == nil {
		return b, nil, nil
	}
	var obj struct {
		StartingBlock string `json:"startingBlock"`
		CurrentBlock  string `json:"currentBlock"`
		HighestBlock  string `json:"highestBlock"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return false, nil, err
	}
	st := &SyncStatus{}
	st.Starting, _ = hexInt(obj.StartingBlock)
	st.Current, _ = hexInt(obj.CurrentBlock)
	st.Highest, _ = hexInt(obj.HighestBlock)
	return true, st, nil
}

// PeerCount returns net_peerCount.
func (c *Client) PeerCount(ctx context.Context) (int64, error) {
	var s string
	if err := c.call(ctx, "net_peerCount", nil, &s); err != nil {
		return 0, err
	}
	return hexInt(s)
}

// GasPrice returns eth_gasPrice in wei.
func (c *Client) GasPrice(ctx context.Context) (*big.Int, error) {
	var s string
	if err := c.call(ctx, "eth_gasPrice", nil, &s); err != nil {
		return nil, err
	}
	v, ok := new(big.Int).SetString(strings.TrimPrefix(s, "0x"), 16)
	if !ok {
		return nil, fmt.Errorf("bad gas price %q", s)
	}
	return v, nil
}

// ClientVersion returns web3_clientVersion (e.g. the evmd build identity).
func (c *Client) ClientVersion(ctx context.Context) (string, error) {
	var s string
	err := c.call(ctx, "web3_clientVersion", nil, &s)
	return s, err
}
