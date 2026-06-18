// Package build holds the in-memory domain object for a Bazel build.
//
// This is an E0 stub matching the shapes E2 (the authoritative API contract)
// commits to. E2 fills in behavior; E0 only freezes the package name, the
// State/Source enum string values, and the Build field set so that every later
// epic imports a stable path without a rename churn.
//
// Note the two layers kept distinct on purpose: this domain object plus the
// flat JSON DTO in internal/api (api.Build). The DTO is what crosses the wire;
// fields like PID/proc handles that E3/E4 add stay off the wire.
package build

import "time"

// State is the lifecycle state of a build. The string values are the wire
// contract, frozen by E2 (§2.2/§4.1).
type State string

const (
	StateQueued   State = "queued"   // admitted-pending (E5); E2 never sets it itself
	StateRunning  State = "running"  // actively building
	StateFinished State = "finished" // exited 0
	StateFailed   State = "failed"   // exited non-zero
	StateKilled   State = "killed"   // terminated by the broker (E3)
	StateUnknown  State = "unknown"  // discovered process whose outcome we never observed
)

// Source records how the build entered the registry.
type Source string

const (
	SourceRegistered Source = "registered" // via POST /register
	SourceDiscovered Source = "discovered" // via process discovery (E3)
)

// Build is the in-memory domain object. Fields the wire DTO omits (proc handle,
// bep/profile paths) are added by E3/E4 — kept off the wire so internal state
// never leaks to clients.
type Build struct {
	InvocationID string
	Worktree     string
	Targets      []string
	PID          int
	State        State
	StartTime    time.Time
	EndTime      time.Time // zero until terminal
	ExitCode     int       // valid only in terminal states
	Source       Source
}

// Elapsed returns wall time, ending at EndTime if terminal else at now.
func (b Build) Elapsed(now time.Time) time.Duration {
	end := now
	if !b.EndTime.IsZero() {
		end = b.EndTime
	}
	return end.Sub(b.StartTime)
}

// ToWire maps the domain object to its flat JSON DTO.
//
// E0 stub: returns nil. E2 T1 implements this to return an api.Build. It is
// declared here so the seam exists; the return type is `any` only to keep E0
// free of an import on internal/api's final shape.
func (b Build) ToWire(now time.Time) any { return nil }
