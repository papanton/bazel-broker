// Package httpapi serves the broker's loopback HTTP + WebSocket API (E2 §2.6).
//
// The Server is the extension point for every later epic: it holds the injected
// Killer (E3), MetricsProvider (E4) and Admitter (E5) seams, defaulting to 501
// stubs. A downstream epic provides a real implementation via the With* options
// (see seams.go) without touching main.go's routing.
package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/antoniospapantoniou/bazel-broker/internal/api"
	"github.com/antoniospapantoniou/bazel-broker/internal/config"
	"github.com/antoniospapantoniou/bazel-broker/internal/registry"
)

// Server is the broker's HTTP front.
type Server struct {
	cfg     *config.Config
	reg     *registry.Registry
	hub     *registry.Hub
	log     *slog.Logger
	http    *http.Server
	started time.Time
	version string // broker build version reported by /healthz

	// extension seams (default 501 stubs until a later epic injects a real impl)
	killer   Killer
	metrics  MetricsProvider
	admitter Admitter
}

// New constructs a Server. Reserved routes default to 501 stubs; pass With*
// options to inject the real E3/E4/E5 handlers.
func New(cfg *config.Config, reg *registry.Registry, hub *registry.Hub, log *slog.Logger, opts ...Option) *Server {
	if log == nil {
		log = slog.Default()
	}
	s := &Server{
		cfg:      cfg,
		reg:      reg,
		hub:      hub,
		log:      log,
		started:  time.Now(),
		version:  defaultVersion,
		killer:   default501{epic: "E3"},
		metrics:  default501{epic: "E4"},
		admitter: default501{epic: "E5"},
	}
	for _, o := range opts {
		o(s)
	}
	s.http = &http.Server{Handler: s.Routes()}
	return s
}

// Routes builds the mux + middleware chain. Exported so tests (httptest) and
// later epics can mount the handler directly.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	// --- E2 live routes ---
	mux.HandleFunc("GET /healthz", s.handleHealthz) // auth-exempt
	mux.HandleFunc("GET /builds", s.handleListBuilds)
	mux.HandleFunc("GET /builds/{invocation_id}", s.handleGetBuild)
	mux.HandleFunc("POST /builds", s.handleRegister) // register lifecycle
	mux.HandleFunc("DELETE /builds/{invocation_id}", s.handleDeregisterPath)
	mux.HandleFunc("POST /register", s.handleRegister) // explicit alias (contract §4.2)
	mux.HandleFunc("POST /deregister", s.handleDeregister)
	mux.HandleFunc("GET /events", s.handleEvents) // WebSocket

	// --- E3 reserved: kill ---
	mux.HandleFunc("POST /builds/{invocation_id}/kill", s.killer.Kill)

	// --- E4 reserved: metrics / profile ---
	mux.HandleFunc("GET /builds/{invocation_id}/metrics", s.metrics.BuildMetrics)
	// NOTE (OD-C): when E4 lands, GET /builds/{id}/profile and the cross-origin
	// GET /profile/{id}/{name} that ui.perfetto.dev fetches must be token-exempt
	// + Origin-restricted CORS (Perfetto cannot send the bearer header). The auth
	// middleware exposes the `authExempt`/`originRestricted` seam for that; E4
	// flips these two paths there.
	mux.HandleFunc("GET /builds/{invocation_id}/profile", s.metrics.BuildProfile)
	mux.HandleFunc("GET /metrics", s.metrics.MetricsList)
	mux.HandleFunc("GET /profile/{invocation_id}/{name}", s.metrics.ProfileFile)
	mux.HandleFunc("GET /diskcache", s.metrics.DiskCache)

	// --- E5 reserved: admission ---
	mux.HandleFunc("POST /admission", s.admitter.Admit) // status-code + one-word body (NOT JSON)
	mux.HandleFunc("POST /admission/release", s.admitter.Release)
	mux.HandleFunc("POST /admission/pause", s.admitter.Pause)
	mux.HandleFunc("POST /admission/resume", s.admitter.Resume)
	mux.HandleFunc("POST /admission/drain", s.admitter.Drain)
	mux.HandleFunc("GET /admission/status", s.admitter.Status)

	// middleware chain (outer -> inner): recover -> log -> auth
	return s.recoverer(s.requestLog(s.auth(mux)))
}

// Serve runs the HTTP server on ln until Shutdown is called.
func (s *Server) Serve(ln net.Listener) error {
	err := s.http.Serve(ln)
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

// Shutdown gracefully drains in-flight requests.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.http.Shutdown(ctx)
}

// --- live handlers ---

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	c := s.reg.Counts()
	writeJSON(w, http.StatusOK, api.HealthResponse{
		Status:  "ok",
		Builds:  c.Building,
		Queued:  c.Queued,
		Total:   c.Total,
		Version: s.version,
		Uptime:  time.Since(s.started).Milliseconds(),
	})
}

func (s *Server) handleListBuilds(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, api.BuildsResponse{Builds: s.reg.SnapshotAPI()})
}

func (s *Server) handleGetBuild(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("invocation_id")
	b, ok := s.reg.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, api.ErrorResponse{Error: "not_found"})
		return
	}
	writeJSON(w, http.StatusOK, api.BuildResponse{Build: b.ToAPI(time.Now())})
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req api.RegisterRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.InvocationID == "" || req.Worktree == "" {
		writeError(w, http.StatusBadRequest, api.ErrorResponse{
			Error: "bad_request", Message: "invocation_id and worktree are required",
		})
		return
	}
	b := toDomain(req)
	stored, err := s.reg.Register(b)
	if err != nil {
		writeError(w, http.StatusBadRequest, api.ErrorResponse{Error: "bad_request", Message: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, api.BuildResponse{Build: stored.ToAPI(time.Now())})
}

func (s *Server) handleDeregister(w http.ResponseWriter, r *http.Request) {
	var req api.DeregisterRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.InvocationID == "" {
		writeError(w, http.StatusBadRequest, api.ErrorResponse{
			Error: "bad_request", Message: "invocation_id is required",
		})
		return
	}
	stored, err := s.reg.Deregister(req.InvocationID, req.ExitCode)
	if err != nil {
		writeError(w, http.StatusBadRequest, api.ErrorResponse{Error: "bad_request", Message: err.Error()})
		return
	}
	if stored == nil {
		writeError(w, http.StatusNotFound, api.ErrorResponse{Error: "not_found"})
		return
	}
	writeJSON(w, http.StatusOK, api.BuildResponse{Build: stored.ToAPI(time.Now())})
}

// handleDeregisterPath backs DELETE /builds/{invocation_id} (exit 0 by default,
// override with ?exit_code=N).
func (s *Server) handleDeregisterPath(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("invocation_id")
	exit := 0
	if v := r.URL.Query().Get("exit_code"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			exit = n
		}
	}
	stored, err := s.reg.Deregister(id, exit)
	if err != nil {
		writeError(w, http.StatusBadRequest, api.ErrorResponse{Error: "bad_request", Message: err.Error()})
		return
	}
	if stored == nil {
		writeError(w, http.StatusNotFound, api.ErrorResponse{Error: "not_found"})
		return
	}
	writeJSON(w, http.StatusOK, api.BuildResponse{Build: stored.ToAPI(time.Now())})
}

// --- small helpers ---

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, e api.ErrorResponse) {
	writeJSON(w, status, e)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, api.ErrorResponse{Error: "bad_request", Message: "invalid JSON body"})
		return false
	}
	return true
}
