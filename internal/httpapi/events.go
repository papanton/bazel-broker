package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"github.com/antoniospapantoniou/bazel-broker/internal/api"
)

// pingInterval is the WS heartbeat cadence. Heartbeats are protocol ping frames,
// NOT JSON events (frozen contract §2.6).
const pingInterval = 30 * time.Second

// handleEvents upgrades to WebSocket and streams the event envelope:
//  1. one "snapshot" frame (full []api.Build) on connect — a hard guarantee so a
//     fresh client is immediately consistent.
//  2. incremental "build" frames (single api.Build, upsert-by-invocation_id).
//  3. 30s ping frames as heartbeats; drop on failure.
//
// A slow consumer whose hub buffer fills is dropped by the hub (its channel is
// closed); the client reconnects and re-snapshots.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Loopback origins only. Browser auth (E7) is a separate open decision.
		OriginPatterns: []string{"127.0.0.1:*", "localhost:*"},
	})
	if err != nil {
		s.log.Warn("ws accept failed", "err", err)
		return
	}
	defer c.CloseNow()

	ctx := r.Context()

	// Per-connection monotonic seq (independent of the registry's global seq).
	var seq uint64

	// 1. snapshot on connect.
	snap := api.Event{
		Type:   api.EventSnapshot,
		Seq:    seq,
		Builds: s.reg.SnapshotAPI(),
		Ts:     api.FormatTime(time.Now()),
	}
	if err := writeWS(ctx, c, snap); err != nil {
		return
	}

	// 2. subscribe and pump incremental events.
	sub, unsub := s.hub.Subscribe()
	defer unsub()

	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-sub.Events():
			if !ok {
				// Dropped by the hub (slow consumer) — close so the client resyncs.
				c.Close(websocket.StatusPolicyViolation, "slow consumer")
				return
			}
			seq++
			ev.Seq = seq
			if err := writeWS(ctx, c, ev); err != nil {
				return
			}
		case <-ticker.C:
			pctx, cancel := context.WithTimeout(ctx, pingInterval)
			err := c.Ping(pctx)
			cancel()
			if err != nil {
				return
			}
		}
	}
}

// writeWS marshals ev and writes it as a single text frame.
func writeWS(ctx context.Context, c *websocket.Conn, ev api.Event) error {
	data, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return c.Write(wctx, websocket.MessageText, data)
}
