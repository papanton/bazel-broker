package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/antoniospapantoniou/bazel-broker/internal/api"
	buildpkg "github.com/antoniospapantoniou/bazel-broker/internal/build"
	"github.com/antoniospapantoniou/bazel-broker/internal/config"
	"github.com/antoniospapantoniou/bazel-broker/internal/registry"
	"github.com/antoniospapantoniou/bazel-broker/internal/store"
)

const testToken = "test-token-123"

func newTestServer(t *testing.T) (*Server, *registry.Registry) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "broker.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	hub := registry.NewHub()
	reg := registry.New(s, hub, nil)
	cfg := &config.Config{Host: "127.0.0.1", Port: 0, Token: testToken}
	return New(cfg, reg, hub, nil), reg
}

func bearer(req *http.Request) *http.Request {
	req.Header.Set("Authorization", "Bearer "+testToken)
	return req
}

func TestHealthzNoAuth(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 200 {
		t.Fatalf("healthz code = %d", rec.Code)
	}
	var h api.HealthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &h); err != nil {
		t.Fatal(err)
	}
	if h.Status != "ok" || h.Builds != 0 || h.Queued != 0 {
		t.Fatalf("healthz body = %+v", h)
	}
}

func TestBuildsRequiresAuth(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, httptest.NewRequest("GET", "/builds", nil))
	if rec.Code != 401 {
		t.Fatalf("unauthenticated /builds = %d, want 401", rec.Code)
	}

	rec = httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, bearer(httptest.NewRequest("GET", "/builds", nil)))
	if rec.Code != 200 {
		t.Fatalf("authenticated /builds = %d, want 200", rec.Code)
	}
}

func TestWrongTokenRejected(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/builds", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	srv.Routes().ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("wrong token = %d, want 401", rec.Code)
	}
}

func TestRegisterListDeregisterFlow(t *testing.T) {
	srv, _ := newTestServer(t)
	body := `{"invocation_id":"smoke1","worktree":"/wt/a","targets":["//app:App"],"pid":4242}`
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, bearer(httptest.NewRequest("POST", "/register", strings.NewReader(body))))
	if rec.Code != 200 {
		t.Fatalf("register = %d: %s", rec.Code, rec.Body)
	}
	var br api.BuildResponse
	json.Unmarshal(rec.Body.Bytes(), &br)
	if br.Build.State != api.StateRunning || br.Build.InvocationID != "smoke1" {
		t.Fatalf("register build = %+v", br.Build)
	}

	// list shows it running
	rec = httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, bearer(httptest.NewRequest("GET", "/builds", nil)))
	var bs api.BuildsResponse
	json.Unmarshal(rec.Body.Bytes(), &bs)
	if len(bs.Builds) != 1 || bs.Builds[0].State != api.StateRunning {
		t.Fatalf("builds = %+v", bs.Builds)
	}

	// deregister exit 0 -> finished
	rec = httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, bearer(httptest.NewRequest("POST", "/deregister", strings.NewReader(`{"invocation_id":"smoke1","exit_code":0}`))))
	json.Unmarshal(rec.Body.Bytes(), &br)
	if br.Build.State != api.StateFinished || br.Build.EndTime == "" {
		t.Fatalf("deregister build = %+v", br.Build)
	}
}

func TestRegisterValidation(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, bearer(httptest.NewRequest("POST", "/register", strings.NewReader(`{"worktree":"/wt"}`))))
	if rec.Code != 400 {
		t.Fatalf("missing invocation_id = %d, want 400", rec.Code)
	}
}

func TestReservedRoutesReturn501(t *testing.T) {
	srv, _ := newTestServer(t)
	cases := []struct{ method, path string }{
		{"POST", "/builds/abc/kill"},
		{"GET", "/builds/abc/metrics"},
		{"GET", "/builds/abc/profile"},
		{"GET", "/metrics"},
		// NOTE: GET /profile/{id}/{name} is intentionally OMITTED here — OD-C makes it
		// token-EXEMPT (Perfetto fetches it cross-origin without the bearer). Its
		// exemption is asserted separately in TestProfileRouteTokenExemptOD_C.
		{"GET", "/diskcache"},
		{"POST", "/admission"},
		{"POST", "/admission/release"},
		{"POST", "/admission/pause"},
		{"POST", "/admission/resume"},
		{"POST", "/admission/drain"},
		{"GET", "/admission/status"},
	}
	for _, tc := range cases {
		// without token -> 401
		rec := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != 401 {
			t.Errorf("%s %s no-token = %d, want 401", tc.method, tc.path, rec.Code)
		}
		// with token -> 501
		rec = httptest.NewRecorder()
		srv.Routes().ServeHTTP(rec, bearer(httptest.NewRequest(tc.method, tc.path, nil)))
		if rec.Code != 501 {
			t.Errorf("%s %s with-token = %d, want 501", tc.method, tc.path, rec.Code)
		}
		var e api.ErrorResponse
		json.Unmarshal(rec.Body.Bytes(), &e)
		if e.Error != "not_implemented" || e.Epic == "" {
			t.Errorf("%s %s 501 body = %+v", tc.method, tc.path, e)
		}
	}
}

// TestProfileRouteTokenExemptOD_C asserts the OD-C exemption: GET /profile/{id}/{name}
// is reachable WITHOUT a bearer token (Perfetto fetches it cross-origin and cannot
// present the token). With the default 501 stub it returns 501 — not 401 — proving
// auth did not block it. (E4's real handler serves the .gz / shim there.)
func TestProfileRouteTokenExemptOD_C(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, httptest.NewRequest("GET", "/profile/abc/foo.gz", nil))
	if rec.Code == 401 {
		t.Fatalf("/profile/{id}/{name} must be token-exempt (OD-C), got 401")
	}
	if rec.Code != 501 {
		t.Errorf("no-token /profile reached the stub = %d, want 501 (auth-exempt)", rec.Code)
	}
}

// TestSeamInjection proves a downstream epic can replace a 501 stub via a With*
// option without touching routing.
func TestSeamInjection(t *testing.T) {
	s, _ := store.Open(filepath.Join(t.TempDir(), "broker.db"), nil)
	defer s.Close()
	hub := registry.NewHub()
	reg := registry.New(s, hub, nil)
	cfg := &config.Config{Token: testToken, Port: 0}

	killer := killerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, api.KillResult{Killed: true, InvocationID: r.PathValue("invocation_id")})
	})
	srv := New(cfg, reg, hub, nil, WithKiller(killer))

	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, bearer(httptest.NewRequest("POST", "/builds/xyz/kill", nil)))
	if rec.Code != 200 {
		t.Fatalf("injected killer code = %d, want 200", rec.Code)
	}
	var kr api.KillResult
	json.Unmarshal(rec.Body.Bytes(), &kr)
	if !kr.Killed || kr.InvocationID != "xyz" {
		t.Fatalf("kill result = %+v", kr)
	}
}

type killerFunc func(http.ResponseWriter, *http.Request)

func (f killerFunc) Kill(w http.ResponseWriter, r *http.Request) { f(w, r) }

func TestWebSocketSnapshotThenBuild(t *testing.T) {
	srv, reg := newTestServer(t)
	hs := httptest.NewServer(srv.Routes())
	defer hs.Close()

	wsURL := "ws" + strings.TrimPrefix(hs.URL, "http") + "/events"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + testToken}},
	})
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer c.CloseNow()

	// 1. first frame is a snapshot
	snap := readEvent(t, ctx, c)
	if snap.Type != api.EventSnapshot {
		t.Fatalf("first frame type = %q, want snapshot", snap.Type)
	}

	// 2. a concurrent register produces a "build" event frame (upsert-by-id)
	if _, err := reg.Register(&buildpkg.Build{InvocationID: "ws1", Worktree: "/wt"}); err != nil {
		t.Fatal(err)
	}
	ev := readEvent(t, ctx, c)
	if ev.Type != api.EventBuild || ev.Build == nil || ev.Build.InvocationID != "ws1" {
		t.Fatalf("build event = %+v", ev)
	}
	if ev.Build.State != api.StateRunning {
		t.Fatalf("ws build state = %q", ev.Build.State)
	}
}

// readEvent reads and decodes one WS event frame.
func readEvent(t *testing.T, ctx context.Context, c *websocket.Conn) api.Event {
	t.Helper()
	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("ws read: %v", err)
	}
	var ev api.Event
	if err := json.Unmarshal(data, &ev); err != nil {
		t.Fatalf("decode event: %v\n%s", err, data)
	}
	return ev
}
