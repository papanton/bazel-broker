package metrics

import "sort"

// Alert strings stored on a metrics row and surfaced via /metrics + the WS feed.
const (
	AlertLowCacheHit  = "low_cache_hit"           // ratio below the absolute floor
	AlertCacheBusting = "cache_busting_suspected" // sudden drop vs this worktree's own history
)

// AlertConfig parameterizes the low-hit alert. Zero value uses the defaults via
// AlertConfig.withDefaults.
type AlertConfig struct {
	Floor    float64 // absolute low-hit threshold (default 0.50)
	Delta    float64 // drop-vs-baseline threshold (default 0.20)
	MinProcs int64   // ignore tiny/no-op builds below this (default 200)
}

func (c AlertConfig) withDefaults() AlertConfig {
	if c.Floor == 0 {
		c.Floor = 0.50
	}
	if c.Delta == 0 {
		c.Delta = 0.20
	}
	if c.MinProcs == 0 {
		c.MinProcs = 200
	}
	return c
}

// EvaluateAlert returns the alert string for ratio given a trailing history of
// prior ratios for the SAME worktree (most-recent-first or any order; only the
// median matters). processesTotal gates out tiny builds. Returns "" for no alert.
//
// A sudden drop relative to this worktree's own recent median is the fingerprint
// of path/env leakage busting the shared --disk_cache; it takes priority over the
// absolute floor.
func EvaluateAlert(ratio float64, processesTotal int64, history []float64, cfg AlertConfig) string {
	cfg = cfg.withDefaults()
	if processesTotal < cfg.MinProcs {
		return ""
	}
	if b, ok := median(history); ok && ratio < b-cfg.Delta {
		return AlertCacheBusting
	}
	if ratio < cfg.Floor {
		return AlertLowCacheHit
	}
	return ""
}

func median(xs []float64) (float64, bool) {
	if len(xs) == 0 {
		return 0, false
	}
	cp := append([]float64(nil), xs...)
	sort.Float64s(cp)
	n := len(cp)
	if n%2 == 1 {
		return cp[n/2], true
	}
	return (cp[n/2-1] + cp[n/2]) / 2, true
}
