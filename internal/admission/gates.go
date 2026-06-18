package admission

import (
	"context"
	"strings"
	"sync"
	"time"
)

// Gate is one admission stage. Try is non-blocking: it reports whether a request
// may pass this gate RIGHT NOW, plus a soft retryAfter hint (0 = wake me on the
// next release/event; >0 = re-evaluate after this long even with no event).
// Acquire/Release bookkeep occupancy and are called only when ALL gates admit.
type Gate interface {
	Name() string
	Try(ctx context.Context, req Request, snap RegistrySnapshot) (admit bool, retryAfter time.Duration)
	Acquire(req Request)
	Release(invocationID string)
}

// ---- Gate 1: global semaphore (max N concurrent) ----

// Semaphore caps the number of concurrently admitted builds. Acquire/Try are
// idempotent on invocation_id (an already-counted id never double-acquires), a
// belt-and-suspenders against re-poll double-acquire.
type Semaphore struct {
	mu   sync.Mutex
	max  int
	held map[string]struct{}
}

// NewSemaphore constructs a semaphore admitting at most max concurrent builds.
func NewSemaphore(max int) *Semaphore {
	if max < 1 {
		max = 1
	}
	return &Semaphore{max: max, held: make(map[string]struct{})}
}

func (s *Semaphore) Name() string { return "semaphore" }

func (s *Semaphore) Try(_ context.Context, req Request, _ RegistrySnapshot) (bool, time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.held[req.InvocationID]; ok {
		return true, 0 // already counted
	}
	return len(s.held) < s.max, 0
}

func (s *Semaphore) Acquire(req Request) {
	s.mu.Lock()
	s.held[req.InvocationID] = struct{}{}
	s.mu.Unlock()
}

func (s *Semaphore) Release(id string) {
	s.mu.Lock()
	delete(s.held, id)
	s.mu.Unlock()
}

// Held reports the current occupancy (for /admission/status).
func (s *Semaphore) Held() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.held)
}

// Max reports the configured limit (for /admission/status).
func (s *Semaphore) Max() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.max
}

// ---- Gate 2: CPU / RAM token bucket (load-aware) ----

// rampPerHead is the synthetic CPU reserved per just-admitted-but-not-yet-
// reflected build (anti-stampede debit), so schedule() can't admit a herd inside
// one ~1s load sample window before their clang/swiftc spawn shows up in load.
const rampPerHead = 0.10

// TokenBucket refuses admission when the machine is already hot even if a
// semaphore slot is free. It reads a cached load sample (refreshed by the
// engine's 1s ticker) and debits rampPerHead per in-ramp build.
type TokenBucket struct {
	cpuHi   float64 // CPUHighWater in [0,1]; admit only while effCPU < this
	ramPmax int     // RAMPressureMax (admit while pressure level <= this)

	mu   sync.Mutex
	cpu  float64
	ramP int
}

// NewTokenBucket builds a token-bucket gate. cpuHi is the CPU high-water in
// [0,1]; ramPmax is the macOS memory-pressure level to admit up to
// (1=normal, 2=warn, 4=critical).
func NewTokenBucket(cpuHi float64, ramPmax int) *TokenBucket {
	return &TokenBucket{cpuHi: cpuHi, ramPmax: ramPmax, ramP: 1}
}

func (t *TokenBucket) Name() string { return "tokenbucket" }

// setSample is called by the engine's load ticker.
func (t *TokenBucket) setSample(cpu float64, ramP int) {
	t.mu.Lock()
	t.cpu, t.ramP = cpu, ramP
	t.mu.Unlock()
}

func (t *TokenBucket) Try(_ context.Context, _ Request, snap RegistrySnapshot) (bool, time.Duration) {
	t.mu.Lock()
	cpu, ramP, cpuHi, ramPmax := t.cpu, t.ramP, t.cpuHi, t.ramPmax
	t.mu.Unlock()
	if cpuHi > 0 {
		effCPU := cpu + float64(snap.RecentlyAdmitted)*rampPerHead
		if effCPU >= cpuHi {
			return false, 2 * time.Second
		}
	}
	if ramPmax > 0 && ramP > ramPmax {
		return false, 2 * time.Second
	}
	return true, 0
}

// Occupancy is the semaphore's job; the bucket is stateless on Acquire/Release.
func (t *TokenBucket) Acquire(Request) {}
func (t *TokenBucket) Release(string)  {}
func (t *TokenBucket) sample() (float64, int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cpu, t.ramP
}

// ---- Gate 3: stagger (anti-thundering-herd) ----

type admitRecord struct {
	targets    map[string]struct{}
	admittedAt time.Time
}

// Stagger holds the SECOND of two overlapping-target builds for StaggerWindow so
// the first can populate --disk_cache and the second hits instead of recomputing
// the identical actions. Overlap is normalized-string equality on raw target
// patterns (no wildcard expansion in v1). The hold is the REMAINING window of the
// EARLIEST overlap, never reset by later arrivals, so a steady overlap stream
// cannot starve later requests.
type Stagger struct {
	window time.Duration
	clock  func() time.Time
	mu     sync.Mutex
	recent map[string]admitRecord
}

// NewStagger builds a stagger gate with the given delay window. clock may be nil
// (defaults to time.Now); tests inject a fake clock.
func NewStagger(window time.Duration, clock func() time.Time) *Stagger {
	if clock == nil {
		clock = time.Now
	}
	return &Stagger{window: window, clock: clock, recent: make(map[string]admitRecord)}
}

func (s *Stagger) Name() string { return "stagger" }

func (s *Stagger) Try(_ context.Context, req Request, snap RegistrySnapshot) (bool, time.Duration) {
	if s.window <= 0 {
		return true, 0
	}
	want := toSet([]string{req.Targets})
	if len(want) == 0 {
		return true, 0 // no visible targets -> never staggered (documented false-negative)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock()
	var hold time.Duration

	consider := func(targets map[string]struct{}, admittedAt time.Time) {
		age := now.Sub(admittedAt)
		if age >= s.window || age < 0 {
			return
		}
		if overlaps(want, targets) {
			if rem := s.window - age; rem > hold {
				hold = rem
			}
		}
	}

	for id, rec := range s.recent {
		if now.Sub(rec.admittedAt) >= s.window {
			delete(s.recent, id) // opportunistic GC
			continue
		}
		// Don't stagger against our own already-admitted record (re-poll case).
		if id == req.InvocationID {
			continue
		}
		consider(rec.targets, rec.admittedAt)
	}
	for _, b := range snap.Builds {
		if b.InvocationID == req.InvocationID {
			continue
		}
		consider(toSet(b.Targets), b.StartedAt)
	}
	if hold > 0 {
		return false, hold
	}
	return true, 0
}

func (s *Stagger) Acquire(req Request) {
	s.mu.Lock()
	s.recent[req.InvocationID] = admitRecord{toSet([]string{req.Targets}), s.clock()}
	s.mu.Unlock()
}

// Release keeps the record so the cache-warm window still applies after a very
// short first build; runGC reclaims it once it ages past the window.
func (s *Stagger) Release(string) {}

// runGC drops records older than the window so `recent` can never grow unbounded.
func (s *Stagger) runGC(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.mu.Lock()
			now := s.clock()
			for id, rec := range s.recent {
				if now.Sub(rec.admittedAt) >= s.window {
					delete(s.recent, id)
				}
			}
			s.mu.Unlock()
		}
	}
}

// toSet normalizes raw target-pattern strings into a set. Normalization: trim
// whitespace and canonicalize a redundant `@//` repo prefix to `//`. Labels are
// case-sensitive, so we lower-case nothing.
func toSet(targets []string) map[string]struct{} {
	out := make(map[string]struct{}, len(targets))
	for _, raw := range targets {
		for _, t := range strings.Fields(raw) {
			t = strings.TrimSpace(t)
			t = strings.TrimPrefix(t, "@//")
			if t != "" {
				out[t] = struct{}{}
			}
		}
	}
	return out
}

// overlaps reports whether two normalized target sets share any element.
func overlaps(a, b map[string]struct{}) bool {
	if len(b) < len(a) {
		a, b = b, a
	}
	for k := range a {
		if _, ok := b[k]; ok {
			return true
		}
	}
	return false
}
