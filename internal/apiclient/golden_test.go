package apiclient_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/antoniospapantoniou/bazel-broker/internal/api"
)

// fixtureDir is the committed cross-epic golden-fixture directory (repo-relative).
const fixtureDir = "../../testdata/api"

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fixtureDir, name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

// TestGoldenFixturesDecode proves the typed client decodes the FROZEN golden
// fixtures (testdata/api/*.json — the executable cross-epic contract) into the
// internal/api types verbatim. Drift in any json tag breaks this test.
func TestGoldenFixturesDecode(t *testing.T) {
	t.Run("healthz", func(t *testing.T) {
		var h api.HealthResponse
		if err := json.Unmarshal(readFixture(t, "healthz.json"), &h); err != nil {
			t.Fatal(err)
		}
		if h.Status != "ok" || h.Builds != 1 || h.Total != 2 || h.Version != "0.1.0" {
			t.Fatalf("healthz decoded wrong: %+v", h)
		}
	})

	t.Run("builds", func(t *testing.T) {
		var r api.BuildsResponse
		if err := json.Unmarshal(readFixture(t, "builds.json"), &r); err != nil {
			t.Fatal(err)
		}
		if len(r.Builds) != 2 {
			t.Fatalf("want 2 builds, got %d", len(r.Builds))
		}
		b0 := r.Builds[0]
		if b0.InvocationID != "a1b2" || b0.State != api.StateRunning || b0.ElapsedMS != 3120 {
			t.Fatalf("build[0] decoded wrong: %+v", b0)
		}
		// Enrichment fields (cache_hit_ratio, profile_url) on the terminal build.
		b1 := r.Builds[1]
		if b1.CacheHitRatio == nil || *b1.CacheHitRatio != 0.87 {
			t.Fatalf("build[1] cache_hit_ratio decoded wrong: %+v", b1.CacheHitRatio)
		}
		if b1.ProfileURL == "" {
			t.Fatalf("build[1] profile_url empty, want a URL")
		}
	})

	t.Run("build", func(t *testing.T) {
		var r api.BuildResponse
		if err := json.Unmarshal(readFixture(t, "build.json"), &r); err != nil {
			t.Fatal(err)
		}
		if r.Build.InvocationID != "a1b2" {
			t.Fatalf("build decoded wrong: %+v", r.Build)
		}
	})

	t.Run("event_snapshot", func(t *testing.T) {
		var ev api.Event
		if err := json.Unmarshal(readFixture(t, "event_snapshot.json"), &ev); err != nil {
			t.Fatal(err)
		}
		if ev.Type != api.EventSnapshot || len(ev.Builds) != 2 {
			t.Fatalf("snapshot decoded wrong: %+v", ev)
		}
	})

	t.Run("event_build", func(t *testing.T) {
		var ev api.Event
		if err := json.Unmarshal(readFixture(t, "event_build.json"), &ev); err != nil {
			t.Fatal(err)
		}
		if ev.Type != api.EventBuild || ev.Build == nil || ev.Build.InvocationID != "a1b2" {
			t.Fatalf("build event decoded wrong: %+v", ev)
		}
	})
}

// TestClientDecodesFixturesOverHTTP serves the golden builds.json from an
// httptest server and asserts the typed client round-trips it end to end — proving
// the transport + decode path agrees with the frozen contract, bearer header and all.
func TestClientDecodesFixturesOverHTTP(t *testing.T) {
	const token = "tok-abc"
	builds := readFixture(t, "builds.json")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/builds" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(builds)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	host, port := splitHostPort(t, srv.URL)
	c := newTestClient(host, port, token)
	resp, err := c.ListBuilds(context.Background())
	if err != nil {
		t.Fatalf("ListBuilds: %v", err)
	}
	if len(resp.Builds) != 2 || resp.Builds[0].InvocationID != "a1b2" {
		t.Fatalf("decoded wrong over HTTP: %+v", resp)
	}
}
