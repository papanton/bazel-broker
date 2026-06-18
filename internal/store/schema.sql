-- schema v1 — Bazel Broker build store (E2). modernc.org/sqlite (pure Go).
PRAGMA user_version = 1;

CREATE TABLE IF NOT EXISTS builds (
    invocation_id TEXT PRIMARY KEY,
    worktree      TEXT NOT NULL,
    targets       TEXT NOT NULL DEFAULT '[]',  -- JSON array of strings
    pid           INTEGER NOT NULL DEFAULT 0,
    state         TEXT NOT NULL,               -- queued|running|finished|failed|killed|gone|unknown
    start_time    INTEGER NOT NULL,            -- unix epoch milliseconds
    end_time      INTEGER NOT NULL DEFAULT 0,  -- 0 = not ended
    exit_code     INTEGER NOT NULL DEFAULT 0,
    source        TEXT NOT NULL                -- registered|discovered
);

CREATE INDEX IF NOT EXISTS idx_builds_state      ON builds(state);
CREATE INDEX IF NOT EXISTS idx_builds_start_time ON builds(start_time DESC);

-- Reserved for E4 (declared now so the schema file is the single source of
-- truth, but no E2 code reads/writes it). E4 migrates to user_version=2.
CREATE TABLE IF NOT EXISTS metrics (
    invocation_id   TEXT PRIMARY KEY REFERENCES builds(invocation_id) ON DELETE CASCADE,
    actions_total   INTEGER,
    actions_cached  INTEGER,
    cache_hit_ratio REAL,
    wall_ms         INTEGER,
    json            TEXT          -- raw BuildMetrics blob
);
