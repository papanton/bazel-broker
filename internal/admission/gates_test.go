package admission

import (
	"context"
	"testing"
	"time"
)

func TestTokenBucketHoldsWhenHot(t *testing.T) {
	reg := newFakeReg()
	e := NewEngine(Policy{MaxConcurrent: 10, CPUHighWater: 0.85, PollSeconds: 150 * time.Millisecond}, reg)
	// Simulate a hot machine: cpu=0.99 with free semaphore slots.
	e.bucket.setSample(0.99, 1)
	if v := admitWithTimeout(t, e, Request{InvocationID: "hot"}, time.Second); v != Queue {
		t.Fatalf("hot machine: want Queue (held), got %v", v)
	}
	// Cool down -> admit proceeds.
	e.onLoadSample(0.10, 1)
	if v := admitWithTimeout(t, e, Request{InvocationID: "hot"}, time.Second); v != Allow {
		t.Fatalf("cooled machine: want Allow, got %v", v)
	}
}

func TestTokenBucketRAMPressureHolds(t *testing.T) {
	reg := newFakeReg()
	e := NewEngine(Policy{MaxConcurrent: 10, RAMPressureMax: 1, PollSeconds: 120 * time.Millisecond}, reg)
	e.bucket.setSample(0.0, 2) // pressure warn (>1) -> hold
	if v := admitWithTimeout(t, e, Request{InvocationID: "p"}, time.Second); v != Queue {
		t.Fatalf("ram warn: want Queue, got %v", v)
	}
	e.onLoadSample(0.0, 1)
	if v := admitWithTimeout(t, e, Request{InvocationID: "p"}, time.Second); v != Allow {
		t.Fatalf("ram normal: want Allow, got %v", v)
	}
}

func TestStaggerDelaysOverlapping(t *testing.T) {
	now := time.Now()
	clk := func() time.Time { return now }
	s := NewStagger(8*time.Second, clk)

	// First admit of //app:app.
	a := Request{InvocationID: "a", Targets: "//app:app"}
	if ok, _ := s.Try(context.Background(), a, RegistrySnapshot{}); !ok {
		t.Fatal("first overlapping admit should pass")
	}
	s.Acquire(a)

	// Second overlapping admit within the window -> held with remaining ~window.
	b := Request{InvocationID: "b", Targets: "//app:app"}
	ok, retry := s.Try(context.Background(), b, RegistrySnapshot{})
	if ok {
		t.Fatal("second overlapping admit should be held")
	}
	if retry <= 0 || retry > 8*time.Second {
		t.Fatalf("hold retry = %v, want (0, 8s]", retry)
	}

	// A disjoint target is NOT delayed.
	c := Request{InvocationID: "c", Targets: "//lib:lib"}
	if ok, _ := s.Try(context.Background(), c, RegistrySnapshot{}); !ok {
		t.Fatal("disjoint target should not be staggered")
	}

	// After the window elapses, the overlap clears.
	now = now.Add(9 * time.Second)
	if ok, _ := s.Try(context.Background(), b, RegistrySnapshot{}); !ok {
		t.Fatal("after window, overlapping admit should pass")
	}
}

func TestStaggerEarliestOverlapClockNoStarve(t *testing.T) {
	now := time.Now()
	clk := func() time.Time { return now }
	s := NewStagger(8*time.Second, clk)
	s.Acquire(Request{InvocationID: "a", Targets: "//x"})

	now = now.Add(4 * time.Second)
	// b overlaps a; remaining window is ~4s (not reset to 8s).
	_, retry := s.Try(context.Background(), Request{InvocationID: "b", Targets: "//x"}, RegistrySnapshot{})
	if retry <= 0 || retry > 4*time.Second {
		t.Fatalf("remaining = %v, want ~4s (earliest-overlap clock, not reset)", retry)
	}
}

func TestStaggerConsultsRegistrySnapshot(t *testing.T) {
	now := time.Now()
	s := NewStagger(8*time.Second, func() time.Time { return now })
	// No local record, but a registry build (un-wrapped / passively discovered)
	// is currently building //app:app.
	snap := RegistrySnapshot{Builds: []ActiveBuild{
		{InvocationID: "ext", Targets: []string{"//app:app"}, StartedAt: now.Add(-1 * time.Second)},
	}}
	ok, retry := s.Try(context.Background(), Request{InvocationID: "b", Targets: "//app:app"}, snap)
	if ok || retry <= 0 {
		t.Fatalf("should stagger against registry snapshot build: ok=%v retry=%v", ok, retry)
	}
}

func TestSemaphoreIdempotentAcquire(t *testing.T) {
	s := NewSemaphore(1)
	r := Request{InvocationID: "a"}
	s.Acquire(r)
	s.Acquire(r) // double acquire same id
	if s.Held() != 1 {
		t.Fatalf("held = %d, want 1 (idempotent)", s.Held())
	}
	// A new id is refused (full).
	if ok, _ := s.Try(context.Background(), Request{InvocationID: "b"}, RegistrySnapshot{}); ok {
		t.Fatal("second distinct id should be refused when full")
	}
	// But the already-held id still passes Try (re-poll).
	if ok, _ := s.Try(context.Background(), r, RegistrySnapshot{}); !ok {
		t.Fatal("held id should pass Try on re-poll")
	}
}

func TestStaggerEndToEndDelaysSecondAdmit(t *testing.T) {
	// Engine-level: two overlapping admits; first immediate, second delayed then
	// admitted after the (short) window via the soft-retry timer.
	reg := newFakeReg()
	e := NewEngine(Policy{MaxConcurrent: 10, StaggerWindow: 150 * time.Millisecond, PollSeconds: 2 * time.Second}, reg)

	if v := admitWithTimeout(t, e, Request{InvocationID: "a", Targets: "//app:app"}, time.Second); v != Allow {
		t.Fatalf("a: want immediate Allow, got %v", v)
	}
	start := time.Now()
	if v := admitWithTimeout(t, e, Request{InvocationID: "b", Targets: "//app:app"}, 2*time.Second); v != Allow {
		t.Fatalf("b: want eventual Allow, got %v", v)
	}
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Fatalf("b admitted too fast (%v); stagger window not applied", elapsed)
	}
}
