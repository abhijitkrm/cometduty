// Package logging wires slog into cometduty: structured logs to stderr plus a
// non-blocking fan-out to the dashboard's live log feed.
package logging

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/abhijitkrm/cometduty/internal/broker"
)

// Entry is one formatted log line for the dashboard feed.
type Entry struct {
	Ts    int64  `json:"ts"`
	Level string `json:"level"`
	Msg   string `json:"msg"`
}

// LogBus carries formatted log entries to dashboard clients.
var LogBus = broker.New[Entry]()

type fanoutHandler struct {
	inner slog.Handler
}

func (h *fanoutHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h *fanoutHandler) Handle(ctx context.Context, r slog.Record) error {
	msg := r.Message
	r.Attrs(func(a slog.Attr) bool {
		msg += " " + a.Key + "=" + a.Value.String()
		return true
	})
	LogBus.Publish(Entry{Ts: time.Now().Unix(), Level: r.Level.String(), Msg: msg})
	return h.inner.Handle(ctx, r)
}

func (h *fanoutHandler) WithAttrs(a []slog.Attr) slog.Handler {
	return &fanoutHandler{inner: h.inner.WithAttrs(a)}
}

func (h *fanoutHandler) WithGroup(g string) slog.Handler {
	return &fanoutHandler{inner: h.inner.WithGroup(g)}
}

// Setup installs the default logger. jsonOut selects JSON formatting.
func Setup(level slog.Level, jsonOut bool) {
	opts := &slog.HandlerOptions{Level: level}
	var inner slog.Handler
	if jsonOut {
		inner = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		inner = slog.NewTextHandler(os.Stderr, opts)
	}
	slog.SetDefault(slog.New(&fanoutHandler{inner: inner}))
}
