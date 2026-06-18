package store

import (
	"database/sql"
	"fmt"

	"github.com/antoniospapantoniou/bazel-broker/internal/metrics"
)

// DiskReport is one row of disk_cache_reports.
type DiskReport struct {
	TakenAt     int64  `json:"taken_at"`
	CacheDir    string `json:"cache_dir"`
	TotalBytes  int64  `json:"total_bytes"`
	FileCount   int64  `json:"file_count"`
	OldestMtime int64  `json:"oldest_mtime"`
	GCFreed     int64  `json:"gc_freed_bytes"`
}

// UpsertMetrics writes a metrics.Row (and its runner-count + mnemonic children)
// in a single transaction. It is an upsert on invocation_id because metrics
// arrive incrementally (started → BuildMetrics → finished). rawJSON is the raw
// BuildMetrics protojson blob (stored in the v1 `json` column; may be empty).
//
// The metrics row FKs to builds(invocation_id); the caller must have registered
// the build first (the registry lifecycle does this before the BEP stream's
// BuildMetrics lands). A missing parent surfaces as an FK error here.
func (s *Store) UpsertMetrics(r *metrics.Row, rawJSON string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin metrics tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit

	ratio, ok := r.CacheHitRatio()
	var ratioArg any
	if ok {
		ratioArg = ratio
	}

	if _, err := tx.Exec(`
		INSERT INTO metrics (
			invocation_id, actions_total, actions_cached, cache_hit_ratio, wall_ms, json,
			worktree, started_at, finished_at, exit_code, success, actions_executed,
			disk_cache_hits, remote_cache_hits, executed_runners, processes_total,
			cpu_time_ms, analysis_ms, execution_ms,
			targets_configured, packages_loaded, peak_heap_bytes,
			tests_total, tests_failed, summary_line, profile_path, bep_path, alert
		) VALUES (?,?,?,?,?,?, ?,?,?,?,?,?, ?,?,?,?, ?,?,?, ?,?,?, ?,?,?,?,?,?)
		ON CONFLICT(invocation_id) DO UPDATE SET
			actions_total     = excluded.actions_total,
			actions_cached    = excluded.actions_cached,
			cache_hit_ratio   = excluded.cache_hit_ratio,
			wall_ms           = excluded.wall_ms,
			json              = excluded.json,
			worktree          = excluded.worktree,
			started_at        = excluded.started_at,
			finished_at       = excluded.finished_at,
			exit_code         = excluded.exit_code,
			success           = excluded.success,
			actions_executed  = excluded.actions_executed,
			disk_cache_hits   = excluded.disk_cache_hits,
			remote_cache_hits = excluded.remote_cache_hits,
			executed_runners  = excluded.executed_runners,
			processes_total   = excluded.processes_total,
			cpu_time_ms       = excluded.cpu_time_ms,
			analysis_ms       = excluded.analysis_ms,
			execution_ms      = excluded.execution_ms,
			targets_configured= excluded.targets_configured,
			packages_loaded   = excluded.packages_loaded,
			peak_heap_bytes   = excluded.peak_heap_bytes,
			tests_total       = excluded.tests_total,
			tests_failed      = excluded.tests_failed,
			summary_line      = excluded.summary_line,
			profile_path      = excluded.profile_path,
			bep_path          = excluded.bep_path,
			alert             = excluded.alert`,
		r.InvocationID, r.ActionsCreated, r.DiskCacheHits, ratioArg, r.WallMS, nullStr(rawJSON),
		nullStr(r.Worktree), nullInt(r.StartedAt), nullInt(r.FinishedAt), r.ExitCode, boolToInt(r.Success), r.ActionsExecuted,
		r.DiskCacheHits, r.RemoteCacheHits, r.ExecutedRunners, r.ProcessesTotal,
		r.CPUMS, r.AnalysisMS, r.ExecMS,
		r.TargetsConfigured, r.PackagesLoaded, r.PeakHeapBytes,
		r.TestsTotal, r.TestsFailed, nullStr(r.SummaryLine), nullStr(r.ProfilePath), nullStr(r.BEPPath), nullStr(r.Alert),
	); err != nil {
		return fmt.Errorf("upsert metrics %s: %w", r.InvocationID, err)
	}

	// Replace child rows wholesale (idempotent re-emit on each metrics update).
	if _, err := tx.Exec(`DELETE FROM metrics_runner_counts WHERE invocation_id = ?`, r.InvocationID); err != nil {
		return err
	}
	for _, rc := range r.RunnerCounts {
		if _, err := tx.Exec(
			`INSERT INTO metrics_runner_counts (invocation_id, name, exec_kind, count) VALUES (?,?,?,?)`,
			r.InvocationID, rc.Name, nullStr(rc.ExecKind), rc.Count); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`DELETE FROM action_mnemonics WHERE invocation_id = ?`, r.InvocationID); err != nil {
		return err
	}
	for _, m := range r.Mnemonics {
		if _, err := tx.Exec(
			`INSERT INTO action_mnemonics (invocation_id, mnemonic, actions_created, actions_executed, system_time_ms, user_time_ms)
			 VALUES (?,?,?,?,?,?)`,
			r.InvocationID, m.Mnemonic, m.ActionsCreated, m.ActionsExecuted, m.SystemTimeMS, m.UserTimeMS); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// GetMetrics returns the full metrics.Row for one invocation (ok=false if absent),
// re-hydrating the runner-count + mnemonic children. The raw BuildMetrics JSON
// blob is returned separately.
func (s *Store) GetMetrics(invocationID string) (*metrics.Row, string, bool, error) {
	r := &metrics.Row{InvocationID: invocationID}
	var (
		ratio                             sql.NullFloat64
		worktree, summary, profile, bepP  sql.NullString
		alert, rawJSON                    sql.NullString
		startedAt, finishedAt, successInt sql.NullInt64
	)
	err := s.db.QueryRow(`
		SELECT actions_total, actions_executed, disk_cache_hits, remote_cache_hits, executed_runners, processes_total,
		       cache_hit_ratio, wall_ms, cpu_time_ms, analysis_ms, execution_ms,
		       targets_configured, packages_loaded, peak_heap_bytes,
		       tests_total, tests_failed, worktree, started_at, finished_at, exit_code, success,
		       summary_line, profile_path, bep_path, alert, json
		  FROM metrics WHERE invocation_id = ?`, invocationID).Scan(
		&r.ActionsCreated, &r.ActionsExecuted, &r.DiskCacheHits, &r.RemoteCacheHits, &r.ExecutedRunners, &r.ProcessesTotal,
		&ratio, &r.WallMS, &r.CPUMS, &r.AnalysisMS, &r.ExecMS,
		&r.TargetsConfigured, &r.PackagesLoaded, &r.PeakHeapBytes,
		&r.TestsTotal, &r.TestsFailed, &worktree, &startedAt, &finishedAt, &r.ExitCode, &successInt,
		&summary, &profile, &bepP, &alert, &rawJSON)
	if err == sql.ErrNoRows {
		return nil, "", false, nil
	}
	if err != nil {
		return nil, "", false, fmt.Errorf("get metrics %s: %w", invocationID, err)
	}
	r.HasMetrics = true
	r.Worktree = worktree.String
	r.StartedAt = startedAt.Int64
	r.FinishedAt = finishedAt.Int64
	r.Success = successInt.Int64 == 1
	r.HasFinish = finishedAt.Valid && finishedAt.Int64 != 0
	r.SummaryLine = summary.String
	r.ProfilePath = profile.String
	r.BEPPath = bepP.String
	r.Alert = alert.String

	rcRows, err := s.db.Query(
		`SELECT name, exec_kind, count FROM metrics_runner_counts WHERE invocation_id = ? ORDER BY name`, invocationID)
	if err != nil {
		return nil, "", false, err
	}
	defer rcRows.Close()
	for rcRows.Next() {
		var rc metrics.RunnerCount
		var ek sql.NullString
		if err := rcRows.Scan(&rc.Name, &ek, &rc.Count); err != nil {
			return nil, "", false, err
		}
		rc.ExecKind = ek.String
		r.RunnerCounts = append(r.RunnerCounts, rc)
	}
	if err := rcRows.Err(); err != nil {
		return nil, "", false, err
	}

	mRows, err := s.db.Query(
		`SELECT mnemonic, actions_created, actions_executed, system_time_ms, user_time_ms
		   FROM action_mnemonics WHERE invocation_id = ? ORDER BY mnemonic`, invocationID)
	if err != nil {
		return nil, "", false, err
	}
	defer mRows.Close()
	for mRows.Next() {
		var m metrics.Mnemonic
		if err := mRows.Scan(&m.Mnemonic, &m.ActionsCreated, &m.ActionsExecuted, &m.SystemTimeMS, &m.UserTimeMS); err != nil {
			return nil, "", false, err
		}
		r.Mnemonics = append(r.Mnemonics, m)
	}
	return r, rawJSON.String, true, mRows.Err()
}

// ListMetrics returns up to limit metrics rows, newest finished_at first (then
// started_at), for the dashboard list form. Children are not hydrated (summary view).
func (s *Store) ListMetrics(limit int) ([]*metrics.Row, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.Query(`
		SELECT invocation_id, actions_total, actions_executed, disk_cache_hits, remote_cache_hits,
		       executed_runners, processes_total, wall_ms, worktree, started_at, finished_at, exit_code, success,
		       summary_line, alert
		  FROM metrics ORDER BY COALESCE(finished_at, started_at, 0) DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list metrics: %w", err)
	}
	defer rows.Close()
	var out []*metrics.Row
	for rows.Next() {
		r := &metrics.Row{HasMetrics: true}
		var worktree, summary, alert sql.NullString
		var startedAt, finishedAt, successInt sql.NullInt64
		if err := rows.Scan(&r.InvocationID, &r.ActionsCreated, &r.ActionsExecuted, &r.DiskCacheHits,
			&r.RemoteCacheHits, &r.ExecutedRunners, &r.ProcessesTotal, &r.WallMS, &worktree, &startedAt, &finishedAt,
			&r.ExitCode, &successInt, &summary, &alert); err != nil {
			return nil, err
		}
		r.Worktree = worktree.String
		r.StartedAt = startedAt.Int64
		r.FinishedAt = finishedAt.Int64
		r.Success = successInt.Int64 == 1
		r.HasFinish = finishedAt.Valid && finishedAt.Int64 != 0
		r.SummaryLine = summary.String
		r.Alert = alert.String
		out = append(out, r)
	}
	return out, rows.Err()
}

// RecentRatiosForWorktree returns up to n recent cache_hit_ratio values for a
// worktree (newest first), excluding NULL ratios — the trailing baseline for the
// low-hit alert. The current build is excluded by passing its id.
func (s *Store) RecentRatiosForWorktree(worktree, excludeID string, n int) ([]float64, error) {
	if n <= 0 {
		n = 10
	}
	rows, err := s.db.Query(`
		SELECT cache_hit_ratio FROM metrics
		 WHERE worktree = ? AND invocation_id != ? AND cache_hit_ratio IS NOT NULL
		 ORDER BY COALESCE(finished_at, started_at, 0) DESC LIMIT ?`, worktree, excludeID, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []float64
	for rows.Next() {
		var v float64
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// InsertDiskReport records one disk-cache size/GC report row.
func (s *Store) InsertDiskReport(r DiskReport) error {
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO disk_cache_reports (taken_at, cache_dir, total_bytes, file_count, oldest_mtime, gc_freed_bytes)
		 VALUES (?,?,?,?,?,?)`,
		r.TakenAt, r.CacheDir, r.TotalBytes, r.FileCount, r.OldestMtime, r.GCFreed)
	return err
}

// LatestDiskReport returns the most recent disk-cache report (ok=false if none).
func (s *Store) LatestDiskReport() (DiskReport, bool, error) {
	var r DiskReport
	var oldest sql.NullInt64
	err := s.db.QueryRow(
		`SELECT taken_at, cache_dir, total_bytes, file_count, oldest_mtime, gc_freed_bytes
		   FROM disk_cache_reports ORDER BY taken_at DESC LIMIT 1`).Scan(
		&r.TakenAt, &r.CacheDir, &r.TotalBytes, &r.FileCount, &oldest, &r.GCFreed)
	if err == sql.ErrNoRows {
		return DiskReport{}, false, nil
	}
	if err != nil {
		return DiskReport{}, false, err
	}
	r.OldestMtime = oldest.Int64
	return r, true, nil
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullInt(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
