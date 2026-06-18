package bep

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/papanton/bazel-broker/internal/build"
	"github.com/papanton/bazel-broker/internal/metrics"
)

// fakeReg is a minimal Registry for dispatch/tailer tests.
type fakeReg struct {
	mu     sync.Mutex
	builds map[string]*build.Build
}

func newFakeReg() *fakeReg { return &fakeReg{builds: map[string]*build.Build{}} }

func (f *fakeReg) Upsert(b *build.Build) (*build.Build, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cur := f.builds[b.InvocationID]
	if cur == nil {
		cur = &build.Build{InvocationID: b.InvocationID}
		f.builds[b.InvocationID] = cur
	}
	if b.Worktree != "" {
		cur.Worktree = b.Worktree
	}
	if b.CacheHitRatio != nil {
		cur.CacheHitRatio = b.CacheHitRatio
	}
	if b.ProfileURL != "" {
		cur.ProfileURL = b.ProfileURL
	}
	cp := *cur
	return &cp, nil
}

func (f *fakeReg) FindByInvocationID(id string) (*build.Build, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.builds[id]
	if !ok {
		return nil, false
	}
	cp := *b
	return &cp, true
}

func (f *fakeReg) get(id string) (*build.Build, bool) { return f.FindByInvocationID(id) }

// fakeSink records UpsertMetrics calls.
type fakeSink struct {
	mu   sync.Mutex
	rows map[string]*metrics.Row
}

func newFakeSink() *fakeSink { return &fakeSink{rows: map[string]*metrics.Row{}} }

func (s *fakeSink) UpsertMetrics(r *metrics.Row, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *r
	s.rows[r.InvocationID] = &cp
	return nil
}
func (s *fakeSink) RecentRatiosForWorktree(string, string, int) ([]float64, error) { return nil, nil }
func (s *fakeSink) row(id string) (*metrics.Row, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[id]
	return r, ok
}

// TestTailerTruncationSurvivesRebuild is the mandatory truncation-supervisor
// check (C7/R2): write a full BEP stream, then TRUNCATE the same file in place
// and write a SECOND build's stream. The supervisor must restart, produce a clean
// second row, and not stall — both builds' metrics land.
func TestTailerTruncationSurvivesRebuild(t *testing.T) {
	warm, err := os.ReadFile("testdata/bep/warm-ios.ndjson")
	if err != nil {
		t.Fatalf("read warm fixture: %v", err)
	}
	full, err := os.ReadFile("testdata/bep/fullcache-ios.ndjson")
	if err != nil {
		t.Fatalf("read full fixture: %v", err)
	}

	wt := t.TempDir()
	bepDir := filepath.Join(wt, ".bazel-broker")
	if err := os.MkdirAll(bepDir, 0o755); err != nil {
		t.Fatal(err)
	}
	bepPath := filepath.Join(bepDir, "bep.json")

	reg := newFakeReg()
	sink := newFakeSink()
	var finalized []string
	var fmu sync.Mutex
	cfg := streamConfig{
		path:       bepPath,
		worktree:   wt,
		log:        testLogger(),
		reg:        reg,
		sink:       sink,
		profileURL: func(id string) string { return "http://127.0.0.1:0/profile/" + id + "/perfetto" },
		onFinalize: func(r *metrics.Row) {
			fmu.Lock()
			finalized = append(finalized, r.InvocationID)
			fmu.Unlock()
		},
	}

	// First build present BEFORE the tailer starts (tests offset-0 read).
	writeFile(t, bepPath, warm)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { watchFile(ctx, cfg); close(done) }()

	// Wait for the first build's row.
	warmID := lastUUID(t, warm)
	waitFor(t, 5*time.Second, func() bool { _, ok := sink.row(warmID); return ok })

	// TRUNCATE in place and write the second build's stream (the certain rebuild case).
	if err := os.Truncate(bepPath, 0); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond) // let the supervisor observe the shrink
	appendFile(t, bepPath, full)

	fullID := lastUUID(t, full)
	waitFor(t, 6*time.Second, func() bool { _, ok := sink.row(fullID); return ok })

	cancel()
	<-done

	if _, ok := sink.row(warmID); !ok {
		t.Errorf("first (warm) build row missing — pre-truncation stream lost")
	}
	r2, ok := sink.row(fullID)
	if !ok {
		t.Fatalf("second (post-truncation) build row missing — supervisor stalled")
	}
	// The second build's counters must NOT bleed the first build's runner counts.
	if r2.DiskCacheHits != 0 {
		t.Errorf("post-truncation row leaked prior counters: disk_cache_hits=%d, want 0", r2.DiskCacheHits)
	}
	// The warm build's ratio enriched the registry build.
	if b, ok := reg.get(warmID); !ok || b.CacheHitRatio == nil || *b.CacheHitRatio < 0.89 {
		t.Errorf("registry build not enriched with cache_hit_ratio: %+v", b)
	}
}

func TestBEPPathFor(t *testing.T) {
	if got := BEPPathFor("/wt/a"); got != "/wt/a/.bazel-broker/bep.json" {
		t.Errorf("BEPPathFor = %q", got)
	}
	if got := BEPPathFor(""); got != "" {
		t.Errorf("empty worktree should yield empty path, got %q", got)
	}
}
