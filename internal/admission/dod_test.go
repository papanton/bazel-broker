package admission

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestDoDThreeBuildsMaxTwo is the epic's headline "Done when": start 3 builds via
// the wrapper with max-concurrency=2; the third must QUEUE, then admit when one of
// the first two finishes — with the slot released SERVER-SIDE (the wrapper here
// does NOT POST /admission/release; release happens via the registry terminal
// event), proving the exec-skips-trap fix.
func TestDoDThreeBuildsMaxTwo(t *testing.T) {
	wrap := wrapperPath(t)
	fake := fakeBazelPath(t)

	reg := newFakeReg()
	e := NewEngine(Policy{MaxConcurrent: 2, PollSeconds: 500 * time.Millisecond}, reg)
	a := NewAdmitter(e)

	// term channel simulates the registry observing a build finish (deregister /
	// BEP BuildFinished / process-gone reconcile). We do NOT route
	// /admission/release here, so the ONLY way a slot frees is server-side.
	term := &fakeTerm{ch: make(chan string, 8)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.runTerminalRelease(ctx, term)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /admission", a.Admit)
	// Deliberately omit /admission/release: the wrapper's release POST 404s, so
	// the only working release path is the server-side terminal event below.
	mux.HandleFunc("GET /admission/status", a.Status)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	tmp := t.TempDir()
	runOne := func(target string, dur string) (int, string) {
		cmd := exec.Command("/bin/bash", wrap, "build", target)
		cmd.Env = append(os.Environ(),
			"BAZEL_REAL="+fake,
			"BROKER_URL="+srv.URL,
			"FAKE_BAZEL_DURATION="+dur,
			"BROKER_CACHE_DIR="+filepath.Join(tmp, "disk"),
			"BROKER_REPO_CACHE="+filepath.Join(tmp, "repo"),
			"BROKER_EVENT_DIR="+filepath.Join(tmp, "bep"),
			"BROKER_PROFILE_DIR="+filepath.Join(tmp, "prof"),
			"CI=",
			"ADMISSION_MAX_ATTEMPTS=240",
		)
		out, err := cmd.CombinedOutput()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
		return code, string(out)
	}

	var wg sync.WaitGroup
	codes := make([]int, 3)
	targets := []string{"//a", "//b", "//c"}
	durs := []string{"3", "3", "1"}
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			codes[n], _ = runOne(targets[n], durs[n])
		}(i)
	}

	// Give the two long builds time to be admitted and //c to queue.
	time.Sleep(800 * time.Millisecond)
	st := e.Status()
	if st.Held != 2 {
		t.Fatalf("held = %d, want 2 (a,b admitted)", st.Held)
	}
	if st.Queued < 1 {
		t.Fatalf("queued = %d, want >=1 (c queued)", st.Queued)
	}

	// Simulate the registry observing build //a finish -> server-side slot release.
	// We must release one of the two admitted ids. The wrapper minted ids we don't
	// know, so instead free a slot by sending a terminal for whatever holds the
	// semaphore: release by walking byID for admitted waiters.
	freeOneAdmitted(e, term)

	// //c (1s build) should now admit and the whole set complete.
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("builds did not all complete; //c likely never admitted after server-side release")
	}
	for i, c := range codes {
		if c != 0 {
			t.Fatalf("build %s exit = %d, want 0", targets[i], c)
		}
	}
}

// freeOneAdmitted picks one currently-admitted invocation id and fires a terminal
// event for it, modeling the registry observing that build finish.
func freeOneAdmitted(e *Engine, term *fakeTerm) {
	e.mu.Lock()
	var id string
	for k, w := range e.byID {
		if w.state == wsAdmitted {
			id = k
			break
		}
	}
	e.mu.Unlock()
	if id != "" {
		term.ch <- id
	}
}
