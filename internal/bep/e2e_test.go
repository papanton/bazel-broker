package bep

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/antoniospapantoniou/bazel-broker/internal/build"
	"github.com/antoniospapantoniou/bazel-broker/internal/store"
)

// TestRealFixtureEndToEnd drives the REAL warm-iOS BEP through the full ingest
// stack (tailer → dispatch → store) and asserts GET /metrics returns the ratio
// that matches Bazel's own summary (35 disk cache hit / (35+4) = 0.8974…), then
// serves the REAL profile.gz and the Perfetto shim.
func TestRealFixtureEndToEnd(t *testing.T) {
	warm, err := os.ReadFile("testdata/bep/warm-ios.ndjson")
	if err != nil {
		t.Fatal(err)
	}
	profileSrc, err := os.ReadFile("testdata/bep/warm-ios.profile.gz")
	if err != nil {
		t.Fatal(err)
	}

	wt := t.TempDir()
	bbDir := filepath.Join(wt, ".bazel-broker")
	if err := os.MkdirAll(bbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Place the real profile where E1's convention expects it.
	if err := os.WriteFile(filepath.Join(bbDir, "command.profile.gz"), profileSrc, 0o644); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(filepath.Join(t.TempDir(), "e2e.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	warmID := lastUUID(t, warm)
	// Pre-register the build so the metrics FK resolves (registry lifecycle).
	if err := st.UpsertBuild(&build.Build{InvocationID: warmID, Worktree: wt, State: build.StateRunning, Source: build.SourceRegistered}); err != nil {
		t.Fatal(err)
	}

	reg := newFakeReg()
	prov := NewProvider(storeAdapter{st}, "http://127.0.0.1:8765", testLogger())
	mgr := NewManager(reg, st, testLogger(), prov.ProfileURLFor, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr.Watch(ctx, wt)

	// Drop the real BEP in place; the tailer reads it from offset 0.
	if err := os.WriteFile(BEPPathFor(wt), warm, 0o644); err != nil {
		t.Fatal(err)
	}

	// Wait for the metrics row to land.
	waitFor(t, 6*time.Second, func() bool {
		_, _, ok, _ := st.GetMetrics(warmID)
		return ok
	})

	mux := http.NewServeMux()
	mux.HandleFunc("GET /builds/{invocation_id}/metrics", prov.BuildMetrics)
	mux.HandleFunc("GET /profile/{invocation_id}/{name}", prov.ProfileFile)

	// /metrics ratio matches Bazel's summary.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/builds/"+warmID+"/metrics", nil))
	if rec.Code != 200 {
		t.Fatalf("metrics status = %d body=%s", rec.Code, rec.Body.String())
	}
	var got metricsJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Cache.DiskCacheHits != 35 || got.Cache.ExecutedRunners != 4 {
		t.Errorf("real-fixture cache wrong: disk=%d exec=%d (want 35/4)", got.Cache.DiskCacheHits, got.Cache.ExecutedRunners)
	}
	if got.Cache.HitRatio == nil || *got.Cache.HitRatio < 0.897 || *got.Cache.HitRatio > 0.898 {
		t.Errorf("real-fixture hit_ratio = %v, want 0.8974 (matches Bazel summary 35/(35+4))", got.Cache.HitRatio)
	}

	// Serve the REAL profile.gz — must be valid gzip and non-empty.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/profile/"+warmID+"/command.profile.gz", nil))
	if rec.Code != 200 {
		t.Fatalf("profile status = %d", rec.Code)
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("real profile not valid gzip: %v", err)
	}
	_ = zr.Close()

	cancel()
	time.Sleep(50 * time.Millisecond)
}
