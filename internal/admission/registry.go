package admission

import "time"

// ActiveBuild is the slice of a registry build that the gates care about. It is
// deliberately decoupled from internal/build.Build so the engine and its tests
// do not pull in the whole registry/store stack.
type ActiveBuild struct {
	InvocationID string
	Targets      []string
	PID          int
	StartedAt    time.Time
}

// RegistrySnapshot is the gate-visible view of "what is building right now". It
// includes passively-discovered (un-wrapped) builds so stagger / headroom
// account for builds that never hit /admission.
type RegistrySnapshot struct {
	Builds []ActiveBuild
	// RecentlyAdmitted counts builds admitted by THIS engine since the last load
	// sample whose clang/swiftc spawn has not yet shown up in CPU load. The token
	// bucket debits a synthetic per-build load against it to avoid admitting a
	// herd inside one ~1s sample window.
	RecentlyAdmitted int
}

// RegistryReader is the E3/E2 registry seam the engine reads. *registry.Registry
// satisfies the adapter in wiring.go; tests supply a fake.
type RegistryReader interface {
	// SnapshotActive returns the currently non-terminal builds.
	SnapshotActive() RegistrySnapshot
	// Alive reports whether pid is a live process (PID-liveness reaper backstop).
	Alive(pid int) bool
}

// TerminalEvents is the registry seam that tells the engine a build finished so
// it can release the slot server-side (the exec-skips-trap fix). Implementations
// fan terminal transitions (deregister / BEP finished / process-gone reconcile)
// onto the returned channel as invocation ids.
type TerminalEvents interface {
	// TerminalIDs returns a channel that emits the invocation_id of each build
	// that reached a terminal state, and a cancel func to unsubscribe.
	TerminalIDs() (<-chan string, func())
}
