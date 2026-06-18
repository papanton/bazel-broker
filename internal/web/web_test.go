package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testToken = "test-token-abc123"

func newServer(t *testing.T) (*httptest.Server, *SessionStore) {
	t.Helper()
	sessions := NewSessionStore(testToken)
	mux := http.NewServeMux()
	if err := RegisterRoutes(mux, Deps{Sessions: sessions}); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}
	return httptest.NewServer(mux), sessions
}

func TestServesIndexAndAssets(t *testing.T) {
	srv, _ := newServer(t)
	defer srv.Close()

	// GET / → the page, with CSP.
	res, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET / status = %d", res.StatusCode)
	}
	if !strings.Contains(string(body), "<title>Bazel Broker</title>") {
		t.Fatalf("GET / did not return the dashboard page")
	}
	if res.Header.Get("Content-Security-Policy") == "" {
		t.Fatalf("GET / missing Content-Security-Policy header")
	}

	// Assets resolve with sensible content types.
	for _, tc := range []struct{ path, wantType string }{
		{"/static/app.js", "javascript"},
		{"/static/app.css", "css"},
	} {
		r, err := http.Get(srv.URL + tc.path)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != http.StatusOK {
			t.Fatalf("GET %s status = %d", tc.path, r.StatusCode)
		}
		if ct := r.Header.Get("Content-Type"); !strings.Contains(ct, tc.wantType) {
			t.Fatalf("GET %s content-type = %q, want substring %q", tc.path, ct, tc.wantType)
		}
	}
}

func TestLoginMintsSessionAndCSRF(t *testing.T) {
	srv, store := newServer(t)
	defer srv.Close()

	// Wrong token → 401, no cookie.
	bad, _ := http.Post(srv.URL+"/login", "application/json", strings.NewReader(`{"token":"nope"}`))
	if bad.StatusCode != http.StatusUnauthorized {
		t.Fatalf("login with bad token status = %d, want 401", bad.StatusCode)
	}
	bad.Body.Close()

	// Correct token → 200, Set-Cookie, CSRF in body.
	ok, _ := http.Post(srv.URL+"/login", "application/json", strings.NewReader(`{"token":"`+testToken+`"}`))
	if ok.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d, want 200", ok.StatusCode)
	}
	var loginResp struct {
		CSRF string `json:"csrf"`
	}
	json.NewDecoder(ok.Body).Decode(&loginResp)
	ok.Body.Close()
	if loginResp.CSRF == "" {
		t.Fatalf("login returned no CSRF token")
	}

	var cookie *http.Cookie
	for _, c := range ok.Cookies() {
		if c.Name == SessionCookie {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatalf("login did not set %s cookie", SessionCookie)
	}
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("session cookie not HttpOnly/SameSite=Strict: %+v", cookie)
	}

	// The Authenticator hook accepts a GET carrying the cookie.
	getReq, _ := http.NewRequest(http.MethodGet, "/builds", nil)
	getReq.AddCookie(cookie)
	if !store.Authenticate(getReq) {
		t.Fatalf("Authenticate rejected a valid session cookie on GET")
	}

	// A mutating request without the CSRF header is rejected (CSRF guard)…
	postReq, _ := http.NewRequest(http.MethodPost, "/builds/x/kill", nil)
	postReq.AddCookie(cookie)
	if store.Authenticate(postReq) {
		t.Fatalf("Authenticate accepted a POST with no CSRF header")
	}
	// …and accepted with the matching CSRF token.
	postReq.Header.Set(CSRFHeader, loginResp.CSRF)
	if !store.Authenticate(postReq) {
		t.Fatalf("Authenticate rejected a POST with the correct CSRF token")
	}
}

func TestAuthenticateRejectsUnknownCookie(t *testing.T) {
	store := NewSessionStore(testToken)
	req, _ := http.NewRequest(http.MethodGet, "/builds", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookie, Value: "forged"})
	if store.Authenticate(req) {
		t.Fatalf("Authenticate accepted a forged session cookie")
	}
	// No cookie at all.
	bare, _ := http.NewRequest(http.MethodGet, "/builds", nil)
	if store.Authenticate(bare) {
		t.Fatalf("Authenticate accepted a request with no session cookie")
	}
}

func TestCSRFEndpointRequiresSession(t *testing.T) {
	srv, _ := newServer(t)
	defer srv.Close()

	// No session → 401.
	r, _ := http.Get(srv.URL + "/api/csrf")
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("/api/csrf without session = %d, want 401", r.StatusCode)
	}
	r.Body.Close()

	// With session → returns the same CSRF token.
	client := &http.Client{}
	login, _ := http.Post(srv.URL+"/login", "application/json", strings.NewReader(`{"token":"`+testToken+`"}`))
	var lr struct {
		CSRF string `json:"csrf"`
	}
	json.NewDecoder(login.Body).Decode(&lr)
	login.Body.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/csrf", nil)
	for _, c := range login.Cookies() {
		req.AddCookie(c)
	}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var cr struct {
		CSRF string `json:"csrf"`
	}
	json.NewDecoder(res.Body).Decode(&cr)
	res.Body.Close()
	if cr.CSRF != lr.CSRF {
		t.Fatalf("/api/csrf returned %q, want %q", cr.CSRF, lr.CSRF)
	}
}
