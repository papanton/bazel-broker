package admission

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func doAdmit(t *testing.T, a *Admitter, id, targets string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"invocation_id":"` + id + `","worktree":"/wt","pid":1,"targets":"` + targets + `"}`
	req := httptest.NewRequest(http.MethodPost, "/admission", strings.NewReader(body))
	rec := httptest.NewRecorder()
	a.Admit(rec, req)
	return rec
}

func TestHTTPAdmitC5StatusBodies(t *testing.T) {
	e := NewEngine(Policy{MaxConcurrent: 1, PollSeconds: 120 * time.Millisecond}, newFakeReg())
	a := NewAdmitter(e)

	// 200 ALLOW
	rec := doAdmit(t, a, "a", "//x")
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != "ALLOW" {
		t.Fatalf("admit: code=%d body=%q, want 200 ALLOW", rec.Code, rec.Body.String())
	}
	// 202 QUEUE (slot full -> poll timeout)
	rec = doAdmit(t, a, "b", "//y")
	if rec.Code != 202 || strings.TrimSpace(rec.Body.String()) != "QUEUE" {
		t.Fatalf("queue: code=%d body=%q, want 202 QUEUE", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("QUEUE response missing Retry-After header")
	}
	// Body must NOT be JSON (C5).
	if strings.Contains(rec.Body.String(), "{") {
		t.Fatalf("body looks like JSON: %q", rec.Body.String())
	}

	// 403 DENY after drain
	e.SetDraining(true)
	rec = doAdmit(t, a, "c", "//z")
	if rec.Code != 403 || strings.TrimSpace(rec.Body.String()) != "DENY" {
		t.Fatalf("drain: code=%d body=%q, want 403 DENY", rec.Code, rec.Body.String())
	}
}

func TestHTTPReleaseIdempotent204(t *testing.T) {
	e := NewEngine(Policy{MaxConcurrent: 1, PollSeconds: 80 * time.Millisecond}, newFakeReg())
	a := NewAdmitter(e)
	doAdmit(t, a, "a", "//x")

	rel := func(id string) int {
		req := httptest.NewRequest(http.MethodPost, "/admission/release", strings.NewReader(`{"invocation_id":"`+id+`"}`))
		rec := httptest.NewRecorder()
		a.Release(rec, req)
		return rec.Code
	}
	if c := rel("a"); c != 204 {
		t.Fatalf("release a: %d, want 204", c)
	}
	if c := rel("a"); c != 204 { // double release
		t.Fatalf("re-release a: %d, want 204", c)
	}
	if c := rel("nope"); c != 204 { // unknown
		t.Fatalf("release unknown: %d, want 204", c)
	}
	if e.sem.Held() != 0 {
		t.Fatalf("held after releases = %d, want 0", e.sem.Held())
	}
}

func TestHTTPPauseResumeDrainStatus(t *testing.T) {
	e := NewEngine(Policy{MaxConcurrent: 1, PollSeconds: 80 * time.Millisecond}, newFakeReg())
	a := NewAdmitter(e)

	call := func(fn http.HandlerFunc, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		rec := httptest.NewRecorder()
		fn(rec, req)
		return rec
	}
	if c := call(a.Pause, "/admission/pause").Code; c != 200 {
		t.Fatalf("pause: %d", c)
	}
	// While paused: a build is HELD (Queue), not denied.
	rec := doAdmit(t, a, "a", "//x")
	if rec.Code != 202 {
		t.Fatalf("paused admit: %d, want 202 QUEUE", rec.Code)
	}
	// Resume releases it.
	if c := call(a.Resume, "/admission/resume").Code; c != 200 {
		t.Fatalf("resume: %d", c)
	}
	rec = doAdmit(t, a, "a", "//x")
	if rec.Code != 200 {
		t.Fatalf("resumed admit: %d, want 200 ALLOW", rec.Code)
	}

	// Status JSON.
	sreq := httptest.NewRequest(http.MethodGet, "/admission/status", nil)
	srec := httptest.NewRecorder()
	a.Status(srec, sreq)
	if srec.Code != 200 {
		t.Fatalf("status: %d", srec.Code)
	}
	var st Status
	if err := json.Unmarshal(srec.Body.Bytes(), &st); err != nil {
		t.Fatalf("status not JSON: %v (%s)", err, srec.Body.String())
	}
	if st.MaxConcurrent != 1 || st.Held != 1 {
		t.Fatalf("status = %+v, want maxConcurrent=1 held=1", st)
	}
}

func TestHTTPAdmitBadRequest(t *testing.T) {
	e := NewEngine(semOnly(1), newFakeReg())
	a := NewAdmitter(e)
	req := httptest.NewRequest(http.MethodPost, "/admission", strings.NewReader("not json"))
	rec := httptest.NewRecorder()
	a.Admit(rec, req)
	if rec.Code != 400 {
		t.Fatalf("bad request: %d, want 400 (wrapper treats non-200/202/403 as fail-open)", rec.Code)
	}
}

func TestPausedHoldsThenAdmitsFIFO(t *testing.T) {
	e := NewEngine(Policy{MaxConcurrent: 2, PollSeconds: 80 * time.Millisecond}, newFakeReg())
	e.SetPaused(true)
	// Two admits while paused both Queue.
	if v := admitWithTimeout(t, e, Request{InvocationID: "a"}, time.Second); v != Queue {
		t.Fatalf("paused a: %v", v)
	}
	if v := admitWithTimeout(t, e, Request{InvocationID: "b"}, time.Second); v != Queue {
		t.Fatalf("paused b: %v", v)
	}
	e.Resume()
	if v := admitWithTimeout(t, e, Request{InvocationID: "a"}, time.Second); v != Allow {
		t.Fatalf("resumed a: %v", v)
	}
	if v := admitWithTimeout(t, e, Request{InvocationID: "b"}, time.Second); v != Allow {
		t.Fatalf("resumed b: %v", v)
	}
}

func TestReaperFreesDeadPIDSlot(t *testing.T) {
	reg := newFakeReg()
	e := NewEngine(Policy{MaxConcurrent: 1, PollSeconds: 80 * time.Millisecond}, reg)
	reg.setAlive(4321, true)
	// Admit holder with a live pid.
	admitWithTimeout(t, e, Request{InvocationID: "holder", PID: 4321}, time.Second)
	if e.sem.Held() != 1 {
		t.Fatalf("held = %d, want 1", e.sem.Held())
	}
	// pid dies; reaper frees the slot.
	reg.setAlive(4321, false)
	e.reapDeadPIDs()
	if e.sem.Held() != 0 {
		t.Fatalf("held after reap = %d, want 0 (dead pid slot freed)", e.sem.Held())
	}
}

// fakeTerm is a controllable TerminalEvents source.
type fakeTerm struct{ ch chan string }

func (f *fakeTerm) TerminalIDs() (<-chan string, func()) { return f.ch, func() {} }

func TestTerminalEventReleasesSlot(t *testing.T) {
	reg := newFakeReg()
	e := NewEngine(Policy{MaxConcurrent: 1, PollSeconds: 80 * time.Millisecond}, reg)
	term := &fakeTerm{ch: make(chan string, 1)}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.runTerminalRelease(ctx, term)

	admitWithTimeout(t, e, Request{InvocationID: "a", PID: 1}, time.Second)
	if e.sem.Held() != 1 {
		t.Fatalf("held = %d, want 1", e.sem.Held())
	}
	// Registry observes the build finish (deregister/BEP/process-gone) -> slot
	// freed server-side, NO wrapper trap involved.
	term.ch <- "a"
	deadline := time.After(time.Second)
	for e.sem.Held() != 0 {
		select {
		case <-deadline:
			t.Fatal("terminal event did not free slot (exec-skips-trap not solved server-side)")
		default:
			time.Sleep(2 * time.Millisecond)
		}
	}
}
