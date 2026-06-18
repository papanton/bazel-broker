package admission

import (
	"context"
	"time"
)

// Background cadences.
const (
	loadSampleInterval = 1 * time.Second
	staggerGCInterval  = 5 * time.Second
	queuedGCInterval   = 5 * time.Second
	reaperInterval     = 5 * time.Second
)

// Run starts the engine's background loops and blocks until ctx is cancelled:
//
//   - load sampler: refreshes the token bucket every 1s and re-schedules,
//   - stagger GC: reclaims aged cache-warm records,
//   - queued GC: drops disconnected queued waiters past the grace window,
//   - terminal-event release: frees a slot when the registry observes a build go
//     terminal (deregister / BEP finished / process-gone reconcile) — this is the
//     exec-skips-trap fix, independent of the wrapper,
//   - PID reaper: backstop that frees slots whose pid is dead.
//
// probe may be nil (load gating disabled). term may be nil (no Hub wired; the
// reaper still backstops release).
func (e *Engine) Run(ctx context.Context, probe LoadProbe, term TerminalEvents) {
	if probe == nil {
		probe = nopProbe{}
	}

	go e.stagger.runGC(ctx, staggerGCInterval)
	go e.runQueuedGC(ctx, queuedGCInterval)
	go e.runReaper(ctx, reaperInterval)
	if term != nil {
		go e.runTerminalRelease(ctx, term)
	}

	// Load sampler (this goroutine).
	t := time.NewTicker(loadSampleInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if cpu, ramP, err := probe.Sample(); err == nil {
				e.onLoadSample(cpu, ramP)
			}
		}
	}
}

// runTerminalRelease releases the slot for any build the registry reports as
// terminal. This is the server-side release that makes the wrapper's exec safe:
// even though the wrapper replaces its shell (so no EXIT trap fires), the broker
// frees the slot the instant it sees the build finish.
func (e *Engine) runTerminalRelease(ctx context.Context, term TerminalEvents) {
	ids, cancel := term.TerminalIDs()
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return
		case id, ok := <-ids:
			if !ok {
				return
			}
			if id != "" {
				e.Release(id)
			}
		}
	}
}

// runReaper frees slots whose owning pid is dead — the backstop for a wrapper
// that crashed/was kill -9'd after ALLOW but before /admission/release, and for
// an admitted-but-disconnected waiter whose buffered ALLOW no client will drain.
func (e *Engine) runReaper(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			e.reapDeadPIDs()
		}
	}
}

func (e *Engine) reapDeadPIDs() {
	// Snapshot the held invocation ids + pids under e.mu, then probe liveness
	// outside the lock (Alive may syscall).
	type heldSlot struct {
		id  string
		pid int
	}
	e.mu.Lock()
	var held []heldSlot
	for id, w := range e.byID {
		if w.state == wsAdmitted && w.req.PID > 0 {
			held = append(held, heldSlot{id, w.req.PID})
		}
	}
	e.mu.Unlock()

	for _, h := range held {
		if !e.registry.Alive(h.pid) {
			e.Release(h.id)
		}
	}
}
