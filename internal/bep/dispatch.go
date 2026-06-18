// Package bep tails Bazel's per-worktree Build Event Protocol JSON stream
// (<worktree>/.bazel-broker/bep.json — C7, reused & truncated across rebuilds),
// protojson-decodes each NDJSON line, derives metrics (cache-hit ratio via
// ActionSummary.runner_count[] — C8), persists them to SQLite, enriches the
// registry build, and serves the /metrics + Perfetto routes (MetricsProvider).
//
// The registry join key is BuildStarted.uuid (NOT the filename — the filename
// carries only the worktree). A mandatory truncation supervisor (supervisor.go)
// re-opens the file on every rebuild because Bazel truncates it in place.
package bep

import (
	"log/slog"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/antoniospapantoniou/bazel-broker/internal/build"
	bes "github.com/antoniospapantoniou/bazel-broker/internal/genproto/buildeventstream"
	"github.com/antoniospapantoniou/bazel-broker/internal/metrics"
)

// Registry is the subset of *registry.Registry the BEP ingest needs. Kept as an
// interface so the dispatcher unit-tests against a fake and bep does not import
// the concrete registry.
type Registry interface {
	// Upsert merges a build (used to enrich cache_hit_ratio / profile_url and to
	// stamp a stub build row before metrics persist, satisfying the metrics FK).
	Upsert(b *build.Build) (*build.Build, error)
	// FindByInvocationID returns the live build for an invocation id.
	FindByInvocationID(id string) (*build.Build, bool)
}

// MetricsSink persists a finalized metrics row + reads trailing history for the
// low-hit alert baseline. Satisfied by *store.Store.
type MetricsSink interface {
	UpsertMetrics(r *metrics.Row, rawJSON string) error
	RecentRatiosForWorktree(worktree, excludeID string, n int) ([]float64, error)
}

// unmarshal tolerates fields from a newer Bazel (forward-compat, R1).
var unmarshal = protojson.UnmarshalOptions{DiscardUnknown: true}

// stream is the per-tailed-file decode + accumulation state. One stream object
// is reset (resetState) on every truncation/restart so a fast rebuild reusing the
// same file never bleeds the prior build's counters into the next.
type stream struct {
	path  string
	wt    string // worktree (resolved from the path)
	log   *slog.Logger
	reg   Registry
	sink  MetricsSink
	alert metrics.AlertConfig

	// per-build accumulation
	row        *metrics.Row
	rawMetrics string
	bound      bool // BuildStarted.uuid seen → row.InvocationID set + stub registered
	finalized  bool
	profileURL func(id string) string
	parseErrs  int

	onFinalize func(r *metrics.Row) // optional hook (alert WS broadcast); may be nil
}

// resetState clears per-build accumulation for a fresh stream (post-truncation).
// The profile path defaults to the registry worktree's E1 convention; BuildStarted
// re-derives it from the stream's own workingDirectory when that arrives.
func (s *stream) resetState() {
	s.row = &metrics.Row{BEPPath: s.path, Worktree: s.wt, ProfilePath: profilePathFor(s.wt)}
	s.rawMetrics = ""
	s.bound = false
	s.finalized = false
}

// consume decodes one NDJSON line and folds it into the current build's state. A
// single bad line is logged + skipped (never crashes the tailer).
func (s *stream) consume(text string) {
	raw := strings.TrimSpace(text)
	if raw == "" {
		return
	}
	var ev bes.BuildEvent
	if err := unmarshal.Unmarshal([]byte(raw), &ev); err != nil {
		s.parseErrs++
		s.log.Debug("bep parse skip", "path", s.path, "err", err)
		return
	}
	s.dispatch(&ev)
}

// dispatch type-switches on the BEP payload and updates the row + registry.
func (s *stream) dispatch(ev *bes.BuildEvent) {
	if s.row == nil {
		s.resetState()
	}

	switch p := ev.GetPayload().(type) {
	case *bes.BuildEvent_Started:
		st := p.Started
		s.row.InvocationID = st.GetUuid()
		if ts := st.GetStartTime(); ts != nil {
			s.row.StartedAt = ts.AsTime().UnixMilli()
		} else if ms := st.GetStartTimeMillis(); ms != 0 {
			s.row.StartedAt = ms
		}
		if wd := st.GetWorkingDirectory(); wd != "" {
			// The BEP's workingDirectory is the authoritative build cwd where Bazel
			// wrote --profile, so re-derive the profile path from it (it may differ
			// from the registry worktree, e.g. /var vs /private/var on macOS).
			s.row.Worktree = wd
			s.row.ProfilePath = profilePathFor(wd)
		}
		s.bindBuild()

	case *bes.BuildEvent_TestResult:
		s.row.TestsTotal++
		if st := p.TestResult.GetStatus(); st != bes.TestStatus_PASSED && st != bes.TestStatus_FLAKY {
			s.row.TestsFailed++
		}

	case *bes.BuildEvent_BuildMetrics:
		s.row.ExtractMetrics(p.BuildMetrics)
		if b, err := protojson.Marshal(p.BuildMetrics); err == nil {
			s.rawMetrics = string(b)
		}

	case *bes.BuildEvent_Finished:
		fin := p.Finished
		s.row.HasFinish = true
		s.row.ExitCode = int(fin.GetExitCode().GetCode())
		s.row.Success = s.row.ExitCode == 0
		if ts := fin.GetFinishTime(); ts != nil {
			s.row.FinishedAt = ts.AsTime().UnixMilli()
		} else if ms := fin.GetFinishTimeMillis(); ms != 0 {
			s.row.FinishedAt = ms
		}
	}

	// last_message is BEP's authoritative end-of-stream marker.
	if ev.GetLastMessage() {
		s.finalize()
	}
}

// bindBuild stamps a stub build row (so the metrics FK is satisfiable) once the
// invocation id is known from BuildStarted.uuid.
func (s *stream) bindBuild() {
	if s.bound || s.row.InvocationID == "" {
		return
	}
	s.bound = true
	if s.reg != nil {
		// Register/enrich a stub so the metrics row's FK to builds resolves even
		// if this stream was discovered passively before any /register.
		if _, ok := s.reg.FindByInvocationID(s.row.InvocationID); !ok {
			_, _ = s.reg.Upsert(&build.Build{
				InvocationID: s.row.InvocationID,
				Worktree:     s.row.Worktree,
				State:        build.StateRunning,
				Source:       build.SourceDiscovered,
				StartTime:    time.Now().UTC(),
			})
		}
	}
}

// finalize persists the metrics row, computes the low-hit alert, and enriches the
// registry build with cache_hit_ratio + profile_url. Idempotent per build.
func (s *stream) finalize() {
	if s.finalized || s.row == nil || s.row.InvocationID == "" {
		return
	}
	s.finalized = true

	// Low-hit alert from the trailing baseline for this worktree.
	if ratio, ok := s.row.CacheHitRatio(); ok && s.sink != nil {
		hist, err := s.sink.RecentRatiosForWorktree(s.row.Worktree, s.row.InvocationID, 10)
		if err != nil {
			s.log.Debug("alert history lookup failed", "err", err)
		}
		s.row.Alert = metrics.EvaluateAlert(ratio, s.row.ProcessesTotal, hist, s.alert)
	}

	if s.sink != nil {
		if err := s.sink.UpsertMetrics(s.row, s.rawMetrics); err != nil {
			s.log.Error("persist metrics failed", "invocation_id", s.row.InvocationID, "err", err)
		}
	}

	// Enrich the registry build (broadcasts a `build` WS upsert automatically).
	if s.reg != nil {
		enrich := &build.Build{InvocationID: s.row.InvocationID}
		// The BEP stream has ended (last_message): the build is terminal. Flip it
		// out of `running` so the elapsed timer stops and the row reads
		// finished/failed (otherwise a completed build ticks "running" forever).
		if s.row.HasFinish && !s.row.Success {
			enrich.State = build.StateFailed
		} else {
			enrich.State = build.StateFinished
		}
		enrich.ExitCode = s.row.ExitCode
		if s.row.FinishedAt != 0 {
			enrich.EndTime = time.UnixMilli(s.row.FinishedAt).UTC()
		} else {
			enrich.EndTime = time.Now().UTC()
		}
		if s.profileURL != nil {
			enrich.ProfileURL = s.profileURL(s.row.InvocationID)
		}
		if ratio, ok := s.row.CacheHitRatio(); ok {
			enrich.CacheHitRatio = &ratio
		}
		if _, err := s.reg.Upsert(enrich); err != nil {
			s.log.Error("registry enrich failed", "invocation_id", s.row.InvocationID, "err", err)
		}
	}

	if s.row.Alert != "" {
		s.log.Warn("cache alert", "invocation_id", s.row.InvocationID,
			"worktree", s.row.Worktree, "alert", s.row.Alert,
			"disk_cache_hits", s.row.DiskCacheHits, "processes_total", s.row.ProcessesTotal)
	}
	if s.onFinalize != nil {
		s.onFinalize(s.row)
	}
	s.log.Info("metrics finalized", "invocation_id", s.row.InvocationID,
		"disk_cache_hits", s.row.DiskCacheHits, "executed_runners", s.row.ExecutedRunners,
		"processes_total", s.row.ProcessesTotal)
}
