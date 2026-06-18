// Package logging provides the slog convention every binary shares: a JSON
// handler whose level comes from a string / env var. Convention: build-scoped
// log lines carry the structured key "invocation_id".
package logging

import (
	"io"
	"log/slog"
	"os"
	"strings"
)

// EnvLevel selects the log level for FromEnv ("debug"|"info"|"warn"|"error").
const EnvLevel = "BAZEL_BROKER_LOG_LEVEL"

// New returns a *slog.Logger writing JSON to w at the given level. Unknown
// levels fall back to info.
func New(w io.Writer, level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: lvl, AddSource: lvl == slog.LevelDebug})
	return slog.New(h)
}

// FromEnv reads BAZEL_BROKER_LOG_LEVEL (default info) for quick verify toggling.
func FromEnv(w io.Writer) *slog.Logger { return New(w, os.Getenv(EnvLevel)) }
