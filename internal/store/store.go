// Package store persists builds to SQLite via the pure-Go modernc.org/sqlite
// driver (no cgo → static binary, trivial cross-compile).
//
// Single-writer discipline (E2 §2.4): the DB is opened with SetMaxOpenConns(1)
// + WAL + busy_timeout so SQLITE_BUSY cannot occur; all writes are additionally
// serialized through the registry's write-lock. Times are stored as unix epoch
// milliseconds; the Go layer converts to/from time.Time and the wire layer emits
// RFC3339 UTC strings.
package store

import (
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"github.com/antoniospapantoniou/bazel-broker/internal/build"
)

//go:embed schema.sql
var schemaSQL string

// schemaVersion is the user_version this build understands.
const schemaVersion = 1

// dsnFmt configures WAL + a 5s busy timeout + foreign keys.
const dsnFmt = "file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)"

// Store is the SQLite-backed build persistence layer.
type Store struct {
	db  *sql.DB
	log *slog.Logger
}

// Open creates the parent directory, opens the DB, and migrates to schema v1.
func Open(path string, log *slog.Logger) (*Store, error) {
	if log == nil {
		log = slog.Default()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("mkdir db dir: %w", err)
	}
	db, err := sql.Open("sqlite", fmt.Sprintf(dsnFmt, path))
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	// Serialize writers at the connection level (belt-and-suspenders with WAL).
	db.SetMaxOpenConns(1)

	s := &Store{db: db, log: log}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// migrate stamps schema v1 if the DB is fresh (user_version == 0).
func (s *Store) migrate() error {
	var v int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		return fmt.Errorf("read user_version: %w", err)
	}
	if v >= schemaVersion {
		return nil
	}
	if _, err := s.db.Exec(schemaSQL); err != nil {
		return fmt.Errorf("apply schema v%d: %w", schemaVersion, err)
	}
	s.log.Info("store migrated", "user_version", schemaVersion)
	return nil
}

// Close releases the underlying handle.
func (s *Store) Close() error { return s.db.Close() }

// UpsertBuild inserts or replaces a build row (idempotent on invocation_id).
func (s *Store) UpsertBuild(b *build.Build) error {
	targets, err := json.Marshal(nonNil(b.Targets))
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`
		INSERT INTO builds (invocation_id, worktree, targets, pid, state, start_time, end_time, exit_code, source)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(invocation_id) DO UPDATE SET
			worktree   = excluded.worktree,
			targets    = excluded.targets,
			pid        = excluded.pid,
			state      = excluded.state,
			start_time = excluded.start_time,
			end_time   = excluded.end_time,
			exit_code  = excluded.exit_code,
			source     = excluded.source`,
		b.InvocationID, b.Worktree, string(targets), b.PID, string(b.State),
		toMillis(b.StartTime), toMillis(b.EndTime), b.ExitCode, string(b.Source))
	if err != nil {
		return fmt.Errorf("upsert build %s: %w", b.InvocationID, err)
	}
	return nil
}

// MarkTerminal flips a build to a terminal state with its end time and exit code.
func (s *Store) MarkTerminal(invocationID string, state build.State, exit int, end time.Time) error {
	_, err := s.db.Exec(
		`UPDATE builds SET state = ?, exit_code = ?, end_time = ? WHERE invocation_id = ?`,
		string(state), exit, toMillis(end), invocationID)
	if err != nil {
		return fmt.Errorf("mark terminal %s: %w", invocationID, err)
	}
	return nil
}

// RecentBuilds returns up to limit builds, newest StartTime first (boot hydration).
func (s *Store) RecentBuilds(limit int) ([]*build.Build, error) {
	rows, err := s.db.Query(
		`SELECT invocation_id, worktree, targets, pid, state, start_time, end_time, exit_code, source
		   FROM builds ORDER BY start_time DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("recent builds: %w", err)
	}
	defer rows.Close()

	out := make([]*build.Build, 0, limit)
	for rows.Next() {
		b, err := scanBuild(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// GetBuild returns one build by id (ok=false if absent).
func (s *Store) GetBuild(invocationID string) (*build.Build, bool, error) {
	row := s.db.QueryRow(
		`SELECT invocation_id, worktree, targets, pid, state, start_time, end_time, exit_code, source
		   FROM builds WHERE invocation_id = ?`, invocationID)
	b, err := scanBuild(row)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return b, true, nil
}

// scanner abstracts *sql.Row and *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

func scanBuild(sc scanner) (*build.Build, error) {
	var (
		b           build.Build
		targetsJSON string
		state, src  string
		startMS     int64
		endMS       int64
	)
	if err := sc.Scan(&b.InvocationID, &b.Worktree, &targetsJSON, &b.PID,
		&state, &startMS, &endMS, &b.ExitCode, &src); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(targetsJSON), &b.Targets); err != nil {
		return nil, fmt.Errorf("decode targets for %s: %w", b.InvocationID, err)
	}
	b.State = build.State(state)
	b.Source = build.Source(src)
	b.StartTime = fromMillis(startMS)
	b.EndTime = fromMillis(endMS)
	return &b, nil
}

func toMillis(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func fromMillis(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
