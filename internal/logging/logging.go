// Package logging wires slog into cometduty: structured logs to stderr.
package logging

import (
	"log/slog"
	"os"
)

// Setup installs the default logger. jsonOut selects JSON formatting.
func Setup(level slog.Level, jsonOut bool) {
	opts := &slog.HandlerOptions{Level: level}
	var inner slog.Handler
	if jsonOut {
		inner = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		inner = slog.NewTextHandler(os.Stderr, opts)
	}
	slog.SetDefault(slog.New(inner))
}
