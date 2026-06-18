package cli

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// profileServer serves GET /builds/{id} and GET /builds/{id}/profile with the
// given handlers.
func profileServer(t *testing.T, build, profile http.HandlerFunc) *Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /builds/{invocation_id}/profile", profile)
	mux.HandleFunc("GET /builds/{invocation_id}", build)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return clientFor(t, srv, "tok")
}

// TestProfile_PrefersProfileURL asserts profile_url on the build is used directly
// (no /profile call needed) and --print emits it without opening.
func TestProfile_PrefersProfileURL(t *testing.T) {
	c := profileServer(t,
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"build":{"invocation_id":"x","profile_url":"http://perfetto/x"}}`))
		},
		func(w http.ResponseWriter, _ *http.Request) {
			t.Error("should not call /profile when profile_url is present")
			w.WriteHeader(http.StatusInternalServerError)
		},
	)
	var buf bytes.Buffer
	opened := false
	err := RunProfile(context.Background(), c, "x", ProfileOpts{
		Print:  true,
		Opener: func(context.Context, string) error { opened = true; return nil },
	}, &buf)
	if err != nil {
		t.Fatal(err)
	}
	if opened {
		t.Error("--print must not open")
	}
	if strings.TrimSpace(buf.String()) != "http://perfetto/x" {
		t.Fatalf("--print should emit the target, got %q", buf.String())
	}
}

// TestProfile_FallsBackToProfileRef asserts that when profile_url is empty, the
// command falls back to GET /builds/{id}/profile and uses perfetto_url.
func TestProfile_FallsBackToProfileRef(t *testing.T) {
	c := profileServer(t,
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"build":{"invocation_id":"x"}}`)) // no profile_url
		},
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"perfetto_url":"http://perfetto/ref","local_path":"/p.gz"}`))
		},
	)
	var buf bytes.Buffer
	var got string
	err := RunProfile(context.Background(), c, "x", ProfileOpts{
		Opener: func(_ context.Context, target string) error { got = target; return nil },
	}, &buf)
	if err != nil {
		t.Fatal(err)
	}
	if got != "http://perfetto/ref" {
		t.Fatalf("opener got %q, want the perfetto_url", got)
	}
}

// TestProfile_501Degrades asserts that when the build has no profile_url and
// /profile is reserved (501), the command degrades to ExitNotImplemented.
func TestProfile_501Degrades(t *testing.T) {
	c := profileServer(t,
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"build":{"invocation_id":"x"}}`))
		},
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotImplemented)
			_, _ = w.Write([]byte(`{"error":"not_implemented","epic":"E4"}`))
		},
	)
	err := RunProfile(context.Background(), c, "x", ProfileOpts{
		Opener: func(context.Context, string) error { t.Error("must not open on 501"); return nil },
	}, &bytes.Buffer{})
	if codeOf(err) != ExitNotImplemented {
		t.Fatalf("want ExitNotImplemented, got %v", err)
	}
}
