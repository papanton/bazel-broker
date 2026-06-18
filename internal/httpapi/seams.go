package httpapi

import (
	"net/http"

	"github.com/papanton/bazel-broker/internal/api"
)

// This file defines the EXTENSION SEAMS that later epics implement. The Server
// holds one of each interface; E2 wires in the default501 stubs so every
// reserved route returns HTTP 501 (not 404 — consumers like E6 degrade on 501).
//
// INTEGRATION RECIPE for a downstream epic:
//
//  1. Implement the relevant interface (Killer / MetricsProvider / Admitter) in
//     your own package.
//  2. Inject it via the Server option, e.g.:
//         srv := httpapi.New(cfg, reg, hub, log, httpapi.WithKiller(myKiller))
//  3. Done. The router already routes the path to your handler; you do not touch
//     main.go's routing nor any other epic's code. The path/method/auth contract
//     in api §4.2 is fixed.
//
// Each interface method is a plain http.Handler-style func so the epic owns its
// own request parsing/response encoding (e.g. /admission returns a status-code +
// one-word body, NOT JSON — see Admitter).

// Killer owns POST /builds/{invocation_id}/kill (E3). It returns api.KillResult
// JSON on success. The default stub 501s.
type Killer interface {
	// Kill terminates the build named by the {invocation_id} path value. The
	// handler reads the id via r.PathValue("invocation_id").
	Kill(w http.ResponseWriter, r *http.Request)
}

// MetricsProvider owns the E4 read routes: GET /builds/{id}/metrics,
// GET /builds/{id}/profile, GET /metrics, GET /profile/{id}/{name}, GET /diskcache.
// It also (eventually) sets Build.CacheHitRatio / Build.ProfileURL via the
// registry. The default stub 501s every route.
type MetricsProvider interface {
	BuildMetrics(w http.ResponseWriter, r *http.Request) // GET /builds/{invocation_id}/metrics
	BuildProfile(w http.ResponseWriter, r *http.Request) // GET /builds/{invocation_id}/profile
	MetricsList(w http.ResponseWriter, r *http.Request)  // GET /metrics
	ProfileFile(w http.ResponseWriter, r *http.Request)  // GET /profile/{invocation_id}/{name}
	DiskCache(w http.ResponseWriter, r *http.Request)    // GET /diskcache
}

// Admitter owns the E5 admission routes. NOTE: POST /admission returns a status
// code + a single-word text body (200 ALLOW / 202 QUEUE / 403 DENY), NOT JSON —
// the wrapper is bash 3.2 with no guaranteed jq. The default stub 501s.
type Admitter interface {
	Admit(w http.ResponseWriter, r *http.Request)   // POST /admission   -> 200 ALLOW|202 QUEUE|403 DENY
	Release(w http.ResponseWriter, r *http.Request) // POST /admission/release
	Pause(w http.ResponseWriter, r *http.Request)   // POST /admission/pause
	Resume(w http.ResponseWriter, r *http.Request)  // POST /admission/resume
	Drain(w http.ResponseWriter, r *http.Request)   // POST /admission/drain
	Status(w http.ResponseWriter, r *http.Request)  // GET  /admission/status
}

// default501 is the placeholder for every unimplemented seam. It returns a 501
// with the standard error body naming the owning epic so a probing client knows
// the capability exists but is not yet live.
type default501 struct{ epic string }

func (d default501) handle(w http.ResponseWriter, _ *http.Request) {
	writeError(w, http.StatusNotImplemented, api.ErrorResponse{
		Error:   "not_implemented",
		Message: "owned by " + d.epic,
		Epic:    d.epic,
	})
}

func (d default501) Kill(w http.ResponseWriter, r *http.Request)         { d.handle(w, r) }
func (d default501) BuildMetrics(w http.ResponseWriter, r *http.Request) { d.handle(w, r) }
func (d default501) BuildProfile(w http.ResponseWriter, r *http.Request) { d.handle(w, r) }
func (d default501) MetricsList(w http.ResponseWriter, r *http.Request)  { d.handle(w, r) }
func (d default501) ProfileFile(w http.ResponseWriter, r *http.Request)  { d.handle(w, r) }
func (d default501) DiskCache(w http.ResponseWriter, r *http.Request)    { d.handle(w, r) }
func (d default501) Admit(w http.ResponseWriter, r *http.Request)        { d.handle(w, r) }
func (d default501) Release(w http.ResponseWriter, r *http.Request)      { d.handle(w, r) }
func (d default501) Pause(w http.ResponseWriter, r *http.Request)        { d.handle(w, r) }
func (d default501) Resume(w http.ResponseWriter, r *http.Request)       { d.handle(w, r) }
func (d default501) Drain(w http.ResponseWriter, r *http.Request)        { d.handle(w, r) }
func (d default501) Status(w http.ResponseWriter, r *http.Request)       { d.handle(w, r) }

// Option configures a Server at construction (the seam-injection mechanism).
type Option func(*Server)

// WithKiller injects the E3 kill handler.
func WithKiller(k Killer) Option { return func(s *Server) { s.killer = k } }

// WithMetrics injects the E4 metrics/profile handlers.
func WithMetrics(m MetricsProvider) Option { return func(s *Server) { s.metrics = m } }

// WithAdmitter injects the E5 admission handlers.
func WithAdmitter(a Admitter) Option { return func(s *Server) { s.admitter = a } }

// WithVersion sets the broker build version reported by /healthz.
func WithVersion(v string) Option {
	return func(s *Server) {
		if v != "" {
			s.version = v
		}
	}
}
