package admission

import (
	"encoding/json"
	"io"
	"net/http"
)

// Admitter is the httpapi.Admitter implementation for E5. It owns request
// parsing/response encoding for the admission routes. POST /admission returns a
// STATUS CODE + ONE-WORD TEXT BODY (never JSON) per conflict C5.
type Admitter struct {
	engine *Engine
}

// NewAdmitter wraps an engine as the httpapi.Admitter seam.
func NewAdmitter(e *Engine) *Admitter { return &Admitter{engine: e} }

// Admit handles POST /admission. It blocks server-side up to PollSeconds (the
// engine's long-poll), then answers:
//
//	200 ALLOW  — admitted, slot held for this invocation_id
//	202 QUEUE  — wait window expired, still queued; the wrapper re-POSTs
//	403 DENY   — drain / hard policy denial
//
// The body is a single uppercase word; queue/position detail rides in X-Broker-*
// headers, which the bash wrapper may read cheaply or ignore.
func (a *Admitter) Admit(w http.ResponseWriter, r *http.Request) {
	var req Request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.InvocationID == "" {
		// Bad request -> the wrapper treats any non-200/202/403 as fail-open.
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, "BAD")
		return
	}
	v := a.engine.Admit(r.Context(), req)

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	switch v {
	case Allow:
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ALLOW")
	case Queue:
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, "QUEUE")
	case Deny:
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, "DENY")
	}
}

// Release handles POST /admission/release {invocation_id} -> 204. Idempotent:
// releasing an unknown/already-released id is a 204 no-op. Primary slot-release
// path; the registry-terminal-event loop and PID reaper are the backstops.
func (a *Admitter) Release(w http.ResponseWriter, r *http.Request) {
	var req struct {
		InvocationID string `json:"invocation_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.InvocationID != "" {
		a.engine.Release(req.InvocationID)
	}
	w.WriteHeader(http.StatusNoContent)
}

// Pause handles POST /admission/pause -> 200. Holds all admissions (no DENY).
func (a *Admitter) Pause(w http.ResponseWriter, _ *http.Request) {
	a.engine.SetPaused(true)
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "PAUSED")
}

// Resume handles POST /admission/resume -> 200. Clears BOTH pause and drain.
func (a *Admitter) Resume(w http.ResponseWriter, _ *http.Request) {
	a.engine.Resume()
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "RESUMED")
}

// Drain handles POST /admission/drain -> 200. DENYs queued + new waiters;
// in-flight builds finish naturally.
func (a *Admitter) Drain(w http.ResponseWriter, _ *http.Request) {
	a.engine.SetDraining(true)
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "DRAINING")
}

// Status handles GET /admission/status -> 200 JSON.
func (a *Admitter) Status(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(a.engine.Status())
}
