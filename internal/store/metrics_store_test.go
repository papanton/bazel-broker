package store

import (
	"path/filepath"
	"testing"

	"github.com/antoniospapantoniou/bazel-broker/internal/build"
	"github.com/antoniospapantoniou/bazel-broker/internal/metrics"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"), nil)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestMigrateV2Idempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mig.db")
	s1, err := Open(path, nil)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	s1.Close()
	// Re-open: migrate must be a no-op (already v2), not error.
	s2, err := Open(path, nil)
	if err != nil {
		t.Fatalf("second open (idempotent migrate): %v", err)
	}
	defer s2.Close()
	var v int
	if err := s2.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != schemaVersion {
		t.Errorf("user_version = %d, want %d", v, schemaVersion)
	}
}

func TestUpsertGetMetricsRoundTrip(t *testing.T) {
	s := openTestStore(t)
	// FK requires the build row first.
	if err := s.UpsertBuild(&build.Build{
		InvocationID: "inv-1", Worktree: "/wt/a", State: build.StateRunning, Source: build.SourceRegistered,
	}); err != nil {
		t.Fatalf("upsert build: %v", err)
	}

	row := &metrics.Row{
		InvocationID:    "inv-1",
		Worktree:        "/wt/a",
		BEPPath:         "/wt/a/.bazel-broker/bep.json",
		ProfilePath:     "/wt/a/.bazel-broker/command.profile.gz",
		StartedAt:       1000,
		FinishedAt:      2000,
		ExitCode:        0,
		Success:         true,
		HasFinish:       true,
		HasMetrics:      true,
		ActionsCreated:  319,
		ActionsExecuted: 39,
		DiskCacheHits:   35,
		ExecutedRunners: 4,
		ProcessesTotal:  108,
		WallMS:          222,
		SummaryLine:     "108 processes: 35 disk cache hit, 69 internal, 4 worker.",
		RunnerCounts: []metrics.RunnerCount{
			{Name: "total", Count: 108},
			{Name: "disk cache hit", ExecKind: "Remote", Count: 35},
			{Name: "internal", Count: 69},
			{Name: "worker", ExecKind: "Worker", Count: 4},
		},
		Mnemonics: []metrics.Mnemonic{
			{Mnemonic: "SwiftCompile", ActionsCreated: 4, ActionsExecuted: 4},
		},
	}
	if err := s.UpsertMetrics(row, `{"raw":true}`); err != nil {
		t.Fatalf("upsert metrics: %v", err)
	}

	got, raw, ok, err := s.GetMetrics("inv-1")
	if err != nil || !ok {
		t.Fatalf("get metrics: ok=%v err=%v", ok, err)
	}
	if got.DiskCacheHits != 35 || got.ExecutedRunners != 4 || got.ProcessesTotal != 108 {
		t.Errorf("counts mismatch: %+v", got)
	}
	if r, ok := got.CacheHitRatio(); !ok || r < 0.89 || r > 0.90 {
		t.Errorf("ratio = %v (ok=%v), want ~0.897", r, ok)
	}
	if len(got.RunnerCounts) != 4 {
		t.Errorf("runner counts = %d, want 4", len(got.RunnerCounts))
	}
	if len(got.Mnemonics) != 1 || got.Mnemonics[0].Mnemonic != "SwiftCompile" {
		t.Errorf("mnemonics round-trip wrong: %+v", got.Mnemonics)
	}
	if raw != `{"raw":true}` {
		t.Errorf("raw json = %q", raw)
	}

	// Idempotent re-upsert (children replaced, no dup-PK error).
	if err := s.UpsertMetrics(row, `{"raw":true}`); err != nil {
		t.Fatalf("re-upsert metrics: %v", err)
	}
}

func TestRecentRatiosForWorktree(t *testing.T) {
	s := openTestStore(t)
	mk := func(id string, ratioHits, exec int64, fin int64) {
		if err := s.UpsertBuild(&build.Build{InvocationID: id, Worktree: "/wt/b", State: build.StateFinished, Source: build.SourceRegistered}); err != nil {
			t.Fatal(err)
		}
		r := &metrics.Row{InvocationID: id, Worktree: "/wt/b", DiskCacheHits: ratioHits, ExecutedRunners: exec, ProcessesTotal: ratioHits + exec, FinishedAt: fin, HasMetrics: true}
		if err := s.UpsertMetrics(r, ""); err != nil {
			t.Fatal(err)
		}
	}
	mk("b1", 90, 10, 100)
	mk("b2", 95, 5, 200)
	mk("cur", 50, 50, 300)
	ratios, err := s.RecentRatiosForWorktree("/wt/b", "cur", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ratios) != 2 {
		t.Fatalf("expected 2 prior ratios (excluding current), got %d: %v", len(ratios), ratios)
	}
}

func TestDiskReportRoundTrip(t *testing.T) {
	s := openTestStore(t)
	if err := s.InsertDiskReport(DiskReport{TakenAt: 111, CacheDir: "/cache", TotalBytes: 4096, FileCount: 3, OldestMtime: 99, GCFreed: 0}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.LatestDiskReport()
	if err != nil || !ok {
		t.Fatalf("latest disk report: ok=%v err=%v", ok, err)
	}
	if got.TotalBytes != 4096 || got.FileCount != 3 {
		t.Errorf("disk report mismatch: %+v", got)
	}
}
