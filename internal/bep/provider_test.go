package bep

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/papanton/bazel-broker/internal/build"
	"github.com/papanton/bazel-broker/internal/metrics"
	"github.com/papanton/bazel-broker/internal/store"
)

// providerHarness wires a real store + provider behind a bare mux mirroring the
// E2 routes the MetricsProvider owns.
func providerHarness(t *testing.T) (*Provider, *store.Store, *http.ServeMux) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "p.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	prov := NewProvider(storeAdapter{st}, "http://127.0.0.1:8765", testLogger())

	mux := http.NewServeMux()
	mux.HandleFunc("GET /builds/{invocation_id}/metrics", prov.BuildMetrics)
	mux.HandleFunc("GET /builds/{invocation_id}/profile", prov.BuildProfile)
	mux.HandleFunc("GET /metrics", prov.MetricsList)
	mux.HandleFunc("GET /profile/{invocation_id}/{name}", prov.ProfileFile)
	mux.HandleFunc("GET /diskcache", prov.DiskCache)
	return prov, st, mux
}

func seedWarm(t *testing.T, st *store.Store, profilePath string) string {
	t.Helper()
	const id = "warm-1"
	if err := st.UpsertBuild(&build.Build{InvocationID: id, Worktree: "/wt", State: build.StateFinished, Source: build.SourceRegistered}); err != nil {
		t.Fatal(err)
	}
	row := &metrics.Row{
		InvocationID: id, Worktree: "/wt", ProfilePath: profilePath,
		ActionsCreated: 319, ActionsExecuted: 39, DiskCacheHits: 35, ExecutedRunners: 4,
		ProcessesTotal: 108, WallMS: 222, HasMetrics: true, HasFinish: true, Success: true,
		SummaryLine: "108 processes: 35 disk cache hit, 69 internal, 4 worker.",
		RunnerCounts: []metrics.RunnerCount{
			{Name: "total", Count: 108}, {Name: "disk cache hit", Count: 35},
			{Name: "internal", Count: 69}, {Name: "worker", Count: 4},
		},
	}
	if err := st.UpsertMetrics(row, "{}"); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestMetricsRouteReturnsRatio(t *testing.T) {
	_, st, mux := providerHarness(t)
	id := seedWarm(t, st, "")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/builds/"+id+"/metrics", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var got metricsJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Cache.DiskCacheHits != 35 || got.Cache.ExecutedRunners != 4 {
		t.Errorf("cache counts wrong: %+v", got.Cache)
	}
	if got.Cache.HitRatio == nil || *got.Cache.HitRatio < 0.89 || *got.Cache.HitRatio > 0.90 {
		t.Errorf("hit_ratio = %v, want ~0.897", got.Cache.HitRatio)
	}
	if got.Profile.PerfettoURL == "" || got.Profile.ServedURL == "" {
		t.Errorf("profile urls missing: %+v", got.Profile)
	}

	// Unknown id → 404.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/builds/nope/metrics", nil))
	if rec.Code != 404 {
		t.Errorf("unknown id status = %d, want 404", rec.Code)
	}

	// List form via ?invocation_id alias.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics?invocation_id="+id, nil))
	if rec.Code != 200 {
		t.Errorf("?invocation_id status = %d", rec.Code)
	}
}

func TestProfileFileServesGzipAndShim(t *testing.T) {
	_, st, mux := providerHarness(t)

	// Real gzip profile on disk.
	dir := t.TempDir()
	gz := filepath.Join(dir, "command.profile.gz")
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write([]byte(`{"traceEvents":[]}`))
	_ = zw.Close()
	if err := os.WriteFile(gz, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	id := seedWarm(t, st, gz)

	// Serve the .gz — must be valid gzip.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/profile/"+id+"/command.profile.gz", nil))
	if rec.Code != 200 {
		t.Fatalf("profile status = %d", rec.Code)
	}
	zr, err := gzip.NewReader(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("served bytes not valid gzip: %v", err)
	}
	if _, err := io.ReadAll(zr); err != nil {
		t.Fatalf("gzip read: %v", err)
	}

	// OD-C: a request carrying the Perfetto Origin gets the Origin-restricted CORS header.
	req := httptest.NewRequest("GET", "/profile/"+id+"/command.profile.gz", nil)
	req.Header.Set("Origin", perfettoOrigin)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != perfettoOrigin {
		t.Errorf("CORS allow-origin = %q, want %q", got, perfettoOrigin)
	}

	// The shim page references ui.perfetto.dev + postMessage.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/profile/"+id+"/perfetto", nil))
	if rec.Code != 200 {
		t.Fatalf("shim status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !bytes.Contains([]byte(body), []byte("ui.perfetto.dev")) || !bytes.Contains([]byte(body), []byte("postMessage")) {
		t.Errorf("shim HTML missing perfetto/postMessage references")
	}
	// Traversal-safety: the {name} component never selects the file (path comes from DB).
	if !bytes.Contains([]byte(body), []byte("/profile/"+id+"/command.profile.gz")) {
		t.Errorf("shim should fetch the canonical same-origin profile URL")
	}
}

func TestDiskCacheRoute(t *testing.T) {
	prov, st, mux := providerHarness(t)
	_ = st
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "a"), []byte("hello"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "b"), []byte("world!!"), 0o644)
	prov.report = func() (DiskReportView, error) {
		return newReporter(DiskCacheConfig{Dir: dir}, nil, testLogger()).run()
	}

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/diskcache", nil))
	if rec.Code != 200 {
		t.Fatalf("diskcache status = %d", rec.Code)
	}
	var rep DiskReportView
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.FileCount != 2 || rep.TotalBytes != int64(len("hello")+len("world!!")) {
		t.Errorf("disk report wrong: %+v", rep)
	}
}
