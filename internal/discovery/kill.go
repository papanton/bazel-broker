package discovery

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"time"
)

// Kill timings. Grace stays under the <1s acceptance budget; the SIGKILL settle window
// is short because SIGKILL cannot be trapped.
const (
	DefaultGrace  = 750 * time.Millisecond
	DefaultPoll   = 25 * time.Millisecond
	sigkillSettle = 250 * time.Millisecond
)

// Outcome is the terminal result of a Kill.
type Outcome string

const (
	OutcomeGraceful    Outcome = "sigint"       // exited within grace after the graceful signal
	OutcomeSIGKILL     Outcome = "sigkill"      // escalated to SIGKILL
	OutcomeCancelled   Outcome = "cancelled"    // D4 command-server Cancel (flag-gated, off)
	OutcomeAlreadyGone Outcome = "already_gone" // pid already absent (ESRCH) == success
	OutcomeError       Outcome = "error"
)

// KillConfig tunes the kill state machine.
type KillConfig struct {
	Grace     time.Duration // graceful window before SIGKILL; <=0 => DefaultGrace
	PollEvery time.Duration // exit-poll interval; <=0 => DefaultPoll
	UseCancel bool          // D4: try command-server Cancel first (off by default; no-op today)
}

func (c KillConfig) grace() time.Duration {
	if c.Grace <= 0 {
		return DefaultGrace
	}
	return c.Grace
}

func (c KillConfig) poll() time.Duration {
	if c.PollEvery <= 0 {
		return DefaultPoll
	}
	return c.PollEvery
}

// KillSpec is what a kill request resolves into. ExpectExe is the exe path discovery
// recorded for this pid; it is re-validated immediately before signalling to close the
// PID-reuse window. Force skips the graceful step and goes straight to SIGKILL.
type KillSpec struct {
	PID       int
	ExpectExe string
	Force     bool
}

// killProc runs the SIGINT/SIGTERM -> grace -> SIGKILL state machine. It is the pure
// engine behind the Killer HTTP handler.
func killProc(ctx context.Context, cfg KillConfig, s KillSpec) (Outcome, error) {
	pid := s.PID
	if pid <= 0 {
		return OutcomeError, fmt.Errorf("invalid pid %d", pid)
	}

	// GUARD 0 (PID-reuse, mandatory): re-validate the live exe path is STILL the bazel
	// client discovery recorded. A recycled pid pointing at a different binary is
	// refused, not signalled. ESRCH here == already gone == success. Only on darwin
	// (pidPath is cgo); ExpectExe == "" skips it.
	if s.ExpectExe != "" {
		cur, err := livePidPath(int32(pid))
		switch {
		case errors.Is(err, errProcGone):
			return OutcomeAlreadyGone, nil
		case errors.Is(err, errProcUnavailable):
			// Cannot confirm identity. In this sudo-free single-user tool our targets
			// are ours; an EPERM means the pid is now a foreign process -> refuse.
			return OutcomeError, fmt.Errorf("pid %d: cannot verify identity before kill: %w", pid, err)
		case err != nil && !errors.Is(err, errUnsupportedPidPath):
			return OutcomeError, fmt.Errorf("pid %d: identity check failed: %w", pid, err)
		case err == nil && cur != s.ExpectExe:
			return OutcomeError, fmt.Errorf("pid %d reused (exe %q != expected %q); refusing to signal", pid, cur, s.ExpectExe)
		}
	}

	if gone, _ := pidGone(pid); gone {
		return OutcomeAlreadyGone, nil
	}

	if s.Force {
		return sigkill(ctx, cfg, pid)
	}

	// D4 OPTIONAL out-of-band Cancel, tried first only when enabled. Today it always
	// falls through to the signal path (the passive broker has no command_id; see D4).
	if cfg.UseCancel {
		if tryCommandServerCancel(pid) == nil {
			if waitGone(ctx, pid, cfg.grace(), cfg.poll()) {
				return OutcomeCancelled, nil
			}
		}
	}

	// Step 1: graceful signal. Send SIGINT to the process GROUP (real bazel client
	// cancellation, and a foreground stub) AND SIGTERM to the pid (the backgrounded
	// fake-bazel stub honors SIGTERM -> exit 8; bash 3.2 ignores SIGINT when
	// `&`-launched). At least one lands gracefully; see CLAUDE.md / OD-A.
	gracefulSignal(pid)

	// Step 2: grace window.
	if waitGone(ctx, pid, cfg.grace(), cfg.poll()) {
		return OutcomeGraceful, nil
	}

	// Step 3: escalate to SIGKILL.
	return sigkill(ctx, cfg, pid)
}

// gracefulSignal delivers the graceful cancel. Errors are swallowed: ESRCH means the
// proc is already gone (handled by the subsequent waitGone), and a missing process
// group is non-fatal.
func gracefulSignal(pid int) {
	// SIGINT to the process group (negative pid). Real bazel honors SIGINT.
	_ = syscall.Kill(-pid, syscall.SIGINT)
	// SIGTERM to the pid. The backgrounded fake-bazel honors SIGTERM -> exit 8.
	_ = syscall.Kill(pid, syscall.SIGTERM)
}

func sigkill(ctx context.Context, cfg KillConfig, pid int) (Outcome, error) {
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return OutcomeSIGKILL, nil // already gone == killed
		}
		return OutcomeError, err
	}
	if waitGone(ctx, pid, sigkillSettle, cfg.poll()) {
		return OutcomeSIGKILL, nil
	}
	return OutcomeError, fmt.Errorf("pid %d survived SIGKILL", pid)
}

// pidGone reports whether the pid definitively does not exist (ESRCH). EPERM is
// "exists but not ours" -> not gone, but surfaced so waitGone never spins forever on a
// recycled foreign pid. For OUR OWN processes signal-0 returns nil or ESRCH, never
// EPERM, so EPERM here means the pid was recycled to another user's process.
func pidGone(pid int) (bool, error) {
	err := syscall.Kill(pid, 0)
	switch {
	case err == nil:
		return false, nil
	case errors.Is(err, syscall.ESRCH):
		return true, syscall.ESRCH
	default:
		return false, err // EPERM etc.: exists but not ours -> treat as not-gone but flag
	}
}

// waitGone polls until the pid is ESRCH-gone or the budget expires. An EPERM (foreign,
// recycled pid) ends the wait early with false rather than spinning the whole budget.
func waitGone(ctx context.Context, pid int, budget, every time.Duration) bool {
	deadline := time.Now().Add(budget)
	t := time.NewTimer(every)
	defer t.Stop()
	for {
		gone, err := pidGone(pid)
		if gone {
			return true
		}
		if err != nil && !errors.Is(err, syscall.ESRCH) {
			return false // EPERM: not ours anymore; do not claim success, do not spin
		}
		if !time.Now().Before(deadline) {
			return false
		}
		t.Reset(every)
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
		}
	}
}

// tryCommandServerCancel is the D4 out-of-band Cancel sketch. It is flag-gated (off by
// default) and a no-op stub today: the passive broker has no in-flight command_id
// (only the E5 wrapper that issued the Run RPC holds it), and command_id is not written
// to <output_base>/server/. So this always returns an error and the caller falls
// through to the signal path. Kept so the API/flag contract is stable for E5.
func tryCommandServerCancel(pid int) error {
	return fmt.Errorf("command-server cancel unavailable: no command_id for passive broker (pid %d)", pid)
}
