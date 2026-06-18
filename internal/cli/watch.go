package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"time"

	"github.com/antoniospapantoniou/bazel-broker/internal/api"
)

// applyEvent folds one WS event into the local build map. snapshot replaces the
// whole map; build upserts by invocation_id (terminal builds stay — there is no
// "removed" event). Unknown/reserved event types (metrics/alert) are ignored.
func applyEvent(state map[string]api.Build, ev api.Event) {
	switch ev.Type {
	case api.EventSnapshot:
		for k := range state {
			delete(state, k)
		}
		for _, b := range ev.Builds {
			state[b.InvocationID] = b
		}
	case api.EventBuild:
		if ev.Build != nil {
			state[ev.Build.InvocationID] = *ev.Build
		}
	}
}

// mapToSortedSlice returns the state map as a deterministically sorted slice.
func mapToSortedSlice(state map[string]api.Build) []api.Build {
	out := make([]api.Build, 0, len(state))
	for _, b := range state {
		out = append(out, b)
	}
	return sortBuilds(out)
}

// WatchOpts configures a watch run.
type WatchOpts struct {
	JSON  bool // NDJSON: one event object per line (for /verify + `jq -c`)
	Once  bool // exit on the first disconnect instead of reconnecting
	TTY   bool // clear-and-redraw live table (else append a table per event)
	Clock func() time.Time
}

// RunWatch subscribes to WS /events and renders a live-updating view until ctx is
// cancelled (Ctrl-C → clean exit). It reconnects with capped, jittered backoff on
// a dropped connection (each reconnect re-snapshots, so it is lossless); a 401 on
// the upgrade is fatal (ExitAuth, no retry).
func RunWatch(ctx context.Context, c *Client, opt WatchOpts, out io.Writer) error {
	if opt.Clock == nil {
		opt.Clock = time.Now
	}
	backoff := newBackoff(200*time.Millisecond, 5*time.Second)
	for {
		err := watchOnce(ctx, c, opt, out)
		switch {
		case ctx.Err() != nil: // Ctrl-C: clean exit
			return nil
		case isAuthErr(err): // 401 on upgrade: do not spin-retry a bad token
			return err
		case opt.Once:
			// --once: a clean disconnect after a successful connect exits 0; a dial
			// failure (never connected) is a real error.
			if err == nil || isConnClosed(err) {
				return nil
			}
			return err
		case err == nil || isReconnectable(err): // dropped / restarted / broker briefly down → reconnect
			if !opt.JSON && opt.TTY {
				fmt.Fprintln(out, "broker connection lost — reconnecting…")
			}
			if sleepErr := backoff.sleep(ctx); sleepErr != nil {
				return nil // ctx cancelled mid-backoff
			}
			continue
		default:
			return err
		}
	}
}

// watchOnce holds exactly one connection: resets local state from the guaranteed
// snapshot, then applies incremental build events until the connection ends.
func watchOnce(ctx context.Context, c *Client, opt WatchOpts, out io.Writer) error {
	stream, err := c.StreamEvents(ctx)
	if err != nil {
		return err
	}
	defer stream.Close()

	state := map[string]api.Build{}
	for {
		ev, err := stream.Next(ctx)
		if err != nil {
			return err
		}
		applyEvent(state, ev)
		if opt.JSON {
			if err := RenderNDJSON(out, ev); err != nil { // one event per line
				return err
			}
			continue
		}
		drawLive(out, opt, mapToSortedSlice(state))
	}
}

// drawLive renders the current state. On a TTY it clears the screen and redraws a
// header + table in place; otherwise it appends a fresh table per event.
func drawLive(out io.Writer, opt WatchOpts, builds []api.Build) {
	building, queued := 0, 0
	for _, b := range builds {
		switch b.State {
		case api.StateQueued:
			queued++
		case api.StateFinished, api.StateFailed, api.StateKilled, api.StateGone:
			// terminal: not counted as building
		default:
			building++
		}
	}
	header := fmt.Sprintf("%d building · %d queued · updated %s",
		building, queued, opt.Clock().Format("15:04:05"))
	if opt.TTY {
		fmt.Fprint(out, "\x1b[2J\x1b[H") // clear + cursor home
	}
	fmt.Fprintln(out, header)
	RenderBuildsTable(out, builds)
}

// isAuthErr reports whether err is the ExitAuth cliError (401 on the WS upgrade).
func isAuthErr(err error) bool {
	var ce *cliError
	return errors.As(err, &ce) && ce.code == ExitAuth
}

// isReconnectable reports whether the watch loop should reconnect after err: a
// dropped/closed connection, OR a transient ExitUnavailable from a redial (the
// launchd-managed broker briefly down during restart/upgrade). ExitAuth is fatal
// and handled by the caller before this.
func isReconnectable(err error) bool {
	if isConnClosed(err) {
		return true
	}
	var ce *cliError
	return errors.As(err, &ce) && ce.code == ExitUnavailable
}

// isConnClosed reports whether err is a normal/abnormal WS close or EOF — i.e. the
// broker dropped us and we should reconnect.
func isConnClosed(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
		return true
	}
	// coder/websocket close errors and net errors surface as opaque errors here;
	// any non-cliError read failure in the watch loop means the connection ended,
	// so treat it as reconnectable. cliErrors (auth/unavailable) are handled above.
	var ce *cliError
	return !errors.As(err, &ce)
}

// backoff is an exponential, capped, jittered backoff that is interruptible by
// context cancellation.
type backoff struct {
	cur, max time.Duration
	base     time.Duration
}

func newBackoff(base, max time.Duration) *backoff {
	return &backoff{cur: base, max: max, base: base}
}

// sleep waits the current interval (with jitter) then doubles it, capped at max.
// Returns ctx.Err() if the context is cancelled mid-sleep.
func (b *backoff) sleep(ctx context.Context) error {
	d := b.cur
	if d > b.max {
		d = b.max
	}
	jitter := time.Duration(rand.Int63n(int64(d/2) + 1))
	wait := d/2 + jitter
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
	}
	b.cur *= 2
	if b.cur > b.max {
		b.cur = b.max
	}
	return nil
}
