package httpapi

import (
	"bufio"
	"crypto/subtle"
	"fmt"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/antoniospapantoniou/bazel-broker/internal/api"
	"github.com/antoniospapantoniou/bazel-broker/internal/build"
)

// defaultVersion is reported by /healthz when no WithVersion option is supplied
// (keeps tests deterministic without global state).
const defaultVersion = "0.1.0"

// authExempt reports whether a path bypasses bearer auth. Today only /healthz is
// exempt. SEAM (OD-C): when E4 lands the Perfetto profile routes
// (GET /builds/{id}/profile, GET /profile/{id}/{name}) become token-exempt +
// Origin-restricted CORS because ui.perfetto.dev cannot present the bearer token.
// Add those prefixes here (and Origin-restrict in a CORS middleware) at that time.
//
// SEAM (browser auth, E7 OD-1): a same-origin cookie acceptance path would be
// added to authorize(...) below behind an authMode switch — E2 ships bearer-only.
func authExempt(path string) bool {
	if path == "/healthz" {
		return true
	}
	// OD-C (E4): the read-only, id-addressed Perfetto profile routes are token-exempt
	// because ui.perfetto.dev fetches them cross-origin and cannot present the bearer
	// token. CORS is Origin-restricted to https://ui.perfetto.dev inside the E4
	// ProfileFile handler. This covers GET /profile/{id}/{name} (the .gz + the
	// postMessage shim page). GET /builds/{id}/profile (the JSON ref) stays guarded.
	return strings.HasPrefix(path, "/profile/")
}

// webPublicPath reports whether a path is a dashboard entry point that must load
// before the browser holds a session: the page itself, its static assets, and the
// login endpoint that mints the session from the bearer token. These carry no
// secrets; the data/kill routes still require the session cookie via authorize().
// Only exempted when the dashboard (browserAuth) is actually mounted.
func webPublicPath(path string) bool {
	return path == "/" || path == "/login" || strings.HasPrefix(path, "/static/")
}

// auth is the bearer-token middleware. Missing/blank/wrong token => 401. The
// comparison is constant-time.
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if authExempt(r.URL.Path) || (s.browserAuth != nil && webPublicPath(r.URL.Path)) {
			next.ServeHTTP(w, r)
			return
		}
		if !s.authorize(r) {
			writeError(w, http.StatusUnauthorized, api.ErrorResponse{Error: "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// authorize returns true if r carries a valid same-origin session cookie (E7
// OD-B; CSRF-enforced for mutating methods inside the authenticator) or the
// configured bearer token.
func (s *Server) authorize(r *http.Request) bool {
	if s.browserAuth != nil && s.browserAuth.Authenticate(r) {
		return true
	}
	return s.bearerAuth(r)
}

// bearerAuth returns true if r carries the configured bearer token.
func (s *Server) bearerAuth(r *http.Request) bool {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return false
	}
	got := strings.TrimSpace(h[len(prefix):])
	want := s.cfg.Token
	if got == "" || want == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// requestLog logs method, path, status and duration for every request.
func (s *Server) requestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		s.log.Info("http",
			"method", r.Method, "path", r.URL.Path,
			"status", sw.status, "dur_ms", time.Since(start).Milliseconds())
	})
}

// recoverer turns a panic into a 500 and logs the stack.
func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic", "err", rec, "stack", string(debug.Stack()))
				writeError(w, http.StatusInternalServerError, api.ErrorResponse{Error: "internal"})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// statusWriter captures the response status for logging. It also implements
// http.Hijacker so the WebSocket upgrade works through the middleware chain.
type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (s *statusWriter) WriteHeader(code int) {
	if !s.wroteHeader {
		s.status = code
		s.wroteHeader = true
	}
	s.ResponseWriter.WriteHeader(code)
}

// Hijack forwards to the underlying ResponseWriter so the WebSocket upgrade
// (coder/websocket) works through the middleware chain.
func (s *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := s.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("underlying ResponseWriter is not a http.Hijacker")
	}
	return hj.Hijack()
}

// Flush forwards to the underlying ResponseWriter when supported.
func (s *statusWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// toDomain maps a RegisterRequest into a domain Build.
func toDomain(req api.RegisterRequest) *build.Build {
	src := build.Source(req.Source)
	if src == "" {
		src = build.SourceRegistered
	}
	return &build.Build{
		InvocationID: req.InvocationID,
		Worktree:     req.Worktree,
		Targets:      req.Targets,
		PID:          req.PID,
		Source:       src,
	}
}
