// Package build holds the in-memory domain object for a Bazel build.
//
// Two layers are kept distinct on purpose: this rich domain object (build.Build)
// plus the flat JSON DTO in internal/api (api.Build). The DTO is what crosses the
// wire; internal-only fields (proc handle, discovery seam fields, bep/profile
// paths) stay off the wire. A single ToAPI mapper bridges the two so the wire
// contract can never accidentally leak internal fields.
//
// E2 §2.2 is authoritative for the field set and the State/Source enum strings.
// The discovery seam fields (ExePath/Cwd/GitDir/WorktreeName/LastSeen) and the
// `gone` state are declared here so E3 adds *behavior*, not *schema*.
package build

import (
	"time"

	"github.com/antoniospapantoniou/bazel-broker/internal/api"
)

// State is the lifecycle state of a build. The string values are the wire
// contract, frozen by E2 (§2.2/§4.1).
type State string

const (
	StateQueued   State = "queued"   // admitted-pending (E5); E2 never sets it itself
	StateRunning  State = "running"  // actively building (NOT "building")
	StateFinished State = "finished" // exited 0
	StateFailed   State = "failed"   // exited non-zero
	StateKilled   State = "killed"   // terminated by the broker (E3)
	StateGone     State = "gone"     // discovered process whose PID vanished, outcome unseen (E3 reap)
	StateUnknown  State = "unknown"  // forward-compat fallback any client maps unrecognized states to
)

// IsTerminal reports whether s is an end state (no further transitions).
func (s State) IsTerminal() bool {
	switch s {
	case StateFinished, StateFailed, StateKilled, StateGone:
		return true
	default:
		return false
	}
}

// Source records how the build entered the registry.
type Source string

const (
	SourceRegistered Source = "registered" // via POST /register
	SourceDiscovered Source = "discovered" // via process discovery (E3)
)

// Build is the in-memory domain object. Fields below the "discovery seam" line
// are populated by later epics; only those noted in §4.1 ever reach the wire.
type Build struct {
	InvocationID string   // Bazel invocation_id (uuid). Primary key. Required.
	Worktree     string   // absolute path of the git worktree (build cwd)
	WorktreeName string   // last path component (display); E3 fills, "" until then
	Command      string   // bazel command verb: build|run|test|… (E4 from BuildStarted)
	Targets      []string // bazel targets/patterns, e.g. ["//app:App"]
	PID          int      // bazel CLIENT pid (0 if unknown at register time)
	State        State
	StartTime    time.Time // when the broker first saw it (canonical "first seen")
	EndTime      time.Time // zero until terminal
	ExitCode     int       // valid only in terminal states; 0 otherwise
	Source       Source

	// ---- discovery seam (E3 populates; not serialized by E2) ----
	ExePath  string    // E3: proc_pidpath — client/server filtering, display
	Cwd      string    // E3: PROC_PIDVNODEPATHINFO — worktree resolution input
	GitDir   string    // E3: resolved .git dir — output-base lookup for D4 Cancel
	LastSeen time.Time // E3: last reconcile pass that saw this PID (reap/staleness)

	// ---- E4 enrichment (BEP metrics; serialized to api.Build) ----
	CacheHitRatio *float64 // disk-cache hit ratio 0..1 from runner_count[] (nil until BEP metrics land)
	ProfileURL    string   // ready-to-open Perfetto shim URL (E4)
}

// Elapsed returns wall time, ending at EndTime if terminal else at now.
func (b Build) Elapsed(now time.Time) time.Duration {
	end := now
	if !b.EndTime.IsZero() {
		end = b.EndTime
	}
	return end.Sub(b.StartTime)
}

// ToAPI maps the domain object to its flat JSON DTO (api.Build). `now` is used to
// compute elapsed_ms for non-terminal builds. Timestamps are emitted as RFC3339
// in UTC; end_time is left empty (omitted) until the build is terminal.
func (b Build) ToAPI(now time.Time) api.Build {
	out := api.Build{
		InvocationID: b.InvocationID,
		Worktree:     b.Worktree,
		WorktreeName: b.WorktreeName,
		Command:      b.Command,
		Targets:      b.Targets,
		PID:          b.PID,
		State:        string(b.State),
		StartTime:    api.FormatTime(b.StartTime),
		EndTime:      api.FormatTime(b.EndTime), // "" (omitted) until terminal
		ExitCode:     b.ExitCode,
		Source:       string(b.Source),
		ElapsedMS:    b.Elapsed(now).Milliseconds(),
	}
	if out.Targets == nil {
		out.Targets = []string{} // always present; [] not null
	}
	out.CacheHitRatio = b.CacheHitRatio // E4 enrichment; nil → omitted
	out.ProfileURL = b.ProfileURL       // E4 enrichment; "" → omitted
	return out
}
