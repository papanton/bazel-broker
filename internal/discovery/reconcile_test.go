package discovery

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/antoniospapantoniou/bazel-broker/internal/build"
	"github.com/antoniospapantoniou/bazel-broker/internal/registry"
)

// fakeScanner returns a fixed set of ProcInfo (no syscalls), so reconcile is testable
// on any platform.
type fakeScanner struct{ procs []ProcInfo }

func (f *fakeScanner) Snapshot() ([]ProcInfo, error) { return f.procs, nil }

func TestReconcileUpsertAndReap(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	base := t.TempDir()
	real, _ := filepath.EvalSymlinks(base)
	wtA := filepath.Join(real, "wt-a")
	if err := os.MkdirAll(wtA, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, wtA, "init", "-q")

	reg := registry.New(nil, nil, nil)

	fs := &fakeScanner{procs: []ProcInfo{
		{PID: 4242, ExePath: "/bin/bash", Cwd: wtA},
		{PID: 9999, ExePath: "/bin/bash", Cwd: "/private/tmp"}, // not in a worktree -> dropped
	}}
	rc := NewReconciler(fs, reg, nil, time.Second)

	if err := rc.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}

	b, ok := reg.FindByPID(4242)
	if !ok {
		t.Fatal("discovered build for pid 4242 not in registry")
	}
	if b.Worktree != wtA {
		t.Errorf("worktree = %q, want %q", b.Worktree, wtA)
	}
	if b.WorktreeName != "wt-a" {
		t.Errorf("worktree_name = %q, want wt-a", b.WorktreeName)
	}
	if b.Source != build.SourceDiscovered {
		t.Errorf("source = %q, want discovered", b.Source)
	}
	if b.State != build.StateRunning {
		t.Errorf("state = %q, want running", b.State)
	}
	if b.ExePath != "/bin/bash" {
		t.Errorf("exe_path = %q", b.ExePath)
	}
	// The non-worktree proc must NOT have been inserted.
	if _, ok := reg.FindByPID(9999); ok {
		t.Error("pid 9999 (not in worktree) should have been dropped")
	}

	// Drop the proc -> next pass reaps it to StateGone.
	fs.procs = nil
	if err := rc.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("ReconcileOnce (reap): %v", err)
	}
	b2, ok := reg.FindByPID(4242)
	if !ok {
		t.Fatal("build vanished entirely; expected reaped-but-present")
	}
	if b2.State != build.StateGone {
		t.Errorf("after reap state = %q, want gone", b2.State)
	}
}

// TestReconcileSkipsUnsupported confirms an ErrUnsupported scanner (non-darwin) is a
// soft no-op, not a crash.
func TestReconcileSkipsUnsupported(t *testing.T) {
	reg := registry.New(nil, nil, nil)
	rc := NewReconciler(stubLike{}, reg, nil, time.Second)
	if err := rc.ReconcileOnce(context.Background()); err != ErrUnsupported {
		t.Errorf("got %v, want ErrUnsupported", err)
	}
	// Run drives one pass and then must not panic when the loop sees ErrUnsupported.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	rc.interval = 10 * time.Millisecond
	rc.Run(ctx) // returns when ctx expires
}

type stubLike struct{}

func (stubLike) Snapshot() ([]ProcInfo, error) { return nil, ErrUnsupported }
