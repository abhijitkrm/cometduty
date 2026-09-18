// Package rpc is a deliberately small CometBFT JSON-RPC client. It only
// implements the handful of endpoints cometduty needs (status, abci_query,
// net_info), which keeps the dependency tree tiny and avoids coupling the
// monitor to any particular CometBFT library version.
package rpc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client talks to a single CometBFT RPC endpoint.
type Client struct {
	endpoint string
	http     *http.Client
	headers  map[string]string
}

type Option func(*Client)

// WithHeader sets an extra HTTP header on every request (e.g. Authorization for
// RPC endpoints behind an authenticating proxy).
func WithHeader(k, v string) Option {
	return func(c *Client) { c.headers[k] = v }
}

// WithTimeout overrides the default 10s HTTP timeout.
func WithTimeout(d time.Duration) Option {
	return func(c *Client) { c.http.Timeout = d }
}

// New builds a client for an endpoint. Accepted schemes: http, https, tcp
// (treated as http), unix:// (UDS — not supported for plain RPC, use ws).
func New(rawURL string, opts ...Option) (*Client, error) {
	rawURL = strings.TrimRight(rawURL, "/")
	if rawURL == "" {
		return nil, errors.New("empty rpc url")
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parsing rpc url %q: %w", rawURL, err)
	}
	switch u.Scheme {
	case "tcp":
		u.Scheme = "http"
	case "http", "https":
	case "ws":
		u.Scheme = "http"
	case "wss":
		u.Scheme = "https"
	default:
		return nil, fmt.Errorf("unsupported rpc scheme %q (want http, https, tcp, ws, wss)", u.Scheme)
	}
	c := &Client{
		endpoint: u.String(),
		http:     &http.Client{Timeout: 10 * time.Second},
		headers:  map[string]string{},
	}
	for _, o := range opts {
		o(c)
	}
	return c, nil
}

// Endpoint returns the normalized endpoint URL.
func (c *Client) Endpoint() string { return c.endpoint }

// Int64 is a JSON number that CometBFT serializes as a string.
type Int64 int64

func (i *Int64) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" {
		*i = 0
		return nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return err
	}
	*i = Int64(v)
	return nil
}

// Status is the subset of /status we use.
type Status struct {
	NodeInfo struct {
		Network string `json:"network"`
		Moniker string `json:"moniker"`
		Version string `json:"version"` // cometbft build version
	} `json:"node_info"`
	SyncInfo struct {
		LatestBlockHeight Int64     `json:"latest_block_height"`
		LatestBlockTime   time.Time `json:"latest_block_time"`
		CatchingUp        bool      `json:"catching_up"`
	} `json:"sync_info"`
}

// NetInfo is the subset of /net_info we use (peer count).
type NetInfo struct {
	NPeers Int64 `json:"n_peers"`
}

// CommitSig is one entry in a block's last_commit signature set.
type CommitSig struct {
	BlockIDFlag      int    `json:"block_id_flag"`
	ValidatorAddress string `json:"validator_address"`
	Timestamp        string `json:"timestamp"`
	Signature        string `json:"signature"`
}

// Commit is the subset of /commit we use — the signature set for height-1.
type Commit struct {
	SignedHeader struct {
		Header struct {
			Height          Int64  `json:"height"`
			ProposerAddress string `json:"proposer_address"`
		} `json:"header"`
		Commit struct {
			Height     int64       `json:"height,string"`
			Signatures []CommitSig `json:"signatures"`
		} `json:"commit"`
	} `json:"signed_header"`
	Canonical bool `json:"canonical"`
}

// Validator is one member of the active set.
type Validator struct {
	Address     string `json:"address"`
	VotingPower Int64  `json:"voting_power"`
}

// Validators returns the active set for a height in signature order — the
// last_commit signature array is positional: signatures[i] belongs to the
// i-th validator of this list (absent entries carry an empty address).
func (c *Client) Validators(ctx context.Context, height int64) ([]Validator, error) {
	var out struct {
		Validators []Validator `json:"validators"`
		Total      Int64       `json:"total"`
	}
	params := map[string]any{"per_page": "200"}
	if height > 0 {
		params["height"] = strconv.FormatInt(height, 10)
	}
	if err := c.call(ctx, "validators", params, &out); err != nil {
		return nil, err
	}
	return out.Validators, nil
}

// ABCIResponse is the result of /abci_query.
type ABCIResponse struct {
	Code      uint32 `json:"code"`
	Log       string `json:"log"`
	Key       string `json:"key"`
	Value     []byte `json:"value"` // base64 in JSON, decoded for us
	Height    Int64  `json:"height"`
	Codespace string `json:"codespace"`
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    string `json:"data"`
	} `json:"error"`
}

func (c *Client) call(ctx context.Context, method string, params any, out any) error {
	body, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: 1, Method: method, Params: params})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("rpc %s: http %d: %s", method, resp.StatusCode, truncate(string(b), 200))
	}
	var rr rpcResponse
	if err := json.Unmarshal(b, &rr); err != nil {
		return fmt.Errorf("rpc %s: decoding response: %w", method, err)
	}
	if rr.Error != nil {
		return fmt.Errorf("rpc %s: %s %s", method, rr.Error.Message, rr.Error.Data)
	}
	if rr.Result == nil {
		return fmt.Errorf("rpc %s: empty result", method)
	}
	return json.Unmarshal(rr.Result, out)
}

// Status fetches node status.
func (c *Client) Status(ctx context.Context) (*Status, error) {
	var s Status
	if err := c.call(ctx, "status", map[string]any{}, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// NetInfo fetches peer info.
func (c *Client) NetInfo(ctx context.Context) (*NetInfo, error) {
	var n NetInfo
	if err := c.call(ctx, "net_info", map[string]any{}, &n); err != nil {
		return nil, err
	}
	return &n, nil
}

// Commit returns the signed header for a height (0 = latest). The commit's
// signatures cover height-1 — CometBFT commits a block in the next block.
func (c *Client) Commit(ctx context.Context, height int64) (*Commit, error) {
	var r Commit
	params := map[string]any{}
	if height > 0 {
		params["height"] = strconv.FormatInt(height, 10) // cometbft wants integers as strings
	}
	if err := c.call(ctx, "commit", params, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// ABCIQuery performs an ABCI query. data is sent as hex (the encoding CometBFT
// expects for the data param); the returned value is decoded bytes.
func (c *Client) ABCIQuery(ctx context.Context, path string, data []byte) (*ABCIResponse, error) {
	var r struct {
		Response ABCIResponse `json:"response"`
	}
	params := map[string]any{
		"path":  path,
		"data":  fmt.Sprintf("%X", data), // HexBytes: uppercase hex, NO 0x prefix
		"prove": false,
	}
	if err := c.call(ctx, "abci_query", params, &r); err != nil {
		return nil, err
	}
	if r.Response.Code != 0 {
		return &r.Response, fmt.Errorf("abci query %s: code %d %s", path, r.Response.Code, r.Response.Log)
	}
	return &r.Response, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
