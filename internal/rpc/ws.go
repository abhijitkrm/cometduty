package rpc

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// WSClient is a websocket connection to a CometBFT endpoint's /websocket
// handler. It adds the things the upstream tendermint client lacked: unix
// socket dialing, optional insecure TLS, custom headers, and keepalive pings.
type WSClient struct {
	conn     *websocket.Conn
	writeMu  sync.Mutex
	url      string
	pingDone chan struct{}
}

// WSOptions tune the websocket dialer.
type WSOptions struct {
	Headers     map[string]string
	InsecureTLS bool // skip TLS verification (self-signed certs on sentries)
	PingEvery   time.Duration
}

// WSEvent is a trimmed-down subscription message. Data.Value carries the raw
// JSON of the event (block, vote, ...) for the consumer to decode.
type WSEvent struct {
	Result struct {
		Query string `json:"query"`
		Data  struct {
			Type  string          `json:"type"`
			Value json.RawMessage `json:"value"`
		} `json:"data"`
	} `json:"result"`
}

// Type returns the abci event type, e.g. "tendermint/event/NewBlock".
func (e WSEvent) Type() string { return e.Result.Data.Type }

// Value returns the event payload JSON.
func (e WSEvent) Value() []byte {
	if e.Result.Data.Value == nil {
		return []byte{}
	}
	return e.Result.Data.Value
}

// DialWS connects a websocket for the given RPC endpoint URL. The /websocket
// suffix is added automatically if missing.
func DialWS(ctx context.Context, rawURL string, opts *WSOptions) (*WSClient, error) {
	if opts == nil {
		opts = &WSOptions{}
	}
	u := strings.TrimRight(rawURL, "/")
	if !strings.HasSuffix(u, "/websocket") {
		u += "/websocket"
	}
	endpoint, err := url.Parse(u)
	if err != nil {
		return nil, fmt.Errorf("parsing ws url %q: %w", u, err)
	}

	dialer := &websocket.Dialer{
		HandshakeTimeout:  15 * time.Second,
		EnableCompression: true,
	}
	var unixSocket string
	switch endpoint.Scheme {
	case "http", "tcp", "ws":
		endpoint.Scheme = "ws"
	case "https", "wss":
		endpoint.Scheme = "wss"
	case "unix":
		// unix:///path/to/socket — dial the socket path, speak ws to a placeholder host.
		unixSocket = endpoint.Path
		if unixSocket == "" {
			unixSocket = endpoint.Host + endpoint.Path
		}
		endpoint.Scheme = "ws"
		endpoint.Host = "unix"
	default:
		return nil, fmt.Errorf("protocol %s is unknown, valid choices are http, https, tcp, unix, ws, wss", endpoint.Scheme)
	}

	if unixSocket != "" {
		socket := unixSocket
		dialer.NetDialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		}
	}
	if opts.InsecureTLS && endpoint.Scheme == "wss" {
		dialer.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec -- explicitly requested for self-signed sentry certs
	}

	hdr := http.Header{}
	for k, v := range opts.Headers {
		hdr.Set(k, v)
	}
	conn, _, err := dialer.DialContext(ctx, endpoint.String(), hdr)
	if err != nil {
		return nil, fmt.Errorf("dialing %s: %w", endpoint.Redacted(), err)
	}

	ws := &WSClient{conn: conn, url: endpoint.Redacted()}

	// Keepalive: CometBFT closes idle connections; keep the socket alive and
	// detect half-open connections.
	if opts.PingEvery > 0 {
		done := make(chan struct{})
		ws.pingDone = done
		go func() {
			t := time.NewTicker(opts.PingEvery)
			defer t.Stop()
			for {
				select {
				case <-t.C:
					if err := ws.writeControl(websocket.PingMessage, nil); err != nil {
						return
					}
				case <-done:
					return
				}
			}
		}()
	}
	return ws, nil
}

func (ws *WSClient) writeControl(typ int, data []byte) error {
	ws.writeMu.Lock()
	defer ws.writeMu.Unlock()
	return ws.conn.WriteControl(typ, data, time.Now().Add(5*time.Second))
}

// Subscribe sends a JSON-RPC subscribe for a query like `tm.event='NewBlock'`.
func (ws *WSClient) Subscribe(ctx context.Context, query string) error {
	msg := map[string]any{
		"jsonrpc": "2.0",
		"method":  "subscribe",
		"id":      1,
		"params":  map[string]string{"query": query},
	}
	ws.writeMu.Lock()
	defer ws.writeMu.Unlock()
	if err := ws.conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return err
	}
	return ws.conn.WriteJSON(msg)
}

// URL returns the connected URL (credentials redacted).
func (ws *WSClient) URL() string { return ws.url }

// ReadEvent blocks for the next subscription event.
func (ws *WSClient) ReadEvent() (*WSEvent, error) {
	_, msg, err := ws.conn.ReadMessage()
	if err != nil {
		return nil, err
	}
	// skip subscribe ACKs etc. that carry no result data
	ev := &WSEvent{}
	if err := json.Unmarshal(msg, ev); err != nil {
		return nil, nil //nolint:nilerr -- unparseable frame, skip it
	}
	if ev.Type() == "" {
		return ws.ReadEvent()
	}
	return ev, nil
}

// Close terminates the websocket.
func (ws *WSClient) Close() error {
	if ws.pingDone != nil {
		close(ws.pingDone)
	}
	ws.writeMu.Lock()
	defer ws.writeMu.Unlock()
	_ = ws.conn.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, "bye"), time.Now().Add(2*time.Second))
	return ws.conn.Close()
}
