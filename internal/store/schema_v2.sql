-- schema v2 (E4) — extends E2's reserved `metrics` table IN PLACE (no parallel
-- table) and adds the structured runner-count / per-mnemonic / disk-cache-report
-- tables. Idempotent: ADD COLUMN is guarded in Go (errors ignored if present),
-- and every CREATE uses IF NOT EXISTS. E2 stamped user_version=1; this migrates
-- to 2 to POPULATE, not restructure.

-- E2-declared metrics columns reused as canonical:
--   actions_total   <- ActionSummary.actions_created
--   actions_cached  <- runner_count "disk cache hit" count
--   cache_hit_ratio <- disk_hits/(disk_hits+executed)  (NULL if denom 0)
--   wall_ms         <- TimingMetrics.wall_time_in_ms
--   json            <- raw BuildMetrics protojson blob

CREATE TABLE IF NOT EXISTS metrics_runner_counts (
    invocation_id TEXT NOT NULL REFERENCES builds(invocation_id) ON DELETE CASCADE,
    name          TEXT NOT NULL,   -- "disk cache hit" | "internal" | "worker" | …
    exec_kind     TEXT,
    count         INTEGER NOT NULL,
    PRIMARY KEY (invocation_id, name)
);

CREATE TABLE IF NOT EXISTS action_mnemonics (
    invocation_id    TEXT NOT NULL REFERENCES builds(invocation_id) ON DELETE CASCADE,
    mnemonic         TEXT NOT NULL,
    actions_created  INTEGER,
    actions_executed INTEGER,
    system_time_ms   INTEGER,
    user_time_ms     INTEGER,
    PRIMARY KEY (invocation_id, mnemonic)
);

CREATE TABLE IF NOT EXISTS disk_cache_reports (
    taken_at      INTEGER PRIMARY KEY,  -- unix ms
    cache_dir     TEXT NOT NULL,
    total_bytes   INTEGER,
    file_count    INTEGER,
    oldest_mtime  INTEGER,
    gc_freed_bytes INTEGER              -- 0 if report-only
);

CREATE INDEX IF NOT EXISTS idx_metrics_worktree ON metrics(worktree);
CREATE INDEX IF NOT EXISTS idx_metrics_finished ON metrics(finished_at);
