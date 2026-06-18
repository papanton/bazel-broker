// Package metrics holds the pure (I/O-free) metrics model derived from a Bazel
// build's BuildMetrics event, plus the cache-hit derivation and low-hit alert
// logic. It depends only on the generated BEP types, so it is trivially unit
// testable against captured real streams.
//
// CACHE-HIT DEFINITION (binding — consolidated-review C8 / E1 cache-config/measure.sh,
// byte-for-byte identical):
//
//	disk_hits = Σ runner_count[name == "disk cache hit"].count
//	executed  = Σ runner_count[name].count  over names EXCLUDING "total",
//	            "internal", AND "disk cache hit"
//	cache_hit_ratio = disk_hits / (disk_hits + executed)   (nil if denominator == 0)
//
// `actions_created − actions_executed` is WRONG (a disk-cache hit is counted INSIDE
// actions_executed); we never use it.
package metrics

import (
	"strings"

	bes "github.com/papanton/bazel-broker/internal/genproto/buildeventstream"
)

// Runner-count name literals Bazel emits in ActionSummary.runner_count[] (the
// structured form of the "N processes: X disk cache hit, …" summary line).
const (
	RunnerTotal       = "total"
	RunnerInternal    = "internal"
	RunnerDiskCache   = "disk cache hit"
	RunnerRemoteCache = "remote cache hit"
)

// RunnerCount is one row of ActionSummary.runner_count[].
type RunnerCount struct {
	Name     string `json:"name"`
	ExecKind string `json:"exec_kind,omitempty"`
	Count    int64  `json:"count"`
}

// Mnemonic is one per-mnemonic action breakdown row (ActionSummary.action_data[]).
type Mnemonic struct {
	Mnemonic        string `json:"mnemonic"`
	ActionsCreated  int64  `json:"actions_created"`
	ActionsExecuted int64  `json:"actions_executed"`
	SystemTimeMS    int64  `json:"system_time_ms,omitempty"`
	UserTimeMS      int64  `json:"user_time_ms,omitempty"`
}

// Row is the fully-extracted, persistable metrics for one invocation. Fields are
// populated incrementally as BEP events arrive (Started → BuildMetrics → Finished);
// the zero value is a valid "nothing seen yet" row.
type Row struct {
	InvocationID string
	Worktree     string
	BEPPath      string
	ProfilePath  string

	StartedAt  int64 // unix ms (BuildStarted.start_time)
	FinishedAt int64 // unix ms (BuildFinished.finish_time)
	ExitCode   int
	Success    bool
	HasFinish  bool

	// Cache (from ActionSummary).
	ActionsCreated  int64 // actions_created (E2 col: actions_total)
	ActionsExecuted int64 // actions_executed (raw; INCLUDES disk/remote cache hits)
	DiskCacheHits   int64 // runner_count "disk cache hit"
	RemoteCacheHits int64 // runner_count "remote cache hit"
	ExecutedRunners int64 // Σ runner_count excluding total/internal/disk-cache-hit
	ProcessesTotal  int64 // runner_count "total" (or Σ if absent)
	RunnerCounts    []RunnerCount
	Mnemonics       []Mnemonic
	SummaryLine     string // the raw "N processes: …" ground-truth string

	// Timing (TimingMetrics).
	WallMS     int64
	CPUMS      int64
	AnalysisMS int64
	ExecMS     int64

	TargetsConfigured int64
	PackagesLoaded    int64
	PeakHeapBytes     int64

	TestsTotal  int64
	TestsFailed int64

	Alert string // "" | AlertLowCacheHit | AlertCacheBusting

	HasMetrics bool // a BuildMetrics event was seen
}

// CacheHitRatio returns the binding disk-cache hit ratio in [0,1], or (0,false)
// when there are no cacheable actions (denominator 0) so the caller stores NULL.
func (r *Row) CacheHitRatio() (float64, bool) {
	denom := r.DiskCacheHits + r.ExecutedRunners
	if denom <= 0 {
		return 0, false
	}
	return float64(r.DiskCacheHits) / float64(denom), true
}

// ExtractMetrics fills the BuildMetrics-derived fields of r from a decoded
// *bes.BuildMetrics. It is idempotent and never panics on absent sub-messages.
func (r *Row) ExtractMetrics(bm *bes.BuildMetrics) {
	if bm == nil {
		return
	}
	r.HasMetrics = true

	if as := bm.GetActionSummary(); as != nil {
		r.ActionsCreated = as.GetActionsCreated()
		r.ActionsExecuted = as.GetActionsExecuted()

		r.RunnerCounts = r.RunnerCounts[:0]
		var diskHits, remoteHits, executed, total, sumAll int64
		for _, rc := range as.GetRunnerCount() {
			name := rc.GetName()
			cnt := int64(rc.GetCount())
			r.RunnerCounts = append(r.RunnerCounts, RunnerCount{
				Name:     name,
				ExecKind: rc.GetExecKind(),
				Count:    cnt,
			})
			switch name {
			case RunnerTotal:
				total = cnt
			case RunnerInternal:
				// excluded from executed and from sumAll-fallback's "real work"
			case RunnerDiskCache:
				diskHits += cnt
			case RunnerRemoteCache:
				remoteHits += cnt
				executed += cnt // remote-cache hits are "executed" runners per the binding formula
			default:
				executed += cnt
			}
			if name != RunnerTotal {
				sumAll += cnt
			}
		}
		r.DiskCacheHits = diskHits
		r.RemoteCacheHits = remoteHits
		r.ExecutedRunners = executed
		if total > 0 {
			r.ProcessesTotal = total
		} else {
			r.ProcessesTotal = sumAll
		}

		r.Mnemonics = r.Mnemonics[:0]
		for _, ad := range as.GetActionData() {
			r.Mnemonics = append(r.Mnemonics, Mnemonic{
				Mnemonic:        ad.GetMnemonic(),
				ActionsCreated:  ad.GetActionsCreated(),
				ActionsExecuted: ad.GetActionsExecuted(),
				SystemTimeMS:    ad.GetSystemTime().AsDuration().Milliseconds(),
				UserTimeMS:      ad.GetUserTime().AsDuration().Milliseconds(),
			})
		}
	}

	if tm := bm.GetTimingMetrics(); tm != nil {
		r.WallMS = tm.GetWallTimeInMs()
		r.CPUMS = tm.GetCpuTimeInMs()
		r.AnalysisMS = tm.GetAnalysisPhaseTimeInMs()
		r.ExecMS = tm.GetExecutionPhaseTimeInMs()
	}
	if tgt := bm.GetTargetMetrics(); tgt != nil {
		r.TargetsConfigured = tgt.GetTargetsConfigured()
	}
	if pm := bm.GetPackageMetrics(); pm != nil {
		r.PackagesLoaded = pm.GetPackagesLoaded()
	}
	if mm := bm.GetMemoryMetrics(); mm != nil {
		r.PeakHeapBytes = mm.GetPeakPostGcHeapSize()
	}
}

// ParseSummaryDiskHits extracts the disk-cache-hit count from Bazel's human
// "N processes: X disk cache hit, …" summary line, for the equality cross-check
// against the runner_count derivation. Returns (0,false) if the line has no
// "disk cache hit" clause.
func ParseSummaryDiskHits(line string) (int64, bool) {
	idx := strings.Index(line, "disk cache hit")
	if idx < 0 {
		return 0, false
	}
	// Walk back over the digits immediately preceding " disk cache hit".
	prefix := strings.TrimRight(line[:idx], " ")
	j := len(prefix)
	for j > 0 && prefix[j-1] >= '0' && prefix[j-1] <= '9' {
		j--
	}
	digits := prefix[j:]
	if digits == "" {
		return 0, false
	}
	var n int64
	for _, c := range digits {
		n = n*10 + int64(c-'0')
	}
	return n, true
}
