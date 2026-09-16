// Package dashboard serves the status UI: a static SPA plus a websocket feed of
// status updates and (optionally) log lines.
package dashboard

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/abhijitkrm/cometduty/internal/broker"
	"github.com/abhijitkrm/cometduty/internal/logging"
	"github.com/abhijitkrm/cometduty/internal/monitor"
)

//go:embed static
var staticFS embed.FS

// urlRex matches endpoint-looking strings for redaction.
var urlRex = regexp.MustCompile(`\W(https?|tcp|wss?)://[^\s]+`)

// Hub implements monitor.Hub and feeds the websocket broadcast.
type Hub struct {
	mu     sync.Mutex
	status map[string]*monitor.Status
	order  []string

	bus      *broker.Broker[[]byte]
	hideLogs bool
}

// NewHub builds a hub. hideLogs suppresses the log feed and scrubs endpoint
// URLs out of error strings (for publicly reachable dashboards).
func NewHub(hideLogs bool) *Hub {
	return &Hub{status: map[string]*monitor.Status{}, bus: broker.New[[]byte](), hideLogs: hideLogs}
}

// PublishStatus records and broadcasts a chain status update.
func (h *Hub) PublishStatus(s *monitor.Status) {
	h.mu.Lock()
	if h.hideLogs {
		s.LastError = urlRex.ReplaceAllString(s.LastError, " -redacted-")
	}
	h.status[s.Name+"|"+s.Validator] = s
	h.mu.Unlock()
	h.broadcastState()
}

// Log is part of monitor.Hub (unused; logs flow via logging.LogBus).
func (h *Hub) Log(e any) {}

func (h *Hub) broadcastState() {
	h.mu.Lock()
	list := make([]*monitor.Status, 0, len(h.status))
	for _, s := range h.status {
		list = append(list, s)
	}
	h.mu.Unlock()
	sort.Slice(list, func(i, j int) bool {
		if list[i].Name == list[j].Name {
			return list[i].Validator < list[j].Validator
		}
		return list[i].Name < list[j].Name
	})
	b, err := json.Marshal(struct {
		MsgType string            `json:"msgType"`
		Status  []*monitor.Status `json:"Status"`
	}{MsgType: "update", Status: list})
	if err == nil {
		h.bus.Publish(b)
	}
}

// stateJSON returns the cached full-state blob.
func (h *Hub) stateJSON() []byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	list := make([]*monitor.Status, 0, len(h.status))
	for _, s := range h.status {
		list = append(list, s)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	b, _ := json.Marshal(struct {
		MsgType string            `json:"msgType"`
		Status  []*monitor.Status `json:"Status"`
	}{MsgType: "update", Status: list})
	return b
}

// Server is the dashboard HTTP server.
type Server struct {
	hub      *Hub
	bind     string
	port     int
	user     string
	pass     string
	hideLogs bool
	srv      *http.Server
}

// NewServer builds the server.
func NewServer(hub *Hub, bind string, port int, user, pass string, hideLogs bool) *Server {
	return &Server{hub: hub, bind: bind, port: port, user: user, pass: pass, hideLogs: hideLogs}
}

var upgrader = websocket.Upgrader{
	CheckOrigin:       func(*http.Request) bool { return true },
	EnableCompression: true,
}

// Start runs until ctx is canceled.
func (s *Server) Start(ctx context.Context) error {
	root, err := fs.Sub(staticFS, "static")
	if err != nil {
		return err
	}
	mux := http.NewServeMux()

	auth := func(h http.HandlerFunc) http.HandlerFunc {
		if s.user == "" {
			return h
		}
		return func(w http.ResponseWriter, r *http.Request) {
			u, p, ok := r.BasicAuth()
			if !ok || u != s.user || p != s.pass {
				w.Header().Set("WWW-Authenticate", `Basic realm="cometduty"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			h(w, r)
		}
	}

	mux.HandleFunc("/ws", auth(s.serveWS))
	mux.HandleFunc("/state", auth(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(s.hub.stateJSON())
	}))
	mux.HandleFunc("/logsenabled", auth(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]bool{"enabled": !s.hideLogs})
	}))
	mux.HandleFunc("/logs", auth(s.serveLogs))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.Handle("/", auth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=300")
		http.FileServer(http.FS(root)).ServeHTTP(w, r)
	}))

	s.srv = &http.Server{
		Addr:              fmt.Sprintf("%s:%d", s.bind, s.port),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- s.srv.ListenAndServe() }()
	slog.Info("dashboard listening", "addr", s.srv.Addr)
	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.srv.Shutdown(shutCtx)
		return nil
	case err := <-errCh:
		return err
	}
}

func (s *Server) serveLogs(w http.ResponseWriter, _ *http.Request) {
	if s.hideLogs {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
		return
	}
	// recent logs are not retained server-side; the client shows the live feed
	// plus whatever it buffered. Return the last 256 from the ring we keep.
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(logCacheJSON())
}

var (
	logCacheMu sync.Mutex
	logCache   []logging.Entry
)

func init() {
	// keep a small ring of recent log entries for the /logs endpoint
	id, ch := logging.LogBus.Subscribe(512)
	_ = id
	go func() {
		for e := range ch {
			logCacheMu.Lock()
			logCache = append([]logging.Entry{e}, logCache...)
			if len(logCache) > 256 {
				logCache = logCache[:256]
			}
			logCacheMu.Unlock()
		}
	}()
}

func logCacheJSON() []byte {
	logCacheMu.Lock()
	defer logCacheMu.Unlock()
	b, _ := json.Marshal(logCache)
	return b
}

// serveWS streams status updates and log entries to a dashboard client.
func (s *Server) serveWS(w http.ResponseWriter, r *http.Request) {
	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = c.Close() }()

	// send current state immediately so the UI isn't empty
	if err := c.WriteMessage(websocket.TextMessage, s.hub.stateJSON()); err != nil {
		return
	}

	id, updates := s.hub.bus.Subscribe(64)
	defer s.hub.bus.Unsubscribe(id)

	var logID int
	var logs <-chan logging.Entry
	if !s.hideLogs {
		logID, logs = logging.LogBus.Subscribe(256)
		defer logging.LogBus.Unsubscribe(logID)
	}

	for {
		select {
		case <-r.Context().Done():
			return
		case msg, ok := <-updates:
			if !ok {
				return
			}
			if err := c.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case e, ok := <-logs:
			if !ok {
				logs = nil
				continue
			}
			b, _ := json.Marshal(struct {
				MsgType string        `json:"msgType"`
				Entry   logging.Entry `json:"entry"`
			}{MsgType: "log", Entry: e})
			if err := c.WriteMessage(websocket.TextMessage, b); err != nil {
				return
			}
		}
	}
}
