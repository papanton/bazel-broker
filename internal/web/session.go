package web

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"sync"
	"time"
)

// Cookie / header names for the same-origin session (OD-B, Option A).
const (
	// SessionCookie is the HttpOnly; SameSite=Strict cookie that authenticates the
	// browser to the broker's data endpoints (/builds, /events, kill). Its value is
	// an opaque in-memory session id minted from the real bearer token at /login —
	// the token itself never enters the DOM or the cookie.
	SessionCookie = "broker_session"
	// CSRFHeader carries the double-submit CSRF token on mutating requests (kill).
	// The value must equal the CSRF token bound to the session.
	CSRFHeader = "X-Broker-CSRF"

	sessionTTL = 12 * time.Hour
)

// session is one authenticated browser session.
type session struct {
	csrf    string
	expires time.Time
}

// SessionStore mints and validates same-origin sessions. It is the single
// security primitive E7 adds: a browser that proves it knows the broker's 0600
// config token (via POST /login) receives an opaque session cookie; that cookie
// then authenticates the otherwise bearer-only data endpoints.
//
// httpapi's auth middleware consults this store through the Authenticator
// interface (see Authenticator) — the documented one-line hook the orchestrator
// wires in. Everything else lives here, in internal/web.
type SessionStore struct {
	mu       sync.Mutex
	sessions map[string]session
	token    string // the broker's bearer token (the login secret)
}

// NewSessionStore returns a store whose login secret is the broker's bearer
// token. It starts a background sweeper that prunes expired sessions so the map
// cannot grow unbounded across the broker's uptime (cookies expire after
// sessionTTL and a browser may never re-present an expired id).
func NewSessionStore(token string) *SessionStore {
	st := &SessionStore{sessions: make(map[string]session), token: token}
	go st.sweep()
	return st
}

// sweep periodically deletes expired sessions. Runs for the process lifetime.
func (st *SessionStore) sweep() {
	t := time.NewTicker(sessionTTL / 12) // hourly at the default 12h TTL
	defer t.Stop()
	for range t.C {
		now := time.Now()
		st.mu.Lock()
		for id, s := range st.sessions {
			if now.After(s.expires) {
				delete(st.sessions, id)
			}
		}
		st.mu.Unlock()
	}
}

// lookup returns the live (unexpired) session for id under the lock. Shared by
// Authenticate and csrfFor so the expiry rule lives in exactly one place.
func (st *SessionStore) lookup(id string) (session, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	s, ok := st.sessions[id]
	if !ok {
		return session{}, false
	}
	if time.Now().After(s.expires) {
		delete(st.sessions, id)
		return session{}, false
	}
	return s, true
}

// Authenticate reports whether r carries a valid session cookie. For mutating
// methods (anything other than GET/HEAD) it additionally requires a matching
// CSRF token in CSRFHeader (double-submit). This is the method httpapi's
// middleware calls.
func (st *SessionStore) Authenticate(r *http.Request) bool {
	c, err := r.Cookie(SessionCookie)
	if err != nil || c.Value == "" {
		return false
	}
	sess, ok := st.lookup(c.Value)
	if !ok {
		return false
	}
	if !isSafeMethod(r.Method) {
		got := r.Header.Get(CSRFHeader)
		if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(sess.csrf)) != 1 {
			return false
		}
	}
	return true
}

// mint creates a new session for a caller that proved knowledge of the token.
// Returns the session id (cookie value) and the CSRF token.
func (st *SessionStore) mint() (id, csrf string) {
	id = randHex(32)
	csrf = randHex(32)
	st.mu.Lock()
	st.sessions[id] = session{csrf: csrf, expires: time.Now().Add(sessionTTL)}
	st.mu.Unlock()
	return id, csrf
}

// revoke drops a session (logout).
func (st *SessionStore) revoke(id string) {
	st.mu.Lock()
	delete(st.sessions, id)
	st.mu.Unlock()
}

// csrfFor returns the CSRF token bound to a valid, unexpired session id, or "".
func (st *SessionStore) csrfFor(id string) string {
	if s, ok := st.lookup(id); ok {
		return s.csrf
	}
	return ""
}

// checkToken constant-time compares a presented token against the broker's.
func (st *SessionStore) checkToken(got string) bool {
	if got == "" || st.token == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(st.token)) == 1
}

// Authenticator is the seam httpapi's auth middleware consults to accept the
// same-origin session cookie in addition to the bearer token. *SessionStore
// satisfies it. See RegisterRoutes' doc comment for the exact wiring.
type Authenticator interface {
	Authenticate(r *http.Request) bool
}

func isSafeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
