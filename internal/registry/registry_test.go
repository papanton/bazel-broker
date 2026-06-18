package registry

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/papanton/bazel-broker/internal/api"
	"github.com/papanton/bazel-broker/internal/build"
	"github.com/papanton/bazel-broker/internal/store"
)

func newReg(t *testing.T) (*Registry, *Hub) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "broker.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	hub := NewHub()
	return New(s, hub, nil), hub
}

func TestRegisterDeregister(t *testing.T) {
	reg, _ := newReg(t)
	b, err := reg.Register(&build.Build{InvocationID: "a", Worktree: "/wt/a", Targets: []string{"//x"}})
	if err != nil {
		t.Fatal(err)
	}
	if b.State != build.StateRunning || b.Source != build.SourceRegistered {
		t.Fatalf("register state/source: %+v", b)
	}

	fin, err := reg.Deregister("a", 0)
	if err != nil {
		t.Fatal(err)
	}
	if fin.State != build.StateFinished {
		t.Fatalf("exit 0 => %q, want finished", fin.State)
	}

	reg.Register(&build.Build{InvocationID: "b", Worktree: "/wt/b"})
	failed, _ := reg.Deregister("b", 1)
	if failed.State != build.StateFailed {
		t.Fatalf("exit 1 => %q, want failed", failed.State)
	}
}

func TestRegisterValidation(t *testing.T) {
	reg, _ := newReg(t)
	if _, err := reg.Register(&build.Build{Worktree: "/wt"}); err == nil {
		t.Fatal("expected error for missing invocation_id")
	}
	if _, err := reg.Register(&build.Build{InvocationID: "x"}); err == nil {
		t.Fatal("expected error for missing worktree")
	}
}

func TestDeregisterIdempotent(t *testing.T) {
	reg, _ := newReg(t)
	if b, err := reg.Deregister("nope", 0); err != nil || b != nil {
		t.Fatalf("deregister unknown: b=%v err=%v", b, err)
	}
	reg.Register(&build.Build{InvocationID: "a", Worktree: "/wt"})
	reg.Deregister("a", 0)
	// Second deregister is a no-op success keeping the finished state.
	b, err := reg.Deregister("a", 1)
	if err != nil {
		t.Fatal(err)
	}
	if b.State != build.StateFinished {
		t.Fatalf("re-deregister flipped state to %q", b.State)
	}
}

func TestCounts(t *testing.T) {
	reg, _ := newReg(t)
	reg.Register(&build.Build{InvocationID: "a", Worktree: "/wt"})
	reg.Register(&build.Build{InvocationID: "b", Worktree: "/wt"})
	reg.Deregister("b", 0)
	c := reg.Counts()
	if c.Building != 1 || c.Total != 2 || c.Queued != 0 {
		t.Fatalf("counts = %+v", c)
	}
}

func TestSnapshotOrder(t *testing.T) {
	reg, _ := newReg(t)
	reg.clock = mkClock(time.Date(2026, 6, 17, 9, 0, 0, 0, time.UTC))
	reg.Register(&build.Build{InvocationID: "old", Worktree: "/wt"})
	reg.clock = mkClock(time.Date(2026, 6, 17, 9, 5, 0, 0, time.UTC))
	reg.Register(&build.Build{InvocationID: "new", Worktree: "/wt"})
	snap := reg.Snapshot()
	if snap[0].InvocationID != "new" {
		t.Fatalf("snapshot newest-first broken: %s first", snap[0].InvocationID)
	}
}

func TestBroadcastOnMutation(t *testing.T) {
	reg, hub := newReg(t)
	sub, unsub := hub.Subscribe()
	defer unsub()
	reg.Register(&build.Build{InvocationID: "a", Worktree: "/wt"})
	select {
	case ev := <-sub.Events():
		if ev.Type != api.EventBuild || ev.Build == nil || ev.Build.InvocationID != "a" {
			t.Fatalf("unexpected event: %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("no build event received")
	}
}

func TestSlowSubscriberDropped(t *testing.T) {
	hub := NewHub()
	sub, _ := hub.Subscribe()
	// Overflow the buffer without draining.
	for i := 0; i < subBufferSize+10; i++ {
		hub.Broadcast(api.Event{Type: api.EventBuild, Seq: uint64(i)})
	}
	if hub.SubscriberCount() != 0 {
		t.Fatalf("slow subscriber not dropped: count=%d", hub.SubscriberCount())
	}
	// Channel must be closed (drained then closed).
	drained := false
	for range sub.Events() {
		drained = true
	}
	_ = drained
}

func TestConcurrentRegister(t *testing.T) {
	reg, _ := newReg(t)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			reg.Register(&build.Build{InvocationID: fmt.Sprintf("b%d", n), Worktree: "/wt"})
		}(i)
	}
	wg.Wait()
	if got := len(reg.Snapshot()); got != 50 {
		t.Fatalf("snapshot count = %d, want 50", got)
	}
}

func TestHydrateFromStore(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "broker.db")
	s, _ := store.Open(dbPath, nil)
	s.UpsertBuild(&build.Build{InvocationID: "persisted", Worktree: "/wt", State: build.StateFinished, StartTime: time.Now(), Source: build.SourceRegistered})
	s.Close()

	s2, _ := store.Open(dbPath, nil)
	defer s2.Close()
	reg := New(s2, NewHub(), nil)
	if err := reg.HydrateFromStore(200); err != nil {
		t.Fatal(err)
	}
	if _, ok := reg.Get("persisted"); !ok {
		t.Fatal("hydrated build missing from registry")
	}
}

func TestUpsertDoesNotDowngradeRegistered(t *testing.T) {
	reg, _ := newReg(t)
	reg.Register(&build.Build{InvocationID: "a", Worktree: "/wt", Targets: []string{"//keep"}, PID: 999})
	// A discovered pass for the same PID must not clobber source/targets.
	reg.Upsert(&build.Build{PID: 999, Source: build.SourceDiscovered, Targets: []string{"//discovered"}, ExePath: "/usr/bin/bazel"})
	b, _ := reg.Get("a")
	if b.Source != build.SourceRegistered {
		t.Fatalf("source downgraded to %q", b.Source)
	}
	if len(b.Targets) != 1 || b.Targets[0] != "//keep" {
		t.Fatalf("targets clobbered: %v", b.Targets)
	}
	if b.ExePath != "/usr/bin/bazel" {
		t.Fatalf("ExePath seam not merged: %q", b.ExePath)
	}
}

func TestReapMissingDiscovered(t *testing.T) {
	reg, _ := newReg(t)
	reg.Upsert(&build.Build{InvocationID: "disc", PID: 555, Source: build.SourceDiscovered, Worktree: "/wt"})
	reg.Register(&build.Build{InvocationID: "reg", Worktree: "/wt", PID: 556})
	reg.ReapMissingDiscovered(map[int]bool{}, time.Now()) // none seen
	disc, _ := reg.Get("disc")
	if disc.State != build.StateGone {
		t.Fatalf("discovered build not reaped to gone: %q", disc.State)
	}
	regd, _ := reg.Get("reg")
	if regd.State == build.StateGone {
		t.Fatal("registered build wrongly reaped")
	}
}

func mkClock(t time.Time) func() time.Time { return func() time.Time { return t } }
