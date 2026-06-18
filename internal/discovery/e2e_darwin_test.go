//go:build darwin

package discovery

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/antoniospapantoniou/bazel-broker/internal/build"
	"github.com/antoniospapantoniou/bazel-broker/internal/registry"
)

// TestE2EDiscoverAndKill is the full E3 "Done when": launch fake-bazel.sh (long
// duration) inside a temp git worktree -> the real libproc reconciler surfaces it with
// source="discovered" and the correct worktree -> the Killer makes it exit within the
// grace window. No wrapper is involved.
func TestE2EDiscoverAndKill(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	// Match the fake-bazel interpreter exe (/bin/bash) via the test allow hook; the
	// cwd->worktree gate is what actually discriminates a client.
	t.Setenv("BB_DISCOVERY_EXE_ALLOW", "bash$")
	resetExeAllow()
	t.Cleanup(resetExeAllow)

	root, _ := filepath.EvalSymlinks(t.TempDir())
	wt := filepath.Join(root, "workspace-wt-a")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, wt, "init", "-q")

	script := fakeBazelPath(t)
	cmd := exec.Command("/bin/bash", script, "build", "//:app")
	cmd.Dir = wt
	cmd.Env = append(cmd.Environ(), "FAKE_BAZEL_DURATION=120")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start fake-bazel: %v", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	time.Sleep(300 * time.Millisecond) // perl re-exec + trap install

	reg := registry.New(nil, nil, nil)
	rc := NewReconciler(NewScanner(), reg, nil, time.Second)
	if err := rc.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}

	// (1) It must appear with source=discovered and the correct worktree.
	var found *build.Build
	for _, b := range reg.Snapshot() {
		if b.Worktree == wt {
			found = b
			break
		}
	}
	if found == nil {
		t.Fatalf("discovered build with worktree %q not found; snapshot=%+v", wt, reg.Snapshot())
	}
	if found.Source != build.SourceDiscovered {
		t.Errorf("source = %q, want discovered", found.Source)
	}
	if found.WorktreeName != "workspace-wt-a" {
		t.Errorf("worktree_name = %q, want workspace-wt-a", found.WorktreeName)
	}
	if found.State != build.StateRunning {
		t.Errorf("state = %q, want running", found.State)
	}

	// (2) Killing it via the Killer HTTP handler makes it exit within the grace window.
	k := NewKiller(reg, KillConfig{}, nil, rc.ReconcileOnce)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /builds/{invocation_id}/kill", k.Kill)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	start := time.Now()
	resp, err := http.Post(srv.URL+"/builds/"+found.InvocationID+"/kill",
		"application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("kill status = %d, want 200", resp.StatusCode)
	}

	select {
	case werr := <-exited:
		elapsed := time.Since(start)
		if elapsed > time.Second {
			t.Errorf("exited after %v, want <1s", elapsed)
		}
		code := exitCode(werr)
		if code != 8 && code != 137 && !signalled(werr) {
			t.Errorf("exit code = %d, want 8 or 137", code)
		}
		t.Logf("E2E: discovered worktree=%q, killed in %v code=%d", wt, elapsed, code)
	case <-time.After(2 * time.Second):
		t.Fatal("fake-bazel did not exit within 2s of kill")
	}
}
