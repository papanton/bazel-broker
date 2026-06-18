// Package registry is the in-memory source of truth for "what is building right
// now", mirrored write-through to the SQLite store and fanned out to WS clients.
//
// Concurrency model (E2 §2.3): one sync.RWMutex guards the build map. Reads
// (/builds, /healthz) take the read lock; mutations take the write lock, persist
// to the store, then broadcast an api.Event AFTER releasing the lock so a slow
// WS consumer never stalls a mutation.
package registry

import (
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/antoniospapantoniou/bazel-broker/internal/api"
	"github.com/antoniospapantoniou/bazel-broker/internal/build"
	"github.com/antoniospapantoniou/bazel-broker/internal/store"
)

// Counts is the live aggregate for /healthz.
type Counts struct {
	Building int // non-terminal, non-queued builds
	Queued   int // state == queued
	Total    int // all known builds
}

// Registry is the in-memory build registry with write-through persistence.
type Registry struct {
	mu     sync.RWMutex
	builds map[string]*build.Build
	store  *store.Store
	hub    *Hub
	log    *slog.Logger
	clock  func() time.Time
	seq    atomic.Uint64 // monotonic event seq (per registry; per-conn seq is the WS layer's)
}

// New constructs a Registry. store and hub may be nil in narrow tests, but the
// daemon always supplies both.
func New(s *store.Store, hub *Hub, log *slog.Logger) *Registry {
	if log == nil {
		log = slog.Default()
	}
	return &Registry{
		builds: make(map[string]*build.Build),
		store:  s,
		hub:    hub,
		log:    log,
		clock:  time.Now,
	}
}

// now returns the registry clock (injectable for tests).
func (r *Registry) now() time.Time { return r.clock().UTC() }

// Register inserts or updates a build (idempotent on InvocationID). A re-register
// of a non-terminal build is treated as a heartbeat/update. Required: InvocationID
// and Worktree. Returns the stored build.
func (r *Registry) Register(b *build.Build) (*build.Build, error) {
	if b.InvocationID == "" {
		return nil, fmt.Errorf("invocation_id is required")
	}
	if b.Worktree == "" {
		return nil, fmt.Errorf("worktree is required")
	}

	r.mu.Lock()
	now := r.now()
	stored, existed := r.builds[b.InvocationID]
	if !existed {
		stored = &build.Build{
			InvocationID: b.InvocationID,
			StartTime:    now,
		}
		r.builds[b.InvocationID] = stored
	}
	stored.Worktree = b.Worktree
	if len(b.Targets) > 0 {
		stored.Targets = b.Targets
	}
	if b.PID != 0 {
		stored.PID = b.PID
	}
	if b.Source != "" {
		stored.Source = b.Source
	} else if stored.Source == "" {
		stored.Source = build.SourceRegistered
	}
	// A fresh register (or re-register of a non-terminal build) is running.
	if !stored.State.IsTerminal() {
		stored.State = build.StateRunning
	}
	snapshot := *stored
	r.mu.Unlock()

	r.persist(&snapshot)
	r.log.Info("build registered", "invocation_id", snapshot.InvocationID,
		"worktree", snapshot.Worktree, "state", string(snapshot.State))
	r.broadcastBuild(&snapshot, now)
	return &snapshot, nil
}

// Deregister marks a build terminal by exit code (0 => finished, non-0 => failed)
// and stamps EndTime. Idempotent: an unknown or already-terminal id is a no-op
// success (the returned build may be nil if the id was never seen).
func (r *Registry) Deregister(invocationID string, exitCode int) (*build.Build, error) {
	if invocationID == "" {
		return nil, fmt.Errorf("invocation_id is required")
	}

	r.mu.Lock()
	stored, ok := r.builds[invocationID]
	if !ok || stored.State.IsTerminal() {
		var snap *build.Build
		if ok {
			s := *stored
			snap = &s
		}
		r.mu.Unlock()
		return snap, nil // no-op success
	}
	now := r.now()
	if exitCode == 0 {
		stored.State = build.StateFinished
	} else {
		stored.State = build.StateFailed
	}
	stored.ExitCode = exitCode
	stored.EndTime = now
	snapshot := *stored
	r.mu.Unlock()

	r.persist(&snapshot)
	r.log.Info("build deregistered", "invocation_id", snapshot.InvocationID,
		"state", string(snapshot.State), "exit_code", exitCode)
	r.broadcastBuild(&snapshot, now)
	return &snapshot, nil
}

// Snapshot returns a copy of all builds, newest StartTime first.
func (r *Registry) Snapshot() []*build.Build {
	r.mu.RLock()
	out := make([]*build.Build, 0, len(r.builds))
	for _, b := range r.builds {
		cp := *b
		out = append(out, &cp)
	}
	r.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		return out[i].StartTime.After(out[j].StartTime)
	})
	return out
}

// SnapshotAPI returns the snapshot as wire DTOs (newest first), elapsed relative
// to now. Maps directly under the read lock to avoid an intermediate copy of the
// domain builds (runs on every WS connect and every GET /builds).
func (r *Registry) SnapshotAPI() []api.Build {
	now := r.now()
	r.mu.RLock()
	out := make([]api.Build, 0, len(r.builds))
	for _, b := range r.builds {
		out = append(out, b.ToAPI(now))
	}
	r.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		return out[i].StartTime > out[j].StartTime // RFC3339 UTC strings sort lexically by time
	})
	return out
}

// Get returns one build by id (ok=false if absent).
func (r *Registry) Get(invocationID string) (*build.Build, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	b, ok := r.builds[invocationID]
	if !ok {
		return nil, false
	}
	cp := *b
	return &cp, true
}

// Counts returns live aggregate counts for /healthz.
func (r *Registry) Counts() Counts {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var c Counts
	for _, b := range r.builds {
		c.Total++
		switch {
		case b.State == build.StateQueued:
			c.Queued++
		case !b.State.IsTerminal():
			c.Building++
		}
	}
	return c
}

// HydrateFromStore loads the most recent n builds from SQLite into the in-memory
// map at boot so /builds and the WS snapshot are continuous across restarts.
func (r *Registry) HydrateFromStore(n int) error {
	if r.store == nil {
		return nil
	}
	builds, err := r.store.RecentBuilds(n)
	if err != nil {
		return err
	}
	r.mu.Lock()
	for _, b := range builds {
		r.builds[b.InvocationID] = b
	}
	count := len(r.builds)
	r.mu.Unlock()
	r.log.Info("hydrated from store", "builds", count)
	return nil
}

// persist write-through to the store (best-effort; logs on error).
func (r *Registry) persist(b *build.Build) {
	if r.store == nil {
		return
	}
	if err := r.store.UpsertBuild(b); err != nil {
		r.log.Error("store upsert failed", "invocation_id", b.InvocationID, "err", err)
	}
}

// broadcastBuild emits a "build" event for b. Called after the write-lock is
// released so a slow WS consumer cannot stall a mutation.
func (r *Registry) broadcastBuild(b *build.Build, now time.Time) {
	if r.hub == nil {
		return
	}
	dto := b.ToAPI(now)
	r.hub.Broadcast(api.Event{
		Type:  api.EventBuild,
		Seq:   r.seq.Add(1),
		Build: &dto,
		Ts:    api.FormatTime(now),
	})
}

// ---- E3 reconciliation seams (E3 fills the bodies in its T5; signatures frozen) ----

// Upsert merges a discovered build without clobbering a richer registered record.
// Dedupe priority: by PID, then by InvocationID, else insert as discovered. A
// discovered pass must never downgrade a SourceRegistered build.
func (r *Registry) Upsert(b *build.Build) (*build.Build, error) {
	if b.InvocationID == "" && b.PID == 0 {
		return nil, fmt.Errorf("upsert needs invocation_id or pid")
	}

	r.mu.Lock()
	now := r.now()
	var stored *build.Build
	if b.PID != 0 {
		for _, ex := range r.builds {
			if ex.PID == b.PID {
				stored = ex
				break
			}
		}
	}
	if stored == nil && b.InvocationID != "" {
		stored = r.builds[b.InvocationID]
	}
	if stored == nil {
		stored = &build.Build{
			InvocationID: b.InvocationID,
			StartTime:    now,
			Source:       build.SourceDiscovered,
			State:        build.StateRunning,
		}
		if stored.InvocationID == "" {
			stored.InvocationID = fmt.Sprintf("pid-%d", b.PID)
		}
		r.builds[stored.InvocationID] = stored
	}
	// Merge: never downgrade a registered record's source/targets.
	if b.Worktree != "" {
		stored.Worktree = b.Worktree
	}
	if b.WorktreeName != "" {
		stored.WorktreeName = b.WorktreeName
	}
	if len(b.Targets) > 0 && stored.Source != build.SourceRegistered {
		stored.Targets = b.Targets
	}
	if b.PID != 0 {
		stored.PID = b.PID
	}
	if b.ExePath != "" {
		stored.ExePath = b.ExePath
	}
	if b.Cwd != "" {
		stored.Cwd = b.Cwd
	}
	if b.GitDir != "" {
		stored.GitDir = b.GitDir
	}
	stored.LastSeen = now
	snapshot := *stored
	r.mu.Unlock()

	r.persist(&snapshot)
	r.broadcastBuild(&snapshot, now)
	return &snapshot, nil
}

// ReapMissingDiscovered marks discovered builds whose PID is absent from `seen`
// as StateGone. SourceRegistered builds are untouched (their lifecycle is
// Deregister, owned by E5).
func (r *Registry) ReapMissingDiscovered(seen map[int]bool, now time.Time) {
	var reaped []build.Build
	r.mu.Lock()
	for _, b := range r.builds {
		if b.Source != build.SourceDiscovered || b.State.IsTerminal() {
			continue
		}
		if b.PID != 0 && !seen[b.PID] {
			b.State = build.StateGone
			b.EndTime = now
			reaped = append(reaped, *b)
		}
	}
	r.mu.Unlock()

	for i := range reaped {
		r.persist(&reaped[i])
		r.broadcastBuild(&reaped[i], now)
	}
}

// FindByPID returns the build with the given pid (ok=false if absent).
func (r *Registry) FindByPID(pid int) (*build.Build, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, b := range r.builds {
		if b.PID == pid {
			cp := *b
			return &cp, true
		}
	}
	return nil, false
}

// FindByInvocationID is an alias of Get matching the E3 §4.3 method name.
func (r *Registry) FindByInvocationID(id string) (*build.Build, bool) {
	return r.Get(id)
}
