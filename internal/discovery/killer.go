package discovery

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/antoniospapantoniou/bazel-broker/internal/api"
	"github.com/antoniospapantoniou/bazel-broker/internal/registry"
)

// Killer implements httpapi.Killer: it owns POST /builds/{invocation_id}/kill. It
// resolves the path id to a registry build, runs the SIGINT/SIGTERM -> grace ->
// SIGKILL state machine against the owned client PID, and returns api.KillResult JSON.
type Killer struct {
	reg     *registry.Registry
	cfg     KillConfig
	log     *slog.Logger
	freshen func(context.Context) error // optional reconcile-on-demand before lookup (keeps ExePath fresh)
}

// NewKiller builds a Killer. log may be nil. freshen may be nil (no reconcile-on-demand);
// pass disco.ReconcileOnce to keep the captured ExePath/PID fresh before each kill lookup.
func NewKiller(reg *registry.Registry, cfg KillConfig, log *slog.Logger, freshen func(context.Context) error) *Killer {
	if log == nil {
		log = slog.Default()
	}
	return &Killer{reg: reg, cfg: cfg, log: log, freshen: freshen}
}

// Kill handles POST /builds/{invocation_id}/kill. The id is read from the path; the
// optional JSON body may carry {"force":true} / {"use_cancel":true} (use_cancel is a
// no-op for the passive broker today — see D4). Resolution: registry lookup by
// invocation_id -> owned PID. Killing an unknown id is 404 (registry membership is the
// authorization boundary in this sudo-free, single-user tool).
func (k *Killer) Kill(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("invocation_id")
	if id == "" {
		writeKillError(w, http.StatusBadRequest, "bad_request", "invocation_id required")
		return
	}

	// Optional body knobs.
	var body struct {
		Force     bool `json:"force"`
		UseCancel bool `json:"use_cancel"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body) // tolerate empty/missing body
	}

	// Reconcile-on-demand so the captured ExePath/PID is no staler than this request.
	if k.freshen != nil {
		if err := k.freshen(r.Context()); err != nil {
			k.log.Warn("kill reconcile-on-demand failed", "invocation_id", id, "err", err)
		}
	}

	b, ok := k.reg.FindByInvocationID(id)
	if !ok {
		writeKillError(w, http.StatusNotFound, "not_found", "no build matching "+id)
		return
	}
	if b.PID <= 0 {
		writeKillError(w, http.StatusConflict, "no_pid", "build "+id+" has no known pid to kill")
		return
	}

	cfg := k.cfg
	if body.UseCancel {
		cfg.UseCancel = true
	}

	start := time.Now()
	outcome, err := killProc(r.Context(), cfg, KillSpec{
		PID:       b.PID,
		ExpectExe: b.ExePath,
		Force:     body.Force,
	})
	elapsed := time.Since(start)

	killed := outcome != OutcomeError
	if err != nil {
		k.log.Warn("kill failed", "invocation_id", id, "pid", b.PID, "outcome", string(outcome), "err", err)
		writeKill(w, http.StatusInternalServerError, api.KillResult{
			Killed:       false,
			InvocationID: id,
			PID:          b.PID,
			Outcome:      string(OutcomeError),
			ElapsedMS:    elapsed.Milliseconds(),
		})
		return
	}

	k.log.Info("build killed", "invocation_id", id, "pid", b.PID,
		"outcome", string(outcome), "elapsed_ms", elapsed.Milliseconds())
	writeKill(w, http.StatusOK, api.KillResult{
		Killed:       killed,
		InvocationID: id,
		PID:          b.PID,
		Outcome:      string(outcome),
		ElapsedMS:    elapsed.Milliseconds(),
	})
}

func writeKill(w http.ResponseWriter, status int, res api.KillResult) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(res)
}

func writeKillError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(api.ErrorResponse{Error: code, Message: msg})
}
