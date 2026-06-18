package bep

import (
	"encoding/json"
	"net/http"

	"github.com/papanton/bazel-broker/internal/api"
	"github.com/papanton/bazel-broker/internal/metrics"
)

// metricsJSON is the GET /metrics response shape (§2.6). Raw inputs are emitted;
// the cache hit ratio is derived at read time from the binding runner_count[]
// definition (disk_hits / (disk_hits + executed)).
type metricsJSON struct {
	InvocationID string `json:"invocation_id"`
	Worktree     string `json:"worktree,omitempty"`
	StartedAt    int64  `json:"started_at,omitempty"`
	FinishedAt   int64  `json:"finished_at,omitempty"`
	ExitCode     int    `json:"exit_code"`
	Success      bool   `json:"success"`

	Cache   cacheJSON   `json:"cache"`
	Timing  timingJSON  `json:"timing"`
	Targets int64       `json:"targets_configured"`
	Pkgs    int64       `json:"packages_loaded"`
	Heap    int64       `json:"peak_heap_bytes,omitempty"`
	Tests   testsJSON   `json:"tests"`
	Alert   string      `json:"alert,omitempty"`
	Profile profileJSON `json:"profile"`
}

type cacheJSON struct {
	ActionsCreated  int64                 `json:"actions_created"`
	ActionsExecuted int64                 `json:"actions_executed"`
	ProcessesTotal  int64                 `json:"processes_total"`
	RunnerCounts    []metrics.RunnerCount `json:"runner_counts"`
	DiskCacheHits   int64                 `json:"disk_cache_hits"`
	RemoteCacheHits int64                 `json:"remote_cache_hits"`
	ExecutedRunners int64                 `json:"executed_runners"`
	HitRatio        *float64              `json:"hit_ratio"`
	SummaryLine     string                `json:"summary_line,omitempty"`
	ByMnemonic      []metrics.Mnemonic    `json:"by_mnemonic,omitempty"`
}

type timingJSON struct {
	WallMS     int64 `json:"wall_time_ms"`
	CPUMS      int64 `json:"cpu_time_ms"`
	AnalysisMS int64 `json:"analysis_ms"`
	ExecMS     int64 `json:"execution_ms"`
}

type testsJSON struct {
	Total  int64 `json:"total"`
	Failed int64 `json:"failed"`
}

type profileJSON struct {
	Path        string `json:"path,omitempty"`
	ServedURL   string `json:"served_url,omitempty"`
	PerfettoURL string `json:"perfetto_url,omitempty"`
}

func (p *Provider) toJSON(r *metrics.Row) metricsJSON {
	out := metricsJSON{
		InvocationID: r.InvocationID,
		Worktree:     r.Worktree,
		StartedAt:    r.StartedAt,
		FinishedAt:   r.FinishedAt,
		ExitCode:     r.ExitCode,
		Success:      r.Success,
		Cache: cacheJSON{
			ActionsCreated:  r.ActionsCreated,
			ActionsExecuted: r.ActionsExecuted,
			ProcessesTotal:  r.ProcessesTotal,
			RunnerCounts:    r.RunnerCounts,
			DiskCacheHits:   r.DiskCacheHits,
			RemoteCacheHits: r.RemoteCacheHits,
			ExecutedRunners: r.ExecutedRunners,
			SummaryLine:     r.SummaryLine,
			ByMnemonic:      r.Mnemonics,
		},
		Timing: timingJSON{
			WallMS: r.WallMS, CPUMS: r.CPUMS, AnalysisMS: r.AnalysisMS, ExecMS: r.ExecMS,
		},
		Targets: r.TargetsConfigured,
		Pkgs:    r.PackagesLoaded,
		Heap:    r.PeakHeapBytes,
		Tests:   testsJSON{Total: r.TestsTotal, Failed: r.TestsFailed},
		Alert:   r.Alert,
	}
	if ratio, ok := r.CacheHitRatio(); ok {
		out.Cache.HitRatio = &ratio
	}
	if out.Cache.RunnerCounts == nil {
		out.Cache.RunnerCounts = []metrics.RunnerCount{}
	}
	if r.InvocationID != "" {
		out.Profile = profileJSON{
			Path:        r.ProfilePath,
			ServedURL:   p.baseURL + "/profile/" + r.InvocationID + "/command.profile.gz",
			PerfettoURL: p.ProfileURLFor(r.InvocationID),
		}
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (p *Provider) fail(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, api.ErrorResponse{Error: code, Message: msg, Epic: "E4"})
}
