package build

import (
	"testing"
	"time"
)

func TestElapsedTerminal(t *testing.T) {
	start := time.Date(2026, 6, 17, 10, 0, 0, 0, time.UTC)
	b := Build{StartTime: start, EndTime: start.Add(5 * time.Second)}
	// EndTime is set, so now is ignored.
	if got := b.Elapsed(start.Add(time.Hour)); got != 5*time.Second {
		t.Fatalf("Elapsed terminal = %v, want 5s", got)
	}
}

func TestElapsedRunning(t *testing.T) {
	start := time.Date(2026, 6, 17, 10, 0, 0, 0, time.UTC)
	b := Build{StartTime: start} // no EndTime
	if got := b.Elapsed(start.Add(3 * time.Second)); got != 3*time.Second {
		t.Fatalf("Elapsed running = %v, want 3s", got)
	}
}

func TestStateValues(t *testing.T) {
	// Guard the frozen wire strings (C4: running, not building).
	cases := map[State]string{
		StateQueued:   "queued",
		StateRunning:  "running",
		StateFinished: "finished",
		StateFailed:   "failed",
		StateKilled:   "killed",
		StateUnknown:  "unknown",
	}
	for s, want := range cases {
		if string(s) != want {
			t.Errorf("state %q != %q", s, want)
		}
	}
}
