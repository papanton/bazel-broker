// Package registry holds the in-memory set of known builds. E0 ships the
// interface and a no-op impl so imports resolve; E2 (registration/snapshot) and
// E3 (discovery merge) provide the real behavior.
package registry

import "github.com/antoniospapantoniou/bazel-broker/internal/build"

// Registry is the set of builds the broker knows about. E2/E3 expand this.
type Registry interface {
	// List returns a snapshot of all known builds.
	List() []build.Build
}

// Noop is a placeholder Registry that knows about nothing. Replaced in E2.
type Noop struct{}

// List returns no builds.
func (Noop) List() []build.Build { return nil }
