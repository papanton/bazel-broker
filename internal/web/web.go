// Package web serves the broker's glanceable local dashboard (E7): a self-
// contained static page (HTML + CSS + vanilla ES2020, NO build step) bundled via
// embed.FS, plus a same-origin cookie session (OD-B, Option A) so the browser can
// reach the otherwise bearer-only data endpoints without ever holding the token.
//
// The page is a pure consumer of E2's FROZEN contract:
//   - GET /builds            seed the table once on load
//   - WS  /events            live updates: exactly two frame types, snapshot+build,
//     upsert-by-invocation_id
//   - POST /builds/{id}/kill kill button (nested path, C2)
//   - GET  /builds/{id}/metrics or /metrics (E4) — cache% / profile enrichment is
//     carried on api.Build itself (cache_hit_ratio, profile_url)
//     and arrives via the same snapshot/build frames once E4 fills it.
//
// AUTH (OD-B, Option A — same-origin session cookie):
//   - POST /login  body {"token":"<bearer>"} → mints an opaque session, sets
//     Set-Cookie: broker_session=…; HttpOnly; SameSite=Strict; Path=/, and returns
//     the CSRF token the page echoes on the kill POST.
//   - The session cookie (not the token) then authenticates /builds, /events, kill.
//     This requires ONE documented hook in httpapi's auth middleware — see
//     RegisterRoutes' doc comment. The token-in-0600-config remains the single
//     security boundary at single-user-Mac scope (D-stack-2).
package web

import (
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"
	"strings"

	"github.com/antoniospapantoniou/bazel-broker/internal/api"
)

// staticFS holds the dashboard assets. URLs map as: "/" → index.html,
// "/static/app.js", "/static/app.css".
//
//go:embed static/index.html static/app.js static/app.css
var staticFS embed.FS

// Deps are the dependencies RegisterRoutes needs. Sessions is the same store
// httpapi's middleware must also consult (the Authenticator hook).
type Deps struct {
	Sessions *SessionStore
}

// RegisterRoutes mounts the dashboard + auth endpoints on the broker's existing
// loopback mux. It registers ONLY paths E2 does not own, so http.ServeMux's
// longest-prefix matching keeps the API routes intact:
//
//	GET  /            → index.html (auth-free; the page itself carries no secrets)
//	GET  /static/...  → app.js / app.css (auth-free)
//	POST /login       → mint session from the bearer token, Set-Cookie + CSRF
//	POST /logout      → revoke session
//	GET  /api/csrf    → CSRF token for the current session (kept out of the DOM source)
//
// ORCHESTRATOR WIRING — apply BOTH:
//
//  1. In cmd/broker/main.go, after `srv := httpapi.New(...)` and before Serve,
//     build the session store from the token, pass it to httpapi so its auth
//     middleware accepts the cookie, and mount this package on the SAME mux.
//     Because httpapi owns the mux internally (Server.Routes), expose the mux or
//     a registration hook. Concretely:
//
//     sessions := web.NewSessionStore(cfg.Token)
//     srv := httpapi.New(cfg, reg, hub, log,
//     httpapi.WithVersion(version.Version),
//     httpapi.WithKiller(killer),
//     httpapi.WithBrowserAuth(sessions),            // (2) below
//     httpapi.WithMux(func(mux *http.ServeMux) {    // mux-registration hook
//     _ = web.RegisterRoutes(mux, web.Deps{Sessions: sessions})
//     }),
//     )
//
//  2. THE ONE httpapi MIDDLEWARE CHANGE (OD-B). In internal/httpapi:
//
//     // seams.go — add a field + option:
//     //   browserAuth web.Authenticator   // nil unless E7 mounts the dashboard
//     //   func WithBrowserAuth(a web.Authenticator) Option { return func(s *Server){ s.browserAuth = a } }
//     // (declare the interface locally to avoid an import cycle, or import internal/web —
//     //  internal/web does NOT import internal/httpapi, so importing it here is cycle-free.)
//     //
//     // middleware.go — in authorize(r), ADD as the FIRST check:
//     //   if s.browserAuth != nil && s.browserAuth.Authenticate(r) { return true }
//     //   ...existing Bearer check unchanged...
//     //
//     // server.go Routes() — add a mux-registration hook so E7 can mount on the
//     // same mux the middleware wraps:
//     //   func WithMux(f func(*http.ServeMux)) Option { return func(s *Server){ s.muxHook = f } }
//     //   ...in Routes(), after the API routes:  if s.muxHook != nil { s.muxHook(mux) }
//
// That is the entire E2-side amendment: one extra branch in authorize() (cookie
// acceptance) plus a mux hook to mount the static handler. The CSRF check is
// enforced inside SessionStore.Authenticate for mutating methods, so no kill-route
// change is needed.
func RegisterRoutes(mux *http.ServeMux, deps Deps) error {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		return err
	}
	index, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		return err
	}
	files := http.StripPrefix("/static/", http.FileServer(http.FS(sub)))

	// withHeaders wraps every handler this package owns so the CSP / nosniff /
	// referrer set is applied uniformly — not re-invoked (and easily forgotten)
	// per handler.
	h := func(fn http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			setSecurityHeaders(w)
			fn(w, r)
		}
	}

	// GET / → index.html served directly (FileServer would 301 index.html→/ and loop).
	mux.HandleFunc("GET /{$}", h(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(index)
	}))
	mux.HandleFunc("GET /static/", h(files.ServeHTTP))
	mux.HandleFunc("POST /login", h(deps.handleLogin))
	mux.HandleFunc("POST /logout", h(deps.handleLogout))
	mux.HandleFunc("GET /api/csrf", h(deps.handleCSRF))
	return nil
}

func (d Deps) handleLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, api.ErrorResponse{Error: "bad_request"})
		return
	}
	if !d.Sessions.checkToken(strings.TrimSpace(body.Token)) {
		writeJSON(w, http.StatusUnauthorized, api.ErrorResponse{Error: "unauthorized"})
		return
	}
	id, csrf := d.Sessions.mint()
	// Set the session cookie. Secure is intentionally omitted: loopback HTTP only.
	http.SetCookie(w, sessionCookie(id, 0))
	writeJSON(w, http.StatusOK, map[string]string{"csrf": csrf})
}

func (d Deps) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(SessionCookie); err == nil {
		d.Sessions.revoke(c.Value)
	}
	http.SetCookie(w, sessionCookie("", -1)) // expire immediately
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleCSRF returns the CSRF token bound to the current session so the page can
// read it after a reload without re-logging-in (the cookie is HttpOnly, so JS
// cannot read it directly).
func (d Deps) handleCSRF(w http.ResponseWriter, r *http.Request) {
	var csrf string
	if c, err := r.Cookie(SessionCookie); err == nil {
		csrf = d.Sessions.csrfFor(c.Value)
	}
	if csrf == "" {
		writeJSON(w, http.StatusUnauthorized, api.ErrorResponse{Error: "no_session"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"csrf": csrf})
}

// sessionCookie builds the broker_session cookie. maxAge==0 → session cookie;
// maxAge<0 → expire now. The HttpOnly; SameSite=Strict; Path=/ invariant lives
// here so login and logout cannot drift.
func sessionCookie(value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     SessionCookie,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	}
}

// setSecurityHeaders applies a tight CSP. No inline scripts/handlers (CSP-clean):
// app.js is a separate self-hosted file; connect-src 'self' allows the same-origin
// fetch + ws://. frame-ancestors 'none' blocks embedding (clickjacking).
func setSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy", strings.Join([]string{
		"default-src 'none'",
		"script-src 'self'",
		"style-src 'self'",
		"img-src 'self' data:",
		"connect-src 'self'",
		"form-action 'self'",
		"frame-ancestors 'none'",
		"base-uri 'none'",
	}, "; "))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
