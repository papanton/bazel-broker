package bep

import (
	"context"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/papanton/bazel-broker/internal/build"
	"github.com/papanton/bazel-broker/internal/metrics"
)

// bepRelPath is the E1-locked per-worktree, relative BEP file (C7). The supervisor
// derives <worktree>/.bazel-broker/bep.json and tails it across rebuilds.
const bepRelPath = ".bazel-broker/bep.json"

// BEPPathFor returns the BEP json path for a worktree (E1 convention).
func BEPPathFor(worktree string) string {
	if worktree == "" {
		return ""
	}
	return filepath.Join(worktree, bepRelPath)
}

// Manager owns one tailer goroutine per WORKTREE (the BEP file is per-worktree and
// reused across builds, so we key on worktree, not invocation id — the join key
// BuildStarted.uuid is bound from the stream itself). Watch is idempotent.
type Manager struct {
	mu    sync.Mutex
	tails map[string]context.CancelFunc // worktree → cancel

	log        *slog.Logger
	reg        Registry
	sink       MetricsSink
	alert      metrics.AlertConfig
	profileURL func(id string) string
	onFinalize func(r *metrics.Row)
}

// NewManager constructs a Manager. reg/sink may be nil in narrow tests.
func NewManager(reg Registry, sink MetricsSink, log *slog.Logger, profileURL func(id string) string, onFinalize func(*metrics.Row)) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{
		tails:      make(map[string]context.CancelFunc),
		log:        log,
		reg:        reg,
		sink:       sink,
		profileURL: profileURL,
		onFinalize: onFinalize,
	}
}

// Watch starts tailing a worktree's BEP file (idempotent per worktree). The
// parent ctx bounds the tailer's lifetime; Stop or ctx cancellation ends it.
func (m *Manager) Watch(ctx context.Context, worktree string) {
	if worktree == "" {
		return
	}
	m.mu.Lock()
	if _, ok := m.tails[worktree]; ok {
		m.mu.Unlock()
		return
	}
	tctx, cancel := context.WithCancel(ctx)
	m.tails[worktree] = cancel
	m.mu.Unlock()

	cfg := streamConfig{
		path:       BEPPathFor(worktree),
		worktree:   worktree,
		log:        m.log.With("worktree", worktree),
		reg:        m.reg,
		sink:       m.sink,
		alert:      m.alert,
		profileURL: m.profileURL,
		onFinalize: m.onFinalize,
	}
	m.log.Info("bep watch start", "worktree", worktree, "bep", cfg.path)
	go func() {
		watchFile(tctx, cfg)
		m.mu.Lock()
		delete(m.tails, worktree)
		m.mu.Unlock()
	}()
}

// Stop ends a worktree's tailer (idempotent; unknown worktree is a no-op).
func (m *Manager) Stop(worktree string) {
	m.mu.Lock()
	cancel := m.tails[worktree]
	delete(m.tails, worktree)
	m.mu.Unlock()
	if cancel != nil {
		cancel()
		m.log.Info("bep watch stop", "worktree", worktree)
	}
}

// Watching reports whether a worktree currently has a tailer (for tests).
func (m *Manager) Watching(worktree string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.tails[worktree]
	return ok
}

// reconcileInterval is how often Run scans the registry for build worktrees to
// watch / unwatch.
const reconcileInterval = 1 * time.Second

// Snapshotter is the registry subset Run polls to discover build worktrees.
type Snapshotter interface {
	Snapshot() []*build.Build
}

// Run is the ingest supervisor: it polls the registry and ensures every active
// build's worktree has a BEP tailer (Watch), tearing tailers down a grace period
// after the build goes terminal (Stop). It blocks until ctx is cancelled.
//
// Worktree-keyed (not invocation-keyed) because the BEP file is per-worktree; a
// terminal build still leaves its worktree watched until grace elapses with no
// active build, so a fast rebuild's stream is never missed.
func (m *Manager) Run(ctx context.Context, snap Snapshotter) {
	if snap == nil {
		<-ctx.Done()
		return
	}
	idleSince := make(map[string]time.Time)
	const grace = 30 * time.Second

	ticker := time.NewTicker(reconcileInterval)
	defer ticker.Stop()
	m.reconcile(ctx, snap, idleSince, grace)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.reconcile(ctx, snap, idleSince, grace)
		}
	}
}

func (m *Manager) reconcile(ctx context.Context, snap Snapshotter, idleSince map[string]time.Time, grace time.Duration) {
	now := time.Now()
	activeWT := make(map[string]bool)
	for _, b := range snap.Snapshot() {
		if b.Worktree == "" {
			continue
		}
		if !b.State.IsTerminal() {
			activeWT[b.Worktree] = true
		}
	}

	// Ensure a tailer for every worktree with an active build.
	for wt := range activeWT {
		delete(idleSince, wt)
		m.Watch(ctx, wt)
	}

	// Tear down tailers whose worktree has had no active build for `grace`.
	m.mu.Lock()
	watched := make([]string, 0, len(m.tails))
	for wt := range m.tails {
		watched = append(watched, wt)
	}
	m.mu.Unlock()
	for _, wt := range watched {
		if activeWT[wt] {
			continue
		}
		if t, ok := idleSince[wt]; !ok {
			idleSince[wt] = now
		} else if now.Sub(t) >= grace {
			m.Stop(wt)
			delete(idleSince, wt)
		}
	}
}
