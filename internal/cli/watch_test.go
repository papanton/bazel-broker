package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/papanton/bazel-broker/internal/api"
)

// TestApplyEvent_SnapshotThenBuild asserts snapshot replaces the map and a build
// event upserts by invocation_id, keeping terminal builds.
func TestApplyEvent_SnapshotThenBuild(t *testing.T) {
	state := map[string]api.Build{}
	applyEvent(state, api.Event{Type: api.EventSnapshot, Builds: []api.Build{
		{InvocationID: "a", State: api.StateRunning},
		{InvocationID: "b", State: api.StateRunning},
	}})
	if len(state) != 2 {
		t.Fatalf("snapshot: want 2, got %d", len(state))
	}
	// build event upserts "a" to finished.
	applyEvent(state, api.Event{Type: api.EventBuild, Build: &api.Build{InvocationID: "a", State: api.StateFinished}})
	if state["a"].State != api.StateFinished {
		t.Fatalf("build upsert failed: %+v", state["a"])
	}
	if len(state) != 2 {
		t.Fatalf("terminal build should stay in the map: %d", len(state))
	}
	// a fresh snapshot replaces the whole map.
	applyEvent(state, api.Event{Type: api.EventSnapshot, Builds: []api.Build{{InvocationID: "c"}}})
	if len(state) != 1 || state["c"].InvocationID != "c" {
		t.Fatalf("snapshot did not replace map: %+v", state)
	}
}

// wsServer stands up an httptest server whose /events handler emits the given
// frames then closes. It counts connections so reconnect is observable.
func wsServer(t *testing.T, token string, frames []api.Event) (*Client, *int32) {
	t.Helper()
	var conns int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		atomic.AddInt32(&conns, 1)
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			OriginPatterns: []string{"*"},
		})
		if err != nil {
			return
		}
		defer c.CloseNow()
		ctx := r.Context()
		for _, ev := range frames {
			data, _ := json.Marshal(ev)
			if err := c.Write(ctx, websocket.MessageText, data); err != nil {
				return
			}
		}
		c.Close(websocket.StatusNormalClosure, "done")
	}))
	t.Cleanup(srv.Close)
	c := clientFor(t, srv, token)
	return c, &conns
}

// TestWatch_JSONOnce captures NDJSON for a single connection (--once): the
// snapshot frame then a terminal build event.
func TestWatch_JSONOnce(t *testing.T) {
	c, _ := wsServer(t, "tok", []api.Event{
		{Type: api.EventSnapshot, Builds: []api.Build{{InvocationID: "a", State: api.StateRunning}}},
		{Type: api.EventBuild, Build: &api.Build{InvocationID: "a", State: api.StateFinished}},
	})
	var buf bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := RunWatch(ctx, c, WatchOpts{JSON: true, Once: true}, &buf); err != nil {
		t.Fatalf("watch: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 NDJSON lines, got %d:\n%s", len(lines), buf.String())
	}
	if !strings.Contains(lines[0], `"snapshot"`) {
		t.Errorf("first line should be snapshot: %s", lines[0])
	}
	if !strings.Contains(lines[1], `"build"`) || !strings.Contains(lines[1], `"finished"`) {
		t.Errorf("second line should be a terminal build: %s", lines[1])
	}
}

// TestWatch_Reconnect asserts the loop reconnects after the fake closes the
// connection, consuming a second snapshot. We bound it with a context timeout.
func TestWatch_Reconnect(t *testing.T) {
	c, conns := wsServer(t, "tok", []api.Event{
		{Type: api.EventSnapshot, Builds: []api.Build{{InvocationID: "a"}}},
	})
	var buf bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	// Not --once: should reconnect until ctx expires.
	err := RunWatch(ctx, c, WatchOpts{JSON: true}, &buf)
	if err != nil {
		t.Fatalf("watch reconnect should exit cleanly on ctx cancel, got %v", err)
	}
	if atomic.LoadInt32(conns) < 2 {
		t.Fatalf("expected >=2 connections (reconnect), got %d", atomic.LoadInt32(conns))
	}
}

// TestWatch_ReconnectAcrossUnavailable asserts the loop keeps retrying when the
// redial fails (broker briefly down) rather than exiting ExitUnavailable — the
// launchd-restart case. The fake closes immediately each connect; the client's
// own backoff drives reconnects until ctx expires.
func TestWatch_ReconnectAcrossUnavailable(t *testing.T) {
	// A server that accepts then immediately closes — each accept is a fresh
	// connection, so reaching >1 connection proves the loop reconnected.
	c, conns := wsServer(t, "tok", nil) // no frames: snapshot-less close
	var buf bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 1200*time.Millisecond)
	defer cancel()
	if err := RunWatch(ctx, c, WatchOpts{JSON: true}, &buf); err != nil {
		t.Fatalf("reconnect loop should exit cleanly on ctx cancel, got %v", err)
	}
	if atomic.LoadInt32(conns) < 2 {
		t.Fatalf("expected reconnect (>=2 conns), got %d", atomic.LoadInt32(conns))
	}
}

// TestIsReconnectable covers the error classification directly.
func TestIsReconnectable(t *testing.T) {
	if !isReconnectable(wrap(ExitUnavailable, "down")) {
		t.Error("ExitUnavailable should be reconnectable (broker briefly down)")
	}
	if isReconnectable(wrap(ExitAuth, "401")) {
		t.Error("ExitAuth must NOT be reconnectable")
	}
}

// TestWatch_AuthNoRetry asserts a 401 on the upgrade returns ExitAuth and does NOT
// retry (a single connection attempt).
func TestWatch_AuthNoRetry(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())
	p := writeCfg(t, t.TempDir(), `{"host":"`+u.Hostname()+`","port":`+strconv.Itoa(port)+`,"token":"wrong"}`)
	c, err := NewClient(GlobalOpts{ConfigPath: p, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	werr := RunWatch(ctx, c, WatchOpts{JSON: true}, &bytes.Buffer{})
	if codeOf(werr) != ExitAuth {
		t.Fatalf("want ExitAuth, got %v", werr)
	}
	if n := atomic.LoadInt32(&attempts); n != 1 {
		t.Fatalf("401 should not retry: got %d attempts", n)
	}
}
