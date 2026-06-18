// Package api holds the JSON DTOs exchanged over the broker's HTTP/WS API.
//
// THIS IS THE FROZEN CROSS-EPIC WIRE CONTRACT (E2 §4.1). brokerctl (E6), the web
// dashboard (E7), and the menu-bar app (E8) all decode these shapes; their field
// names and the WS envelope MUST match this package verbatim. The canonical,
// byte-checked serializations live in testdata/api/*.json and are exercised by
// contract_test.go — if you change a json tag here, that test will fail until you
// regenerate the fixtures, which is exactly the drift guard we want.
//
// Field-name freezes that supersede earlier consumer guesses:
//   - `invocation_id`   (NOT "id")
//   - `start_time`      (NOT "started_at")
//   - state value `running` (NOT "building")
//   - `cache_hit_ratio` 0..1 float pointer (NOT "cache_hit_pct"/"cache_hit_rate")
//   - `profile_url`     a fully-formed URL clients just open (E4 populates it)
//
// WS envelope: EXACTLY TWO event types — "snapshot" (full list once on connect)
// and "build" (single build, upsert-by-invocation_id). "metrics"/"alert" are
// RESERVED for E4. Heartbeats are WS ping frames, never JSON events.
package api

import "time"

// FormatTime renders t as RFC3339 in UTC — the single canonical wire-timestamp
// format. Returns "" for the zero value (so omitempty fields are omitted). Every
// timestamp on the wire (Build.StartTime/EndTime, Event.Ts) goes through here so
// the format is defined in exactly one place.
func FormatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// Build is the JSON DTO for one build (returned by /builds, /builds/{id},
// /events, /register, /deregister). Field tags are FROZEN.
type Build struct {
	InvocationID string   `json:"invocation_id"`           // primary key everywhere; NOT "id"
	Worktree     string   `json:"worktree"`                // absolute path
	WorktreeName string   `json:"worktree_name,omitempty"` // display basename (E3 fills)
	Command      string   `json:"command,omitempty"`       // bazel verb: build|run|test|… (E4 fills)
	Targets      []string `json:"targets"`                 // always present; [] not null
	PID          int      `json:"pid"`
	State        string   `json:"state"`              // see State* consts
	StartTime    string   `json:"start_time"`         // RFC3339 UTC; NOT "started_at"
	EndTime      string   `json:"end_time,omitempty"` // RFC3339 UTC; omitted until terminal
	ExitCode     int      `json:"exit_code"`          // meaningful only when terminal
	Source       string   `json:"source"`             // "registered" | "discovered"
	ElapsedMS    int64    `json:"elapsed_ms"`         // now-StartTime (or EndTime-StartTime if terminal)

	// ---- enrichment, omitempty so E2-only builds stay valid; filled by E4 ----
	CacheHitRatio *float64 `json:"cache_hit_ratio,omitempty"` // 0.0-1.0; nil/absent until E4 reports
	ProfileURL    string   `json:"profile_url,omitempty"`     // ready-to-open Perfetto deep-link; E4
}

// State / Source string consts mirror build.* — kept here so client imports do
// not need to depend on the internal/build domain package.
const (
	StateQueued   = "queued"
	StateRunning  = "running"
	StateFinished = "finished"
	StateFailed   = "failed"
	StateKilled   = "killed"
	StateGone     = "gone"
	StateUnknown  = "unknown" // forward-compat fallback for clients

	SourceRegistered = "registered"
	SourceDiscovered = "discovered"
)

// ---- request / response bodies ----

// RegisterRequest is the POST /register body.
type RegisterRequest struct {
	InvocationID string   `json:"invocation_id"` // required
	Worktree     string   `json:"worktree"`      // required (absolute path)
	Targets      []string `json:"targets,omitempty"`
	PID          int      `json:"pid,omitempty"`
	Source       string   `json:"source,omitempty"` // default "registered"
}

// DeregisterRequest is the POST /deregister body.
type DeregisterRequest struct {
	InvocationID string `json:"invocation_id"` // required
	ExitCode     int    `json:"exit_code"`     // 0 => finished, non-0 => failed
}

// HealthResponse is the GET /healthz body. /healthz is auth-exempt.
type HealthResponse struct {
	Status  string `json:"status"` // "ok"
	Builds  int    `json:"builds"` // count in non-terminal states (building)
	Queued  int    `json:"queued"` // count in "queued" (always 0 until E5)
	Total   int    `json:"total"`  // all known builds in the registry
	Version string `json:"version"`
	Uptime  int64  `json:"uptime_ms"`
}

// BuildResponse wraps a single build (POST /register, /deregister, GET /builds/{id}).
type BuildResponse struct {
	Build Build `json:"build"`
}

// BuildsResponse is the GET /builds body: builds newest StartTime first.
type BuildsResponse struct {
	Builds []Build `json:"builds"`
}

// ErrorResponse is the body of every non-2xx response.
type ErrorResponse struct {
	Error   string `json:"error"`             // machine code
	Message string `json:"message,omitempty"` // human-friendly detail
	Epic    string `json:"epic,omitempty"`    // owning epic for 501 reserved routes
}

// ---- WS event envelope (FROZEN — exactly two live types) ----

// EventType is the WS event discriminator.
type EventType string

const (
	EventSnapshot EventType = "snapshot" // full list, sent once on connect (carries Builds)
	EventBuild    EventType = "build"    // a single build created/updated/terminated (carries Build)
	EventMetrics  EventType = "metrics"  // RESERVED for E4: metrics update keyed by invocation_id
	EventAlert    EventType = "alert"    // RESERVED for E4: low-cache-hit alert
)

// Event is one WS frame. Clients dispatch on Type: "snapshot" carries Builds,
// "build" carries a single Build to upsert by invocation_id. Seq is a
// per-connection monotonically increasing counter so a client can detect a gap.
type Event struct {
	Type   EventType `json:"type"`
	Seq    uint64    `json:"seq"`              // monotonically increasing per connection
	Build  *Build    `json:"build,omitempty"`  // set for type=="build"
	Builds []Build   `json:"builds,omitempty"` // set for type=="snapshot"
	Ts     string    `json:"ts"`               // RFC3339 UTC emit time

	// ---- additive E4 payloads (omitted for the frozen snapshot/build types) ----
	// These ride alongside the reserved EventMetrics/EventAlert discriminators and
	// are omitempty so they never appear on a snapshot/build frame (contract-safe).
	Metrics any         `json:"metrics,omitempty"` // set for type=="metrics" (E4 metrics JSON)
	Alert   *AlertEvent `json:"alert,omitempty"`   // set for type=="alert" (E4 low-hit alert)
}

// AlertEvent is the payload of a WS "alert" event (E4, reserved/additive).
type AlertEvent struct {
	InvocationID string `json:"invocation_id"`
	Worktree     string `json:"worktree,omitempty"`
	Alert        string `json:"alert"` // "low_cache_hit" | "cache_busting_suspected"
}

// ---- E3/E4 response bodies declared here so the shapes are frozen early ----

// KillResult is the POST /builds/{id}/kill response (E3 fills the handler).
type KillResult struct {
	Killed       bool   `json:"killed"`
	InvocationID string `json:"invocation_id"`
	PID          int    `json:"pid"`
	Outcome      string `json:"outcome"` // sigint | sigkill | cancelled | already_gone | error
	ElapsedMS    int64  `json:"elapsed_ms"`
}

// ProfileRef is the GET /builds/{id}/profile response (E4 fills the handler).
type ProfileRef struct {
	PerfettoURL string `json:"perfetto_url"` // ready-to-open ui.perfetto.dev deep-link
	LocalPath   string `json:"local_path"`   // absolute path to the --profile .gz (fallback)
}
