package discovery

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sync"
)

// ErrUnsupported is returned by the non-darwin Scanner stub so the broker compiles
// (and degrades cleanly) on CI/Linux while real discovery only runs on macOS.
var ErrUnsupported = errors.New("discovery: process scanning is only supported on darwin")

// errProcUnavailable (EPERM: alive but unreadable) and errProcGone (ESRCH: exited
// mid-scan) are KEPT DISTINCT so the reaper can reap on ESRCH and the killer can treat
// "gone" as success while still re-validating on EPERM. Declared platform-neutral
// because kill.go (all platforms) branches on them; classifyErrno (darwin/cgo) is the
// only producer.
var (
	errProcUnavailable = errors.New("discovery: process info unavailable (EPERM)")
	errProcGone        = errors.New("discovery: process gone (ESRCH)")
)

// errUnsupportedPidPath means kill-time exe re-validation (GUARD 0) is not available on
// this platform. Only the non-darwin livePidPath returns it; on darwin GUARD 0 uses the
// real cgo proc_pidpath. kill.go treats it as "skip the identity check".
var errUnsupportedPidPath = errors.New("discovery: live pid path unsupported")

// ProcInfo is one discovered process: its PID, executable path (proc_pidpath) and
// current working directory (PROC_PIDVNODEPATHINFO). The Cwd is what places a build
// in a worktree.
type ProcInfo struct {
	PID     int32
	ExePath string
	Cwd     string
}

// Scanner is the seam behind D-stack-1: the libproc impl is the darwin default, but
// a shell-out (lsof/ps) impl could satisfy the same interface if D-stack-1 flips.
type Scanner interface {
	// Snapshot returns all bazel client processes currently running. On non-darwin
	// it returns ErrUnsupported.
	Snapshot() ([]ProcInfo, error)
}

// envAllowOnce compiles BB_DISCOVERY_EXE_ALLOW once. When set it is a test hook that
// REPLACES the built-in allowlist (lets tests match fake-bazel.sh's interpreter exe,
// e.g. /bin/bash). Unset in production.
var (
	envAllowOnce sync.Once
	envAllowRe   *regexp.Regexp
)

func exeAllowRe() *regexp.Regexp {
	envAllowOnce.Do(func() {
		if pat := os.Getenv("BB_DISCOVERY_EXE_ALLOW"); pat != "" {
			envAllowRe = regexp.MustCompile(pat)
		}
	})
	return envAllowRe
}

// resetExeAllow re-reads BB_DISCOVERY_EXE_ALLOW on the next call (test hook for
// t.Setenv, which the production sync.Once would otherwise cache past).
func resetExeAllow() {
	envAllowOnce = sync.Once{}
	envAllowRe = nil
}

// isBazelClient is the cheap first-pass exe filter. It is NOT sufficient on its own to
// separate client from server — the authoritative discriminator is the cwd→worktree
// resolution done by the reconciler (the server's cwd is the output base, which has no
// reachable .git and is therefore dropped). When BB_DISCOVERY_EXE_ALLOW is set it is
// the ONLY allowlist (test hook). In production the built-in basename allowlist applies.
func isBazelClient(exePath string) bool {
	if re := exeAllowRe(); re != nil {
		return re.MatchString(exePath)
	}
	switch filepath.Base(exePath) {
	case "bazel", "bazelisk", "bazel-real":
		return true
	}
	return false
}
