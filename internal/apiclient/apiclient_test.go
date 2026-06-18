package apiclient_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	"github.com/antoniospapantoniou/bazel-broker/internal/apiclient"
)

func splitHostPort(t *testing.T, raw string) (string, int) {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %s: %v", raw, err)
	}
	p, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("port from %s: %v", raw, err)
	}
	return u.Hostname(), p
}

func newTestClient(host string, port int, token string) *apiclient.Client {
	return apiclient.New(host, port, token, http.DefaultClient)
}

// TestStatusErrorMapping asserts each non-2xx status surfaces as a *StatusError
// carrying the code, and that the broker's {"error","message","epic"} body is
// decoded. The 501 case carries the owning epic so callers can degrade cleanly.
func TestStatusErrorMapping(t *testing.T) {
	cases := []struct {
		status   int
		body     string
		wantCode string
		wantEpic string
	}{
		{http.StatusUnauthorized, `{"error":"unauthorized"}`, "unauthorized", ""},
		{http.StatusNotImplemented, `{"error":"not_implemented","message":"owned by E3","epic":"E3"}`, "not_implemented", "E3"},
		{http.StatusNotFound, `{"error":"not_found"}`, "not_found", ""},
		{http.StatusInternalServerError, `{"error":"internal"}`, "internal", ""},
	}
	for _, tc := range cases {
		t.Run(strconv.Itoa(tc.status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			host, port := splitHostPort(t, srv.URL)
			c := newTestClient(host, port, "tok")

			_, err := c.ListBuilds(context.Background())
			var se *apiclient.StatusError
			if !errors.As(err, &se) {
				t.Fatalf("want *StatusError, got %T: %v", err, err)
			}
			if se.Status != tc.status {
				t.Errorf("status: got %d want %d", se.Status, tc.status)
			}
			if se.Code != tc.wantCode {
				t.Errorf("code: got %q want %q", se.Code, tc.wantCode)
			}
			if se.Epic != tc.wantEpic {
				t.Errorf("epic: got %q want %q", se.Epic, tc.wantEpic)
			}
		})
	}
}

// TestTransportError asserts a connection failure surfaces as *TransportError
// (not a StatusError), so callers map it to ExitUnavailable.
func TestTransportError(t *testing.T) {
	// Reserved TEST-NET / closed port: New on an unused loopback port.
	c := newTestClient("127.0.0.1", 1, "tok") // port 1: connection refused
	_, err := c.ListBuilds(context.Background())
	var te *apiclient.TransportError
	if !errors.As(err, &te) {
		t.Fatalf("want *TransportError, got %T: %v", err, err)
	}
}

// TestKillPath asserts Kill hits the nested per-build route (frozen contract:
// POST /builds/{invocation_id}/kill — NOT POST /kill).
func TestKillPath(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"killed":true,"invocation_id":"x","outcome":"sigint"}`))
	}))
	defer srv.Close()
	host, port := splitHostPort(t, srv.URL)
	c := newTestClient(host, port, "tok")

	res, err := c.Kill(context.Background(), "x")
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost || gotPath != "/builds/x/kill" {
		t.Fatalf("kill hit %s %s, want POST /builds/x/kill", gotMethod, gotPath)
	}
	if !res.Killed || res.Outcome != "sigint" {
		t.Fatalf("kill result decoded wrong: %+v", res)
	}
}

// TestAdmissionPaths asserts drain/pause/resume hit the /admission/* routes.
func TestAdmissionPaths(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	host, port := splitHostPort(t, srv.URL)
	c := newTestClient(host, port, "tok")

	for _, tc := range []struct {
		call func() error
		want string
	}{
		{func() error { return c.Drain(context.Background()) }, "/admission/drain"},
		{func() error { return c.Pause(context.Background()) }, "/admission/pause"},
		{func() error { return c.Resume(context.Background()) }, "/admission/resume"},
	} {
		if err := tc.call(); err != nil {
			t.Fatalf("%s: %v", tc.want, err)
		}
		if gotPath != tc.want {
			t.Errorf("hit %s, want %s", gotPath, tc.want)
		}
	}
}
