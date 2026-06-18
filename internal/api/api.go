// Package api holds the JSON DTOs exchanged over the broker's HTTP/WS API.
//
// This is the importable wire contract: brokerctl (E6), the web dashboard (E7),
// and the menu-bar app (E8) all decode these shapes. E0 ships the struct so the
// import path exists from day one; E2 §4.1 is authoritative for the final
// json tags and any added fields.
package api

// Build is the JSON DTO for one build (returned by /builds, /events, /register).
// FROZEN by E2 §4.1 — E0 ships the struct so the import path exists; E2 finalizes
// tags and adds fields (e.g. cache_hit_ratio) as the owning epics land.
type Build struct {
	InvocationID string   `json:"invocation_id"`
	Worktree     string   `json:"worktree"`
	Targets      []string `json:"targets"`
	PID          int      `json:"pid"`
	State        string   `json:"state"`
	StartTime    string   `json:"start_time"`         // RFC3339 UTC
	EndTime      string   `json:"end_time,omitempty"` // omitted until terminal
	ExitCode     int      `json:"exit_code"`
	Source       string   `json:"source"`
	ElapsedMS    int64    `json:"elapsed_ms"`
}

// State/Source string consts mirror build.* — kept here for client imports that
// should not depend on the internal/build domain package.
const (
	StateQueued   = "queued"
	StateRunning  = "running"
	StateFinished = "finished"
	StateFailed   = "failed"
	StateKilled   = "killed"
	StateUnknown  = "unknown"

	SourceRegistered = "registered"
	SourceDiscovered = "discovered"
)
