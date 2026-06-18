package discovery

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/antoniospapantoniou/bazel-broker/internal/build"
	"github.com/antoniospapantoniou/bazel-broker/internal/registry"
)

// DefaultInterval is the reconcile cadence. 1s is fast enough that a build appears in
// `ls` "instantly" for the acceptance test, and a full proc_listpids sweep (a few
// hundred PIDs) costs well under a millisecond.
const DefaultInterval = time.Second

// resolveFunc maps a cwd to a worktree (worktree resolution seam, injectable for tests).
type resolveFunc func(string) (Worktree, error)

// Reconciler folds discovered bazel client processes into the E2 registry on a ticker,
// without clobbering wrapper-registered builds, and reaps the discovered ones that
// vanish. The dedupe/precedence rules live inside registry.Upsert (E2 §2.3).
type Reconciler struct {
	scan     Scanner
	reg      *registry.Registry
	resolve  resolveFunc
	clock    func() time.Time
	interval time.Duration
	log      *slog.Logger

	unsupportedOnce sync.Once // log the non-darwin "unsupported" exactly once
}

// NewReconciler wires a Scanner + registry into a reconcile loop. log/clock may be nil
// (defaults applied). interval <= 0 uses DefaultInterval.
func NewReconciler(scan Scanner, reg *registry.Registry, log *slog.Logger, interval time.Duration) *Reconciler {
	if log == nil {
		log = slog.Default()
	}
	if interval <= 0 {
		interval = DefaultInterval
	}
	return &Reconciler{
		scan:     scan,
		reg:      reg,
		resolve:  ResolveFromCwd,
		clock:    time.Now,
		interval: interval,
		log:      log,
	}
}

// Run drives ReconcileOnce on a ticker until ctx is cancelled. It runs one pass
// immediately so a build shows up without waiting a full tick. Intended to be started
// in its own goroutine from main.go: `go disco.Run(ctx)`.
func (r *Reconciler) Run(ctx context.Context) {
	r.reconcileLogged(ctx)
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.reconcileLogged(ctx)
		}
	}
}

func (r *Reconciler) reconcileLogged(ctx context.Context) {
	if err := r.ReconcileOnce(ctx); err != nil {
		if errors.Is(err, ErrUnsupported) {
			r.unsupportedOnce.Do(func() {
				r.log.Info("discovery unsupported on this platform; skipping reconcile loop")
			})
			return
		}
		r.log.Warn("discovery reconcile failed", "err", err)
	}
}

// ReconcileOnce performs one discovery pass and merges into the registry. Exported so
// the HTTP layer can reconcile-on-demand immediately before a kill lookup (keeps the
// captured ExePath fresh, hardening PID reuse).
func (r *Reconciler) ReconcileOnce(ctx context.Context) error {
	procs, err := r.scan.Snapshot()
	if err != nil {
		return err
	}
	now := r.clock().UTC()
	seenPIDs := make(map[int]bool, len(procs))

	for _, p := range procs {
		wt, err := r.resolve(p.Cwd)
		if err != nil {
			continue // not in a worktree -> not a build we surface (filters servers/strays)
		}
		pid := int(p.PID) // libproc int32 -> E2's int at the registry boundary
		seenPIDs[pid] = true

		// Hand an E2 build.Build to E2's Upsert. The dedupe/precedence (by PID, then
		// InvocationID; never downgrade SourceRegistered) lives inside Upsert. A
		// brand-new discovered process with no invocation_id is keyed "pid-<pid>" by
		// Upsert until a real id arrives.
		if _, err := r.reg.Upsert(&build.Build{
			PID:          pid,
			ExePath:      p.ExePath,
			Cwd:          p.Cwd,
			Worktree:     wt.Root,
			WorktreeName: wt.Name,
			GitDir:       wt.GitDir,
			Source:       build.SourceDiscovered,
			State:        build.StateRunning,
		}); err != nil {
			r.log.Warn("discovery upsert failed", "pid", pid, "err", err)
		}
	}

	// Reap discovered builds whose PID vanished (-> StateGone). Registered builds are
	// untouched (their lifecycle is Deregister, owned by E5).
	r.reg.ReapMissingDiscovered(seenPIDs, now)
	return nil
}
