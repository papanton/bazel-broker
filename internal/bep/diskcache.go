package bep

import (
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// DiskCacheConfig parameterizes the shared --disk_cache size report + optional GC.
type DiskCacheConfig struct {
	Dir          string        // shared --disk_cache directory (broker config.disk_cache)
	ReportEvery  time.Duration // periodic report interval (default 15m)
	MaxBytes     int64         // direct-GC target; 0 = report-only (recommended interim, D-E4-4)
	MinAge       time.Duration // never delete entries newer than this (default 24h)
	Hysteresis   float64       // prune down to MaxBytes*Hysteresis (default 0.9)
	EnableDirect bool          // opt-in direct LRU prune (default OFF; prefer Bazel-native GC)
}

func (c DiskCacheConfig) withDefaults() DiskCacheConfig {
	if c.ReportEvery == 0 {
		c.ReportEvery = 15 * time.Minute
	}
	if c.MinAge == 0 {
		c.MinAge = 24 * time.Hour
	}
	if c.Hysteresis == 0 {
		c.Hysteresis = 0.9
	}
	return c
}

// cacheEntry is a cache file's path/size/mtime for the size report + LRU prune.
type cacheEntry struct {
	path  string
	size  int64
	mtime time.Time
}

// scanDiskCache walks dir (stat-only) and returns total bytes, file count, and the
// oldest mtime. When collect is true it also returns the per-file list for the LRU
// prune; report-only scans (the default) pass collect=false to skip that
// allocation. Symlinks/dirs are skipped.
func scanDiskCache(dir string, collect bool) (total int64, count int64, oldest time.Time, entries []cacheEntry, err error) {
	if dir == "" {
		return 0, 0, time.Time{}, nil, nil
	}
	err = filepath.WalkDir(dir, func(path string, d os.DirEntry, werr error) error {
		if werr != nil {
			return nil // tolerate transient races during a concurrent build
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		fi, ferr := d.Info()
		if ferr != nil {
			return nil
		}
		total += fi.Size()
		count++
		mt := fi.ModTime()
		if oldest.IsZero() || mt.Before(oldest) {
			oldest = mt
		}
		if collect {
			entries = append(entries, cacheEntry{path: path, size: fi.Size(), mtime: mt})
		}
		return nil
	})
	return total, count, oldest, entries, err
}

// reporter produces a fresh disk-cache report, optionally running an opt-in LRU
// prune (off by default; D-E4-4 recommends report-only until Bazel-native GC
// covers all pinned versions). buildActive reports whether any build currently
// targets the cache → GC is skipped while one is in flight (R4 safety).
type reporter struct {
	cfg         DiskCacheConfig
	log         *slog.Logger
	buildActive func() bool
	now         func() time.Time
}

func newReporter(cfg DiskCacheConfig, buildActive func() bool, log *slog.Logger) *reporter {
	if log == nil {
		log = slog.Default()
	}
	if buildActive == nil {
		buildActive = func() bool { return false }
	}
	return &reporter{cfg: cfg.withDefaults(), log: log, buildActive: buildActive, now: time.Now}
}

// run produces a report and (if enabled + safe) prunes. Returns the report view.
func (rp *reporter) run() (DiskReportView, error) {
	// Only collect the per-file entry list when a prune could actually run; a
	// report-only scan (the default) skips that allocation.
	collect := rp.cfg.EnableDirect && rp.cfg.MaxBytes > 0
	total, count, oldest, entries, err := scanDiskCache(rp.cfg.Dir, collect)
	if err != nil {
		return DiskReportView{}, err
	}
	view := DiskReportView{
		TakenAt:    rp.now().UnixMilli(),
		CacheDir:   rp.cfg.Dir,
		TotalBytes: total,
		FileCount:  count,
	}
	if !oldest.IsZero() {
		view.OldestMtime = oldest.UnixMilli()
	}

	if rp.cfg.EnableDirect && rp.cfg.MaxBytes > 0 && total > rp.cfg.MaxBytes {
		if rp.buildActive() {
			rp.log.Info("disk-cache GC skipped: build in progress", "dir", rp.cfg.Dir)
		} else {
			freed := rp.prune(entries, total)
			view.GCFreed = freed
			view.TotalBytes = total - freed
		}
	}
	return view, nil
}

// prune deletes oldest-by-mtime entries until total drops to MaxBytes*Hysteresis,
// never touching entries younger than MinAge (R4: nothing freshly written). Uses
// mtime (not atime — unreliable on macOS). Returns bytes freed.
func (rp *reporter) prune(entries []cacheEntry, total int64) int64 {
	target := int64(float64(rp.cfg.MaxBytes) * rp.cfg.Hysteresis)
	sort.Slice(entries, func(i, j int) bool { return entries[i].mtime.Before(entries[j].mtime) })
	cutoff := rp.now().Add(-rp.cfg.MinAge)
	var freed int64
	for _, e := range entries {
		if total-freed <= target {
			break
		}
		if e.mtime.After(cutoff) {
			continue // too fresh to evict
		}
		if err := os.Remove(e.path); err != nil {
			continue
		}
		freed += e.size
	}
	if freed > 0 {
		rp.log.Info("disk-cache GC pruned", "dir", rp.cfg.Dir, "freed_bytes", freed)
	}
	return freed
}
