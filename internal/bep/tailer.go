package bep

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/nxadm/tail"

	"github.com/papanton/bazel-broker/internal/metrics"
)

// pollInterval is how often the truncation supervisor stats the file. Bazel
// truncates <worktree>/.bazel-broker/bep.json IN PLACE on every rebuild (same
// inode, size shrinks); macOS kqueue truncation events are unreliable, so we poll.
const pollInterval = 250 * time.Millisecond

// streamConfig is the static wiring for one tailed BEP file.
type streamConfig struct {
	path       string
	worktree   string
	log        *slog.Logger
	reg        Registry
	sink       MetricsSink
	alert      metrics.AlertConfig
	profileURL func(id string) string
	onFinalize func(r *metrics.Row)
}

// profilePath derives the E1 per-worktree profile path from the worktree root.
func profilePathFor(worktree string) string {
	if worktree == "" {
		return ""
	}
	return filepath.Join(worktree, ".bazel-broker", "command.profile.gz")
}

// watchFile tails path with truncation-aware re-open until ctx is cancelled. It
// is the MANDATORY supervisor (C7/R2): every rebuild truncates the file in place,
// which nxadm/tail does not reliably follow past EOF, so we additionally stat the
// file and tear down + restart the tailer from offset 0 on (a) inode change
// (rotation) or (b) size < lastReadOffset (in-place truncation). On restart the
// prior build's row is finalized first (if it reached BuildFinished/last_message)
// and per-stream parse state is reset so counters never bleed across rebuilds.
func watchFile(ctx context.Context, cfg streamConfig) {
	s := &stream{
		path:       cfg.path,
		wt:         cfg.worktree,
		log:        cfg.log,
		reg:        cfg.reg,
		sink:       cfg.sink,
		alert:      cfg.alert,
		profileURL: cfg.profileURL,
		onFinalize: cfg.onFinalize,
	}
	s.resetState()

	for ctx.Err() == nil {
		// (Re)start a tailer from the current start (offset 0 on a fresh/truncated
		// file). The supervisor cancels restartCtx to force a clean restart.
		restartCtx, restart := context.WithCancel(ctx)
		runOneTail(restartCtx, s, restart)
		restart()
		if ctx.Err() != nil {
			break
		}
		// A restart was triggered (truncation/rotation): finalize whatever the prior
		// build reached, then reset for the next build reusing the same path.
		s.finalize()
		s.resetState()
		s.log.Debug("bep stream restarted (truncation/rotation)", "path", s.path)
	}
	// Context cancelled (Stop / shutdown): finalize a build that ended without a
	// last_message (e.g. crash) so its row is not lost.
	s.finalize()
}

// runOneTail runs a single nxadm/tail session plus the stat supervisor. It
// returns when the supervisor detects truncation/rotation (it calls restart) or
// ctx is done.
func runOneTail(ctx context.Context, s *stream, restart context.CancelFunc) {
	t, err := tail.TailFile(s.path, tail.Config{
		Follow:    true,
		ReOpen:    true, // rotation (rename+recreate); supervisor below covers in-place truncation
		MustExist: false,
		Poll:      true, // macOS: kqueue truncation signals are flaky → poll
		Logger:    tail.DiscardingLogger,
		Location:  &tail.SeekInfo{Offset: 0, Whence: io.SeekStart},
	})
	if err != nil {
		s.log.Error("tail open failed", "path", s.path, "err", err)
		// brief backoff so a missing dir doesn't hot-loop
		select {
		case <-ctx.Done():
		case <-time.After(pollInterval):
		}
		return
	}
	defer func() { _ = t.Stop() }()

	var (
		mu          sync.Mutex
		lastOffset  int64
		startDevIno = statKey(s.path)
	)

	// Supervisor goroutine: stat the path, force a restart on inode change or
	// in-place shrink.
	go func() {
		ticker := time.NewTicker(pollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				fi, err := os.Stat(s.path)
				if err != nil {
					continue // file may be momentarily gone during recreate
				}
				key := devIno(fi)
				mu.Lock()
				off := lastOffset
				mu.Unlock()
				if (startDevIno != (devInoKey{}) && key != startDevIno) || fi.Size() < off {
					restart()
					return
				}
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case line, ok := <-t.Lines:
			if !ok {
				return
			}
			if line.Err != nil {
				s.log.Debug("tail line err", "path", s.path, "err", line.Err)
				continue
			}
			s.consume(line.Text)
			if off, err := t.Tell(); err == nil {
				mu.Lock()
				lastOffset = off
				mu.Unlock()
			}
			// After last_message the build is finalized; we keep tailing so the next
			// rebuild's stream on the same file is caught. resetState/re-finalize is
			// driven by the supervisor restart when the file truncates.
		}
	}
}

type devInoKey struct {
	dev uint64
	ino uint64
}

func statKey(path string) devInoKey {
	fi, err := os.Stat(path)
	if err != nil {
		return devInoKey{}
	}
	return devIno(fi)
}

func devIno(fi os.FileInfo) devInoKey {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return devInoKey{dev: uint64(st.Dev), ino: uint64(st.Ino)}
	}
	return devInoKey{}
}
