package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"
)

// clientFor builds a cli.Client pointed at srv via a temp config file, so the full
// config→client→exit-code path is exercised.
func clientFor(t *testing.T, srv *httptest.Server, token string) *Client {
	t.Helper()
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())
	p := writeCfg(t, t.TempDir(), `{"host":"`+u.Hostname()+`","port":`+strconv.Itoa(port)+`,"token":"`+token+`"}`)
	c, err := NewClient(GlobalOpts{ConfigPath: p, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestStatusToExitCode asserts the degradation contract: 401→ExitAuth,
// 501→ExitNotImplemented (NOT a crash), 404→ExitBroker, 500→ExitBroker.
func TestStatusToExitCode(t *testing.T) {
	cases := []struct {
		status int
		want   int
	}{
		{http.StatusUnauthorized, ExitAuth},
		{http.StatusNotImplemented, ExitNotImplemented},
		{http.StatusNotFound, ExitBroker},
		{http.StatusInternalServerError, ExitBroker},
	}
	for _, tc := range cases {
		t.Run(strconv.Itoa(tc.status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"error":"x","message":"m","epic":"E3"}`))
			}))
			defer srv.Close()
			c := clientFor(t, srv, "tok")
			_, err := c.ListBuildsResponse(context.Background())
			if codeOf(err) != tc.want {
				t.Fatalf("status %d: got code %d want %d (err=%v)", tc.status, codeOf(err), tc.want, err)
			}
		})
	}
}

// TestBuildsUnwrap asserts /builds is decoded from the {"builds":[…]} envelope.
func TestBuildsUnwrap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"builds":[{"invocation_id":"a"},{"invocation_id":"b"}]}`))
	}))
	defer srv.Close()
	c := clientFor(t, srv, "tok")
	resp, err := c.ListBuildsResponse(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Builds) != 2 || resp.Builds[1].InvocationID != "b" {
		t.Fatalf("unwrap failed: %+v", resp)
	}
}

// TestUnavailable asserts a refused connection maps to ExitUnavailable.
func TestUnavailable(t *testing.T) {
	p := writeCfg(t, t.TempDir(), `{"host":"127.0.0.1","port":1,"token":"tok"}`)
	c, err := NewClient(GlobalOpts{ConfigPath: p, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListBuildsResponse(context.Background()); codeOf(err) != ExitUnavailable {
		t.Fatalf("want ExitUnavailable, got %v", err)
	}
}

// TestPortOverride asserts --port overrides the config port.
func TestPortOverride(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"builds":[]}`))
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())
	// config points at a dead port; --port override redirects to the live server.
	p := writeCfg(t, t.TempDir(), `{"host":"`+u.Hostname()+`","port":1,"token":"tok"}`)
	c, err := NewClient(GlobalOpts{ConfigPath: p, Port: port, Token: "tok", Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListBuildsResponse(context.Background()); err != nil {
		t.Fatalf("--port override should reach the live server: %v", err)
	}
}
