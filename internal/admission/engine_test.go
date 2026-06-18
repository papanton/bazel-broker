package admission

import (
	"context"
	"sync"
	"testing"
	"time"
)

// fakeReg is a controllable RegistryReader for tests.
type fakeReg struct {
	mu     sync.Mutex
	active []ActiveBuild
	alive  map[int]bool
}

func newFakeReg() *fakeReg { return &fakeReg{alive: map[int]bool{}} }

func (f *fakeReg) SnapshotActive() RegistrySnapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make([]ActiveBuild, len(f.active))
	copy(cp, f.active)
	return RegistrySnapshot{Builds: cp}
}
func (f *fakeReg) Alive(pid int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.alive[pid]
}
func (f *fakeReg) setAlive(pid int, a bool) {
	f.mu.Lock()
	f.alive[pid] = a
	f.mu.Unlock()
}

func semOnly(max int) Policy {
	return Policy{MaxConcurrent: max, PollSeconds: 200 * time.Millisecond}
}

// admitWithTimeout runs Admit with a watchdog so a deadlocked engine fails the
// test instead of hanging.
func admitWithTimeout(t *testing.T, e *Engine, req Request, d time.Duration) Verdict {
	t.Helper()
	ch := make(chan Verdict, 1)
	go func() { ch <- e.Admit(context.Background(), req) }()
	select {
	case v := <-ch:
		return v
	case <-time.After(d):
		t.Fatalf("Admit(%s) deadlocked", req.InvocationID)
		return Deny
	}
}

func TestSemaphoreAdmitQueueRelease(t *testing.T) {
	e := NewEngine(semOnly(2), newFakeReg())
	if v := admitWithTimeout(t, e, Request{InvocationID: "a"}, time.Second); v != Allow {
		t.Fatalf("a: want Allow, got %v", v)
	}
	if v := admitWithTimeout(t, e, Request{InvocationID: "b"}, time.Second); v != Allow {
		t.Fatalf("b: want Allow, got %v", v)
	}
	// Third must Queue (poll timeout) since 2 slots held.
	if v := admitWithTimeout(t, e, Request{InvocationID: "c"}, time.Second); v != Queue {
		t.Fatalf("c: want Queue, got %v", v)
	}
	if got := e.QueuedCount(); got != 1 {
		t.Fatalf("queued = %d, want 1", got)
	}
	// Release a -> c re-poll admits.
	e.Release("a")
	if v := admitWithTimeout(t, e, Request{InvocationID: "c"}, time.Second); v != Allow {
		t.Fatalf("c after release: want Allow, got %v", v)
	}
	if got := e.QueuedCount(); got != 0 {
		t.Fatalf("queued after admit = %d, want 0", got)
	}
}

func TestReleaseIdempotent(t *testing.T) {
	e := NewEngine(semOnly(1), newFakeReg())
	admitWithTimeout(t, e, Request{InvocationID: "a"}, time.Second)
	e.Release("a")
	e.Release("a")       // double release
	e.Release("unknown") // unknown id
	if got := e.sem.Held(); got != 0 {
		t.Fatalf("held after idempotent release = %d, want 0", got)
	}
}

func TestConcurrentAdmitNoDeadlock(t *testing.T) {
	e := NewEngine(semOnly(50), newFakeReg())
	var wg sync.WaitGroup
	results := make([]Verdict, 50)
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			results[n] = e.Admit(context.Background(), Request{InvocationID: id(n)})
		}(i)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent Admit deadlocked")
	}
	for i, v := range results {
		if v != Allow {
			t.Fatalf("admit %d: want Allow, got %v", i, v)
		}
	}
}

// The load-bearing deadlock test: a handler times out to Queue BEFORE schedule()
// admits its waiter; the buffered ALLOW must be drained by the next re-poll
// without re-acquiring, and schedule() must never block.
func TestScheduleNoDeadlockOnTimedOutHandler(t *testing.T) {
	e := NewEngine(semOnly(1), newFakeReg())
	// Hold the single slot.
	admitWithTimeout(t, e, Request{InvocationID: "holder"}, time.Second)

	// "c" polls, times out -> Queue (slot still held by holder).
	if v := admitWithTimeout(t, e, Request{InvocationID: "c"}, time.Second); v != Queue {
		t.Fatalf("c first poll: want Queue, got %v", v)
	}
	// Now release the holder. schedule() will admit c and buffer its ALLOW even
	// though no handler is connected — this must NOT deadlock holding e.mu.
	doneSched := make(chan struct{})
	go func() { e.Release("holder"); close(doneSched) }()
	select {
	case <-doneSched:
	case <-time.After(2 * time.Second):
		t.Fatal("Release/schedule deadlocked sending buffered ALLOW")
	}
	// c re-polls and drains the buffered ALLOW.
	if v := admitWithTimeout(t, e, Request{InvocationID: "c"}, time.Second); v != Allow {
		t.Fatalf("c re-poll: want Allow (buffered), got %v", v)
	}
	if e.sem.Held() != 1 {
		t.Fatalf("held = %d, want 1 (c only; no double-acquire)", e.sem.Held())
	}
}

func TestCtxCancelQueuedDropsWaiter(t *testing.T) {
	e := NewEngine(semOnly(1), newFakeReg())
	e.queuedGCGrace = 20 * time.Millisecond
	admitWithTimeout(t, e, Request{InvocationID: "holder"}, time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan Verdict, 1)
	go func() { got <- e.Admit(ctx, Request{InvocationID: "q"}) }()
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case v := <-got:
		if v != Queue {
			t.Fatalf("cancelled queued: want Queue, got %v", v)
		}
	case <-time.After(time.Second):
		t.Fatal("Admit did not return on ctx cancel")
	}
	// GC the dropped queued waiter.
	time.Sleep(40 * time.Millisecond)
	e.gcQueued()
	if got := e.QueuedCount(); got != 0 {
		t.Fatalf("queued after GC = %d, want 0 (dropped)", got)
	}
}

func TestCtxCancelAdmittedDoesNotReleaseSlot(t *testing.T) {
	e := NewEngine(semOnly(1), newFakeReg())
	ctx, cancel := context.WithCancel(context.Background())
	// Admit "a" then cancel its context — the slot must stay held.
	done := make(chan struct{})
	go func() { e.Admit(ctx, Request{InvocationID: "a"}); close(done) }()
	// Wait until admitted.
	deadline := time.After(time.Second)
	for e.sem.Held() == 0 {
		select {
		case <-deadline:
			t.Fatal("a never admitted")
		default:
			time.Sleep(2 * time.Millisecond)
		}
	}
	cancel()
	<-done
	if e.sem.Held() != 1 {
		t.Fatalf("held after admitted-ctx-cancel = %d, want 1 (slot NOT freed by connection loss)", e.sem.Held())
	}
	// Only an explicit release frees it.
	e.Release("a")
	if e.sem.Held() != 0 {
		t.Fatalf("held after release = %d, want 0", e.sem.Held())
	}
}

func id(n int) string {
	return "id-" + string(rune('A'+n%26)) + "-" + itoa(n)
}
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
