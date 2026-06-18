package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/antoniospapantoniou/bazel-broker/internal/build"
)

func tempStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "broker.db")
	s, err := Open(path, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestUpsertAndGet(t *testing.T) {
	s := tempStore(t)
	start := time.Date(2026, 6, 17, 9, 0, 0, 0, time.UTC)
	b := &build.Build{
		InvocationID: "x1",
		Worktree:     "/wt/a",
		Targets:      []string{"//app:App", "//lib:lib"},
		PID:          1234,
		State:        build.StateRunning,
		StartTime:    start,
		Source:       build.SourceRegistered,
	}
	if err := s.UpsertBuild(b); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.GetBuild("x1")
	if err != nil || !ok {
		t.Fatalf("GetBuild ok=%v err=%v", ok, err)
	}
	if got.Worktree != "/wt/a" || got.PID != 1234 || len(got.Targets) != 2 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if !got.StartTime.Equal(start) {
		t.Fatalf("start_time mismatch: got %v want %v", got.StartTime, start)
	}
	if got.State != build.StateRunning {
		t.Fatalf("state = %q", got.State)
	}
}

func TestMarkTerminal(t *testing.T) {
	s := tempStore(t)
	start := time.Date(2026, 6, 17, 9, 0, 0, 0, time.UTC)
	end := start.Add(2 * time.Minute)
	b := &build.Build{InvocationID: "x2", Worktree: "/wt/b", State: build.StateRunning, StartTime: start, Source: build.SourceRegistered}
	if err := s.UpsertBuild(b); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkTerminal("x2", build.StateFinished, 0, end); err != nil {
		t.Fatal(err)
	}
	got, _, _ := s.GetBuild("x2")
	if got.State != build.StateFinished || got.ExitCode != 0 || !got.EndTime.Equal(end) {
		t.Fatalf("terminal mismatch: %+v", got)
	}
}

func TestRecentBuildsOrder(t *testing.T) {
	s := tempStore(t)
	base := time.Date(2026, 6, 17, 9, 0, 0, 0, time.UTC)
	for i, id := range []string{"old", "mid", "new"} {
		b := &build.Build{
			InvocationID: id, Worktree: "/wt", State: build.StateRunning,
			StartTime: base.Add(time.Duration(i) * time.Minute), Source: build.SourceRegistered,
		}
		if err := s.UpsertBuild(b); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.RecentBuilds(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].InvocationID != "new" || got[2].InvocationID != "old" {
		t.Fatalf("order wrong: %v", ids(got))
	}
}

func TestSchemaVersion(t *testing.T) {
	s := tempStore(t)
	var v int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != schemaVersion {
		t.Fatalf("user_version = %d, want %d", v, schemaVersion)
	}
}

func TestEmptyTargetsRoundTrip(t *testing.T) {
	s := tempStore(t)
	b := &build.Build{InvocationID: "e", Worktree: "/wt", State: build.StateRunning, StartTime: time.Now(), Source: build.SourceDiscovered}
	if err := s.UpsertBuild(b); err != nil {
		t.Fatal(err)
	}
	got, _, _ := s.GetBuild("e")
	if got.Targets == nil || len(got.Targets) != 0 {
		t.Fatalf("targets = %v, want empty non-nil", got.Targets)
	}
}

func ids(bs []*build.Build) []string {
	out := make([]string, len(bs))
	for i, b := range bs {
		out[i] = b.InvocationID
	}
	return out
}
