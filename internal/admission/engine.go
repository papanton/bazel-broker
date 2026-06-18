package admission

import (
	"context"
	"sync"
	"time"
)

// Verdict is the single-word outcome the HTTP layer maps to a status code.
type Verdict int

const (
	Allow Verdict = iota // 200 ALLOW
	Queue                // 202 QUEUE (wait window expired, still waiting)
	Deny                 // 403 DENY  (drain / hard policy)
)

func (v Verdict) String() string {
	switch v {
	case Allow:
		return "ALLOW"
	case Queue:
		return "QUEUE"
	case Deny:
		return "DENY"
	default:
		return "QUEUE"
	}
}

// Request is what the wrapper POSTs to /admission.
type Request struct {
	InvocationID string `json:"invocation_id"`
	Worktree     string `json:"worktree"`
	PID          int    `json:"pid"`
	Targets      string `json:"targets"` // space-joined; the engine splits to a set
}

// Policy is the tunable admission posture.
type Policy struct {
	MaxConcurrent  int           // global semaphore size
	CPUHighWater   float64       // admit only while CPU busy% (0..1) is below this; 0 disables
	RAMPressureMax int           // macOS memory-pressure level to admit up to (1=normal,2=warn,4=critical); 0 disables
	StaggerWindow  time.Duration // delay the 2nd overlapping target set by this; 0 disables
	PollSeconds    time.Duration // server-side long-poll cap (default 25s)
}

// DefaultPolicy returns sane defaults (semaphore-only is the minimum shippable;
// CPU/RAM/stagger layer on top).
func DefaultPolicy() Policy {
	return Policy{
		MaxConcurrent:  2,
		CPUHighWater:   0.85,
		RAMPressureMax: 1, // hold on warn+ (level 2+)
		StaggerWindow:  8 * time.Second,
		PollSeconds:    25 * time.Second,
	}
}

type waiterState int

const (
	wsQueued   waiterState = iota // in FIFO, no verdict yet, no gates held
	wsAdmitted                    // gates acquired, ALLOW buffered/delivered
	wsDone                        // terminal verdict delivered or abandoned
)

type waiter struct {
	req        Request
	state      waiterState
	connected  bool      // a live long-poll handler is selecting on ch
	lastDetach time.Time // when the last handler returned (queued-GC grace)
	// ch is cap-1 BUFFERED + single-shot: schedule() does a non-blocking send, so
	// it can deliver a verdict while holding e.mu without ever deadlocking on a
	// gone handler. The buffered verdict is drained by whichever re-poll attaches.
	ch chan Verdict
}

// Engine is the admission state machine: FIFO queue behind an ordered gate chain.
type Engine struct {
	mu          sync.Mutex
	pollSeconds time.Duration // server-side long-poll cap (the only Policy field read after construction)
	gates       []Gate
	sem         *Semaphore // kept typed for status occupancy
	bucket      *TokenBucket
	stagger     *Stagger
	queue       []*waiter
	byID        map[string]*waiter
	registry    RegistryReader
	paused      bool
	draining    bool

	// recentlyAdmitted counts admits since the last load sample (anti-stampede).
	recentlyAdmitted int

	// queuedGCGrace is how long a timed-out/disconnected queued waiter is kept in
	// the FIFO before GC drops it, tolerating the gap between a 202 and the re-POST.
	queuedGCGrace time.Duration

	// retryTimers tracks armed soft-retry timers per invocation_id so a stagger /
	// token wait re-runs schedule() with no external event.
	retryTimers map[string]*time.Timer

	clock func() time.Time
}

// NewEngine constructs the engine and its gate chain from a policy.
func NewEngine(policy Policy, reg RegistryReader) *Engine {
	if policy.PollSeconds <= 0 {
		policy.PollSeconds = 25 * time.Second
	}
	sem := NewSemaphore(policy.MaxConcurrent)
	bucket := NewTokenBucket(policy.CPUHighWater, policy.RAMPressureMax)
	stagger := NewStagger(policy.StaggerWindow, nil)

	e := &Engine{
		pollSeconds:   policy.PollSeconds,
		sem:           sem,
		bucket:        bucket,
		stagger:       stagger,
		gates:         []Gate{sem, bucket, stagger},
		byID:          make(map[string]*waiter),
		registry:      reg,
		queuedGCGrace: 30 * time.Second,
		retryTimers:   make(map[string]*time.Timer),
		clock:         time.Now,
	}
	return e
}

func (e *Engine) now() time.Time { return e.clock() }

// Admit is called by the HTTP handler. It blocks up to PollSeconds, then returns
// Queue so the wrapper re-polls. Gates are acquired ONLY inside schedule() under
// e.mu and the resulting ALLOW is buffered, so delivery never depends on a
// handler being connected at that instant.
func (e *Engine) Admit(ctx context.Context, req Request) Verdict {
	w := e.attach(req)
	defer e.detach(w)

	// Fast path: a verdict may already be buffered from a prior poll cycle.
	select {
	case v := <-w.ch:
		return v
	default:
	}

	timer := time.NewTimer(e.pollSeconds)
	defer timer.Stop()
	select {
	case v := <-w.ch:
		return v
	case <-timer.C:
		return Queue
	case <-ctx.Done():
		return Queue
	}
}

// attach (re-)attaches a long-poll handler to the waiter for req, deduped by
// invocation_id, and kicks schedule() so a newly-enqueued waiter is considered.
func (e *Engine) attach(req Request) *waiter {
	e.mu.Lock()
	w, ok := e.byID[req.InvocationID]
	if !ok {
		w = &waiter{
			req:   req,
			state: wsQueued,
			ch:    make(chan Verdict, 1),
		}
		e.byID[req.InvocationID] = w
		e.queue = append(e.queue, w)
	} else {
		// Re-poll: refresh request fields (pid/targets may arrive late) without
		// disturbing state or the buffered verdict.
		if req.PID != 0 {
			w.req.PID = req.PID
		}
		if req.Targets != "" {
			w.req.Targets = req.Targets
		}
		if req.Worktree != "" {
			w.req.Worktree = req.Worktree
		}
	}
	w.connected = true
	e.mu.Unlock()

	e.schedule()
	return w
}

// detach clears connected and GC-marks a still-queued waiter whose client is
// gone. It NEVER releases gates for an admitted waiter — that is the wrapper's
// /admission/release job, backed by the registry-event path and the PID reaper.
func (e *Engine) detach(w *waiter) {
	e.mu.Lock()
	defer e.mu.Unlock()
	w.connected = false
	w.lastDetach = e.now()
	// wsQueued holds no gates; wsAdmitted keeps its buffered ALLOW + held gates.
	// GC of a stale queued waiter happens lazily in runQueuedGC (tolerates re-poll).
}

// Release frees a slot for invocationID across all gates and wakes the next
// FIFO waiter. Idempotent: releasing an unknown/already-released id is a no-op.
// This is the PRIMARY happy-path release (POST /admission/release) and is also
// invoked by the registry-terminal-event loop and the PID reaper.
func (e *Engine) Release(invocationID string) {
	e.mu.Lock()
	w := e.byID[invocationID]
	if w != nil {
		delete(e.byID, invocationID)
		w.state = wsDone
	}
	if t := e.retryTimers[invocationID]; t != nil {
		t.Stop()
		delete(e.retryTimers, invocationID)
	}
	e.mu.Unlock()

	for _, g := range e.gates {
		g.Release(invocationID)
	}
	e.schedule()
}

// schedule walks the FIFO head and admits as many waiters as the gate chain
// allows. It NEVER blocks on a waiter channel (cap-1 buffered + non-blocking
// deliver), so it safely holds e.mu.
func (e *Engine) schedule() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.paused || len(e.queue) == 0 {
		// Nothing to do; skip the registry snapshot entirely (the 1s load ticker
		// re-runs schedule() constantly even while idle).
		return
	}
	snap := e.registry.SnapshotActive()
	snap.RecentlyAdmitted = e.recentlyAdmitted

	for len(e.queue) > 0 {
		w := e.queue[0]
		if w.state != wsQueued {
			e.queue = e.queue[1:] // already admitted (ALLOW buffered) or done; pop
			continue
		}
		if e.draining {
			e.deliver(w, Deny)
			w.state = wsDone
			e.queue = e.queue[1:]
			delete(e.byID, w.req.InvocationID)
			continue
		}
		admit, retry := e.tryAllGates(w.req, snap)
		if !admit {
			// FIFO fairness: do NOT skip the head. Arm a soft-retry timer so a
			// stagger/token wait re-runs schedule() even with no external event.
			e.armRetryTimer(w, retry)
			return
		}
		e.acquireAllGates(w.req)
		w.state = wsAdmitted
		e.recentlyAdmitted++
		e.deliver(w, Allow)
		e.queue = e.queue[1:]
		// Keep w in byID until Release so a re-poll re-attaches to the SAME
		// admitted waiter and drains its buffered ALLOW (no duplicate, no second slot).
		snap.Builds = append(snap.Builds, ActiveBuild{
			InvocationID: w.req.InvocationID,
			Targets:      splitTargets(w.req.Targets),
			PID:          w.req.PID,
			StartedAt:    e.now(),
		})
		snap.RecentlyAdmitted = e.recentlyAdmitted
	}
}

func (e *Engine) tryAllGates(req Request, snap RegistrySnapshot) (bool, time.Duration) {
	var maxRetry time.Duration
	for _, g := range e.gates {
		ok, retry := g.Try(context.Background(), req, snap)
		if !ok {
			if retry > maxRetry {
				maxRetry = retry
			}
			return false, maxRetry
		}
	}
	return true, 0
}

func (e *Engine) acquireAllGates(req Request) {
	for _, g := range e.gates {
		g.Acquire(req)
	}
}

// deliver does a non-blocking send on the cap-1 buffered channel. A full buffer
// means a verdict is already pending — drop the duplicate. This is what lets
// schedule() hold e.mu.
func (e *Engine) deliver(w *waiter, v Verdict) {
	select {
	case w.ch <- v:
	default:
	}
}

// armRetryTimer schedules a schedule() re-run after d for a staggered/token-held
// head, deduped per invocation_id. Must hold e.mu.
func (e *Engine) armRetryTimer(w *waiter, d time.Duration) {
	if d <= 0 {
		return
	}
	id := w.req.InvocationID
	if t := e.retryTimers[id]; t != nil {
		t.Stop()
	}
	e.retryTimers[id] = time.AfterFunc(d, func() {
		e.mu.Lock()
		delete(e.retryTimers, id)
		e.mu.Unlock()
		e.schedule()
	})
}

// runQueuedGC drops still-queued waiters whose client disconnected and never
// re-polled within the grace window, so a dead head can't wedge the FIFO.
func (e *Engine) runQueuedGC(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			e.gcQueued()
		}
	}
}

func (e *Engine) gcQueued() {
	e.mu.Lock()
	now := e.now()
	kept := e.queue[:0]
	for _, w := range e.queue {
		if w.state == wsQueued && !w.connected && !w.lastDetach.IsZero() &&
			now.Sub(w.lastDetach) >= e.queuedGCGrace {
			w.state = wsDone
			delete(e.byID, w.req.InvocationID)
			continue
		}
		kept = append(kept, w)
	}
	e.queue = kept
	e.mu.Unlock()
}

// SetPaused flips the pause policy. Paused holds all admissions (no DENY).
func (e *Engine) SetPaused(p bool) {
	e.mu.Lock()
	e.paused = p
	e.mu.Unlock()
	e.schedule()
}

// SetDraining flips drain. Draining DENYs all queued + new waiters; in-flight
// builds finish naturally (drain != kill).
func (e *Engine) SetDraining(d bool) {
	e.mu.Lock()
	e.draining = d
	e.mu.Unlock()
	e.schedule()
}

// Resume clears BOTH paused and draining (ergonomic pairing per E5 §4.1).
func (e *Engine) Resume() {
	e.mu.Lock()
	e.paused = false
	e.draining = false
	e.mu.Unlock()
	e.schedule()
}

// onLoadSample updates the token bucket and resets the anti-stampede counter,
// then re-evaluates (load may have dropped enough to admit the head).
func (e *Engine) onLoadSample(cpu float64, ramP int) {
	e.bucket.setSample(cpu, ramP)
	e.mu.Lock()
	e.recentlyAdmitted = 0
	e.mu.Unlock()
	e.schedule()
}

// Status is the /admission/status snapshot.
type Status struct {
	MaxConcurrent int     `json:"maxConcurrent"`
	Held          int     `json:"held"`
	Queued        int     `json:"queued"`
	Paused        bool    `json:"paused"`
	Draining      bool    `json:"draining"`
	CPU           float64 `json:"cpu"`
	RAMPressure   int     `json:"ram"`
}

// Status returns the current admission status.
func (e *Engine) Status() Status {
	e.mu.Lock()
	queued := e.countQueuedLocked()
	paused, draining := e.paused, e.draining
	e.mu.Unlock()
	cpu, ramP := e.bucket.sample()
	return Status{
		MaxConcurrent: e.sem.Max(),
		Held:          e.sem.Held(),
		Queued:        queued,
		Paused:        paused,
		Draining:      draining,
		CPU:           cpu,
		RAMPressure:   ramP,
	}
}

// QueuedCount reports the number of waiters still queued (for /healthz).
func (e *Engine) QueuedCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.countQueuedLocked()
}

// countQueuedLocked counts waiters still in wsQueued. Caller must hold e.mu.
func (e *Engine) countQueuedLocked() int {
	n := 0
	for _, w := range e.queue {
		if w.state == wsQueued {
			n++
		}
	}
	return n
}

func splitTargets(s string) []string {
	if s == "" {
		return nil
	}
	out := make([]string, 0)
	for k := range toSet([]string{s}) {
		out = append(out, k)
	}
	return out
}
