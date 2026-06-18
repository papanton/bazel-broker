// Package store persists builds and metrics to SQLite. E0 ships the interface
// and a no-op impl; E2 owns schema v1, E4 adds the metrics tables.
package store

import "github.com/antoniospapantoniou/bazel-broker/internal/build"

// Store persists terminal builds (and, later, metrics). E2/E4 expand this.
type Store interface {
	// Save records a build. E0 no-op; E2 writes to SQLite.
	Save(b build.Build) error
	// Close releases the underlying handle.
	Close() error
}

// Noop is a placeholder Store that drops everything. Replaced in E2.
type Noop struct{}

// Save discards the build.
func (Noop) Save(build.Build) error { return nil }

// Close is a no-op.
func (Noop) Close() error { return nil }
