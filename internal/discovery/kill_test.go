package discovery

import (
	"context"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"
)

// fakeBazelPath locates testdata/fake-bazel.sh relative to the repo root.
func fakeBazelPath(t *testing.T) string {
	t.Helper()
	// internal/discovery -> repo root is ../../
	p, err := filepath.Abs(filepath.Join("..", "..", "testdata", "fake-bazel.sh"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestKillFakeBazelGraceful launches fake-bazel.sh BACKGROUNDED (as the E3 contract /
// OD-A requires) and kills it. Per C6/OD-A a backgrounded stub may die gracefully (exit
// 8 via the SIGTERM trap) OR by SIGKILL (137); we assert it EXITED WITHIN THE GRACE
// WINDOW and accept either. True graceful-cancel fidelity is only provable against real
// bazel in the foreground, not the backgrounded stub.
func TestKillFakeBazelGraceful(t *testing.T) {
	script := fakeBazelPath(t)
	cmd := exec.Command("/bin/bash", script, "build", "//:app")
	cmd.Dir = t.TempDir()
	cmd.Env = append(cmd.Environ(), "FAKE_BAZEL_DURATION=120")
	// New process group so the SIGINT-to-group path is exercised.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start fake-bazel: %v", err)
	}
	pid := cmd.Process.Pid
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	time.Sleep(200 * time.Millisecond) // let the perl re-exec + trap install settle

	start := time.Now()
	outcome, err := killProc(context.Background(), KillConfig{}, KillSpec{PID: pid})
	if err != nil {
		t.Fatalf("killProc: %v (outcome=%s)", err, outcome)
	}
	if outcome != OutcomeGraceful && outcome != OutcomeSIGKILL && outcome != OutcomeAlreadyGone {
		t.Fatalf("unexpected outcome %q", outcome)
	}

	select {
	case werr := <-exited:
		elapsed := time.Since(start)
		if elapsed > time.Second {
			t.Errorf("process exited after %v, want <1s", elapsed)
		}
		code := exitCode(werr)
		// Accept 8 (graceful SIGTERM trap) OR 137 (128+9 SIGKILL) OR -1/signal forms.
		if code != 8 && code != 137 && !signalled(werr) {
			t.Errorf("exit code = %d (err=%v), want 8 or 137", code, werr)
		}
		t.Logf("fake-bazel exited in %v, code=%d, outcome=%s", elapsed, code, outcome)
	case <-time.After(2 * time.Second):
		t.Fatal("process did not exit within 2s of kill")
	}
}

// TestKillForceSkipsGrace verifies Force goes straight to SIGKILL on a child that
// ignores graceful signals.
func TestKillForceSkipsGrace(t *testing.T) {
	// `sleep` does not trap SIGINT/SIGTERM specially, but Force should SIGKILL it fast.
	cmd := exec.Command("/bin/sleep", "120")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	pid := cmd.Process.Pid
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	time.Sleep(100 * time.Millisecond)

	start := time.Now()
	outcome, err := killProc(context.Background(), KillConfig{}, KillSpec{PID: pid, Force: true})
	if err != nil {
		t.Fatalf("killProc force: %v", err)
	}
	if outcome != OutcomeSIGKILL {
		t.Errorf("outcome = %q, want sigkill", outcome)
	}
	select {
	case <-exited:
		if d := time.Since(start); d > 500*time.Millisecond {
			t.Errorf("force kill took %v, want fast", d)
		}
	case <-time.After(time.Second):
		t.Fatal("force-killed process did not exit")
	}
}

// TestKillAlreadyGone: a pid that has already exited returns OutcomeAlreadyGone.
func TestKillAlreadyGone(t *testing.T) {
	cmd := exec.Command("/bin/sleep", "0.01")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Wait()
	time.Sleep(20 * time.Millisecond)
	outcome, err := killProc(context.Background(), KillConfig{}, KillSpec{PID: pid})
	if err != nil {
		t.Fatalf("killProc: %v", err)
	}
	if outcome != OutcomeAlreadyGone {
		t.Errorf("outcome = %q, want already_gone", outcome)
	}
}

// TestKillGuard0RefusesWrongExe: a KillSpec whose ExpectExe no longer matches the live
// exe is refused (OutcomeError, no signal delivered). darwin only (GUARD 0 needs cgo
// pidPath); on other platforms the guard is skipped by design.
func TestKillGuard0RefusesWrongExe(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("GUARD 0 exe re-validation is darwin-only")
	}
	cmd := exec.Command("/bin/sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	time.Sleep(100 * time.Millisecond)

	outcome, err := killProc(context.Background(), KillConfig{},
		KillSpec{PID: pid, ExpectExe: "/usr/bin/totally-different-binary"})
	if err == nil {
		t.Fatalf("expected refusal error, got outcome=%q nil err", outcome)
	}
	if outcome != OutcomeError {
		t.Errorf("outcome = %q, want error", outcome)
	}
	// The helper must still be alive (no signal delivered).
	if gone, _ := pidGone(pid); gone {
		t.Error("GUARD 0 refused but the process was killed anyway")
	}
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	return -1
}

func signalled(err error) bool {
	if ee, ok := err.(*exec.ExitError); ok {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok {
			return ws.Signaled()
		}
	}
	return false
}
