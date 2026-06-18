// Package logging provides the slog convention every binary shares: a JSON
// handler whose level comes from a string / env var. Convention: build-scoped
// log lines carry the structured key "invocation_id".
package logging

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// EnvLevel selects the log level for FromEnv ("debug"|"info"|"warn"|"error").
const EnvLevel = "BAZEL_BROKER_LOG_LEVEL"

// NewFile returns a logger that writes JSON to both the file at path (created,
// appended, 0600; parent dir 0700) and os.Stderr (so launchd captures it too).
// If the file cannot be opened it falls back to stderr-only. The level comes
// from $BAZEL_BROKER_LOG_LEVEL (default info).
func NewFile(path string) *slog.Logger {
	level := os.Getenv(EnvLevel)
	if path == "" {
		return New(os.Stderr, level)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return New(os.Stderr, level)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return New(os.Stderr, level)
	}
	return New(io.MultiWriter(f, os.Stderr), level)
}

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
