//go:build darwin

package discovery

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestSelfPID verifies the cgo libproc bindings against the test process itself:
// listPIDs includes our pid, pidPath returns our exe, pidCwd returns our cwd. This is
// the OD-D readability check in CI form — same-user cwd reads must succeed.
func TestSelfPID(t *testing.T) {
	tmp := t.TempDir()
	// Resolve symlinks (/var -> /private/var on macOS) so the comparison is stable.
	realTmp, err := filepath.EvalSymlinks(tmp)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	cwd0, _ := os.Getwd()
	if err := os.Chdir(realTmp); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd0) })

	self := int32(os.Getpid())

	pids, err := listPIDs()
	if err != nil {
		t.Fatalf("listPIDs: %v", err)
	}
	found := false
	for _, p := range pids {
		if p == self {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("listPIDs did not include self pid %d", self)
	}

	path, err := pidPath(self)
	if err != nil {
		t.Fatalf("pidPath(self): %v", err)
	}
	if !filepath.IsAbs(path) {
		t.Errorf("pidPath returned non-absolute path %q", path)
	}

	cwd, err := pidCwd(self)
	if err != nil {
		t.Fatalf("pidCwd(self): %v (OD-D: same-user cwd read denied?)", err)
	}
	if cwd != realTmp {
		t.Errorf("pidCwd = %q, want %q", cwd, realTmp)
	}
}

// TestClassifyErrno checks the EPERM != ESRCH split on a foreign (launchd, pid 1) and a
// non-existent pid via the public cwd path.
func TestErrnoSplit(t *testing.T) {
	// launchd (pid 1) is root-owned -> EPERM on cwd read.
	if _, err := pidCwd(1); err != errProcUnavailable {
		// Not strictly guaranteed, but on stock macOS pid 1 is unreadable. Tolerate a
		// generic error but flag if it is ESRCH (would mean pid 1 vanished).
		if err == errProcGone {
			t.Errorf("pidCwd(1) returned errProcGone; pid 1 should exist")
		}
	}
	// A very high, almost-certainly-unused pid -> ESRCH.
	if _, err := pidCwd(2_000_000_000); err != errProcGone {
		t.Errorf("pidCwd(huge pid) = %v, want errProcGone", err)
	}
}

// TestSnapshotFindsBazelClient launches a `sleep` and matches it via the
// BB_DISCOVERY_EXE_ALLOW test hook, proving Snapshot picks up a "bazel client" and
// reads its cwd.
func TestSnapshotFindsBazelClient(t *testing.T) {
	t.Setenv("BB_DISCOVERY_EXE_ALLOW", "sleep$")
	// Reset the once-cached regex for this test.
	resetExeAllow()
	t.Cleanup(resetExeAllow)

	tmp := t.TempDir()
	realTmp, _ := filepath.EvalSymlinks(tmp)
	cmd := exec.Command("/bin/sleep", "30")
	cmd.Dir = realTmp
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	time.Sleep(150 * time.Millisecond)

	procs, err := NewScanner().Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	var got *ProcInfo
	for i := range procs {
		if int(procs[i].PID) == cmd.Process.Pid {
			got = &procs[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("Snapshot did not find sleep pid %d", cmd.Process.Pid)
	}
	if got.Cwd != realTmp {
		t.Errorf("discovered cwd = %q, want %q", got.Cwd, realTmp)
	}
}
