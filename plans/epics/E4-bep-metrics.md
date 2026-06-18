# E4 — BEP Ingest, Metrics & Cache Insight — Implementation Plan

> ⚠️ **Superseded where it conflicts with [`00-consolidated-review.md`](00-consolidated-review.md) + E2 §4 (frozen contract).** Conform before coding:
> - **P4:** lock the BEP path string and the **cache-hit definition (`ActionSummary.runner_count[]`)** byte/semantics-identical with E1 (C7/C8). Join builds on `BuildStarted.uuid`, not filename; truncation supervisor is **mandatory**.
> - Routes are `GET /builds/{invocation_id}/metrics` + `GET /metrics?invocation_id=…`; set `Build.cache_hit_ratio` (0–1 float) + `Build.profile_url` (fully-formed URL).
> - **OD-C:** `GET /profile/{id}/{name}` likely token-exempt + `Origin`-restricted CORS (Perfetto can't send the bearer); Perfetto `?url=` needs HTTPS → postMessage shim.

> Epic E4 of the Bazel Broker project. Maps to architecture §5 (C3 live-trace ingest, C4
> Perfetto deep timing) and §8.4 (broker cache features). Maps to milestone **M3**.
>
> **Depends on:** E2 (broker daemon core + SQLite store + HTTP/WS API) and E1 (per-worktree
> `--build_event_json_file` + `--profile` flags actually emitted by builds).
>
> Status: **Draft v1** · Owner: Antonis · Target: macOS arm64, Go 1.26

---

## 1. Goal & scope recap

**Goal (from 02-epics.md E4):** live trace + cache%/timing/failures + Perfetto deep-links.

Turn each build's Build Event Protocol (BEP) JSON stream into a durable, queryable picture of
*what happened* and *how well the cache worked*, and give the operator a one-click deep dive
into Bazel's own critical-path profile via Perfetto — all self-contained on the local Mac.

**In scope (deliverables):**
1. **BEP tailer** — follow each build's per-worktree `--build_event_json_file` with `nxadm/tail`,
   correctly handling **truncation** (a new build at the same path truncates/recreates the file).
2. **BEP parser** — `protojson`-decode each NDJSON line into Go types generated from Bazel's
   `build_event_stream.proto` (includes the protoc + `protoc-gen-go` codegen step).
3. **Live registry feed** — stream `Progress`, `Configured`, `ActionExecuted`, `TestResult`,
   `BuildStarted`, `BuildFinished` into the in-memory registry (E2) so `ls`/WS show live state.
4. **Metrics extraction** — from `BuildMetrics`: `ActionSummary` (disk/remote-cache hits derived
   from `runner_count[]` — NOT `created − executed`; see §2.4), `TimingMetrics`, target/package
   metrics; persist to SQLite (extends the `metrics` table E2 pre-declared in schema v1).
5. **API** — `GET /metrics?invocation_id=<id>` (E2's frozen param) returning the metrics JSON for
   one build; this fills E2's reserved `/metrics` `501` stub.
6. **Low-hit alerts** — threshold logic that flags a cache-hit-ratio drop and signals likely
   path/env leakage busting the cache (§8.3 / §8.4.1).
7. **Disk-cache GC + size report** — scan the shared `--disk_cache` dir by size/age and prune,
   and/or drive Bazel's `--experimental_disk_cache_gc_max_size`.
8. **Perfetto serving** — locate the build's `--profile` gz, serve it over the broker's
   localhost HTTP, and construct a `ui.perfetto.dev/#!/?url=` deep-link.

**Out of scope (other epics):** admission/stagger (E5), process discovery & kill (E3), the
web dashboard (E7) and menu-bar app (E8) that *consume* these metrics, the `.bazelrc` flag
config itself (E1). This epic *produces* metrics rows and the `/metrics` endpoint; the
front-ends render them.

**New Go package:** `internal/bep` (tailer + parser + metrics extraction), plus small
additions to `internal/store` (schema + queries), `internal/api` (the `/metrics` and Perfetto
handlers), and a new `internal/diskcache` (GC + size report). All inside the existing broker
module from E2.

---

## 2. Design & implementation details

### 2.1 Proto codegen step (`build_event_stream.pb.go`)

BEP events are `build_event_stream.BuildEvent` messages serialized one-per-line as JSON
(`--build_event_json_file`). To decode with `protojson` we need the generated Go structs.

**Proto sources required** (from the Bazel source tree, matching the Bazel version in use —
see Risk R1). The canonical set, with their import graph:

```
src/main/java/com/google/devtools/build/lib/buildeventstream/proto/build_event_stream.proto   (root)
  ├─ imports: src/main/protobuf/action_cache.proto                  (ActionCacheStatistics)
  ├─ imports: src/main/protobuf/command_line.proto                  (CommandLine, used by StructuredCommandLine)
  ├─ imports: src/main/protobuf/invocation_policy.proto             (transitively via command_line)
  ├─ imports: src/main/protobuf/failure_details.proto              (FailureDetail)
  ├─ imports: src/main/java/com/google/devtools/build/lib/packages/metrics/package_load_metrics.proto
  └─ imports: google/protobuf/any.proto, google/protobuf/duration.proto,
              google/protobuf/timestamp.proto                       (well-known, ship with protoc)
```

> ⚠️ Verified against upstream: the root imports `google/protobuf/any.proto` as well (the earlier
> draft omitted it — `Any` is used by some BEP payloads). `action_cache.proto` lives in package
> `blaze` (referenced as `blaze.ActionCacheStatistics`), so the `M`-mapping below uses the
> *filename*, not the package, as the key. Re-derive this exact import set from the **pinned**
> Bazel tag at codegen time (`protoc --dependency_out` or `grep '^import' *.proto`) rather than
> trusting this list across version bumps (R1).

`BuildMetrics` (the message we care most about) is defined **inline** in
`build_event_stream.proto` — it is NOT a separate import — and contains nested
`ActionSummary`, `MemoryMetrics`, `TargetMetrics`, `PackageMetrics`, `TimingMetrics`,
`CumulativeMetrics`, `ArtifactMetrics`, `BuildGraphMetrics`, `WorkerMetrics`,
`NetworkMetrics`, `DynamicExecutionMetrics`.

**Vendoring approach (decision surfaced as D-E4-1):** the cleanest reproducible path is to
vendor the exact `.proto` files for the pinned Bazel version under
`third_party/bazel_protos/` rather than `git submodule` the whole Bazel repo. Codegen step:

```bash
# tools: protoc 35, protoc-gen-go 1.36 (both stated available); Go 1.26
# Run from repo root. Output package: internal/genproto/buildeventstream
PROTO_ROOT=third_party/bazel_protos
OUT=internal/genproto

protoc \
  -I "$PROTO_ROOT" \
  -I "$(go env GOPATH)/pkg/mod/google.golang.org/protobuf@*/" \  # well-known types
  --go_out="$OUT" \
  --go_opt=paths=source_relative \
  --go_opt=Mbuild_event_stream.proto=github.com/<org>/bazel-broker/internal/genproto/buildeventstream \
  --go_opt=Mcommand_line.proto=github.com/<org>/bazel-broker/internal/genproto/commandline \
  --go_opt=Maction_cache.proto=github.com/<org>/bazel-broker/internal/genproto/actioncache \
  --go_opt=Mfailure_details.proto=github.com/<org>/bazel-broker/internal/genproto/failuredetails \
  --go_opt=Mpackage_load_metrics.proto=github.com/<org>/bazel-broker/internal/genproto/packagemetrics \
  build_event_stream.proto command_line.proto action_cache.proto \
  failure_details.proto invocation_policy.proto package_load_metrics.proto
```

- The `M<file>=<import path>` mappings rewrite each proto's Go import path so the generated
  files form a consistent internal package set. Bazel's protos lack `go_package` options
  (confirmed: `build_event_stream.proto` has **no** `go_package`), so per-file `M` overrides are
  mandatory — without them `protoc-gen-go` errors with "unable to determine Go import path".
  **Only map the Bazel protos**, never the well-known types (`google/protobuf/*`): protoc-gen-go
  already knows their canonical import paths (`google.golang.org/protobuf/types/known/...`), and
  M-mapping them produces broken imports. Two viable styles, pick one and document it:
  - **per-file `M…=…`** (shown above) — explicit, one line per proto; OR
  - **`--go_opt=module=github.com/<org>/bazel-broker/internal/genproto`** to strip a common prefix
    — but this requires every proto to carry a `go_package` (they don't), so for Bazel's protos
    the per-file `M` form is the only one that works. Keep `M`.
  Cross-check after generation: `go vet ./internal/genproto/...` and a compile, since a wrong `M`
  silently produces an importable-but-mis-pathed package that only fails at use site.
- Wrap this in **`make protos`** (a phony Make target) and **commit the generated
  `*.pb.go`** (so a clean checkout builds without protoc; codegen is only re-run on Bazel
  version bump). Add a `//go:generate` directive in `internal/genproto/gen.go` documenting it.
- Pin tool versions in `tools/tools.go` (the `_ "google.golang.org/protobuf/cmd/protoc-gen-go"`
  blank-import pattern) so `go.mod` records `protoc-gen-go` 1.36.

### 2.2 Tail + truncation handling

**E1↔E4 contract — already resolved on E1's side; E4 must conform.** E1 (`E1-cache-config.md`
§2.4, §4) is authoritative (it ships at M0, before this epic) and has **firmly chosen a relative,
per-*worktree* path**, NOT per-invocation:
- BEP json:  `<worktree>/.bazel-broker/bep.json`
- profile:   `<worktree>/.bazel-broker/command.profile.gz`

E1's collision-safety argument: a relative path resolves against the worktree root, so distinct
worktrees get distinct files; and the **exclusive per-output-base lock** guarantees at most one
build per worktree at a time, so there is never a concurrent writer to one worktree's `bep.json`.
**Consequence E4 MUST accept:** the *same* file is **reused and truncated** by the next build in
that worktree. E1 explicitly declines to mint per-invocation paths and tells E4 to rotate history
into SQLite on `BuildFinished` (E1 OD-1). **Therefore the truncation supervisor (below) is
MANDATORY, not a fallback**, and the "join key in the filename" idea this plan floated does not
hold — the filename carries the *worktree*, and the invocation id comes from the stream's own
`BuildStarted.uuid` (§3) or the registry binding, not from the path.

The broker learns the file path two ways:
- **Active:** the registry binding from `Register` (E5/E2) gives `{worktree}` → derive
  `<worktree>/.bazel-broker/bep.json` per the E1 convention.
- **Passive:** the discovered invocation (E3) resolves the worktree from the process cwd → same
  deterministic path (see §4). Both paths use E1's `pwd -P`-resolved absolute worktree so the
  string matches what E3 discovers.

We use `github.com/nxadm/tail`:

```go
t, err := tail.TailFile(bepPath, tail.Config{
    Follow:    true,
    ReOpen:    true,   // reopen if the file is rotated/recreated (new build truncates)
    MustExist: false,  // tolerate "file not yet created" — broker may start tailing before bazel opens it
    Poll:      true,   // IMPORTANT on macOS: kqueue/fsevents truncation signals are flaky; poll is reliable
    Logger:    tail.DiscardingLogger,
    Location:  &tail.SeekInfo{Offset: 0, Whence: io.SeekStart},
})
```

**The truncation problem (unavoidable given E1's locked contract).** Because E1 reuses a
per-worktree file, the **same worktree's next build truncates the file in place** (Bazel opens
`--build_event_json_file` with O_TRUNC). `nxadm/tail` with `ReOpen:true` handles file *rotation*
(rename+recreate / new inode), but **in-place truncation** (file shrinks, *same* inode) is the
dangerous case `nxadm/tail` does not reliably catch: the tailer's read offset is now past EOF and
it silently stalls, and on macOS kqueue truncation events are flaky — hence `Poll:true`. This is
the residual risk E1 hands us, and the supervisor below is therefore **required**, not optional.

Defensive handling (we implement on top of `nxadm/tail`, do not rely on it alone):

1. **Inode + size watch supervisor (mandatory).** A small supervisor goroutine `os.Stat`s the
   path every `pollInterval` (250ms). If `(dev, ino)` changes (rotation) **or** the current size
   `< lastReadOffset` (in-place truncation), we **tear down the current `tail.Tail`, `Stop()` it,
   and start a fresh one from offset 0**, emitting a synthetic `bep.StreamRestarted{path}` so the
   parser resets per-stream state (partial-line buffer, seen-event set, the invocation binding).
   Before restarting, we **finalize the prior stream's metrics row** if it reached
   `BuildFinished`/`last_message`, so a fast rebuild can't lose the previous build's row.
2. **Drain-before-restart.** On a detected truncation we first read any bytes between
   `lastReadOffset` and the *old* EOF that we may not have consumed (the previous build's tail
   may not have been flushed to us before the truncate), to avoid dropping the final
   `BuildFinished`/`last_message`. Best-effort; if the file already shrank past those bytes they
   are gone and the registry timeout finalizes the row.
3. **Line framing safety.** `nxadm/tail` yields whole lines, but a truncation mid-line can
   surface a partial JSON line. The parser tolerates a `protojson.Unmarshal` error on a single
   line by logging + skipping (counter `bep_parse_errors_total`), never crashing the tailer.

(A truly per-invocation filename would eliminate truncation entirely, but that is E1's decision
and E1 has declined it — see §4 / D-E4-2. E4 does not get to assume it.)

Each tailed stream runs in its own goroutine, owned by a `bep.Manager` keyed by
`invocation_id`. `Manager` exposes `Watch(invocationID, bepPath)` and `Stop(invocationID)`,
called from registry lifecycle hooks (build appears → Watch; build finishes + grace → Stop).

### 2.3 protojson parsing loop

```go
import (
    "google.golang.org/protobuf/encoding/protojson"
    bes "github.com/<org>/bazel-broker/internal/genproto/buildeventstream"
)

var unmarshal = protojson.UnmarshalOptions{
    DiscardUnknown: true,  // forward-compat: tolerate fields from a newer Bazel (see R1)
}

func (s *stream) consume(line tail.Line) {
    raw := strings.TrimSpace(line.Text)
    if raw == "" { return }
    var ev bes.BuildEvent
    if err := unmarshal.Unmarshal([]byte(raw), &ev); err != nil {
        metrics.bepParseErrors.Inc()
        log.Debug("bep parse skip", "path", s.path, "err", err)
        return
    }
    s.dispatch(&ev)   // type-switch on ev.Payload + bind via ev.Id
}
```

`BuildEvent` is `{ id, children[], payload (oneof), last_message }`. The oneof `Payload`
gives us the concrete event. `dispatch` switches on `ev.Payload.(type)`:

| BEP payload (`ev.GetX()`)              | Action in `dispatch`                                                                 |
|----------------------------------------|--------------------------------------------------------------------------------------|
| `*BuildEvent_Started` (`BuildStarted`) | bind stream → `uuid` (invocation id, §3 driver 4); record start ts from `start_time` (Timestamp), command, cwd |
| `*BuildEvent_Progress`                 | append stderr/stdout chunks to live ring buffer; update "last activity" ts            |
| `*BuildEvent_Configured`               | count configured targets; update registry target list                                |
| `*BuildEvent_Action` (`ActionExecuted`)| increment live action counter; capture failures (exit code, stderr ref)              |
| `*BuildEvent_TestResult`               | per-test pass/fail/cached → feed pass/fail tally + live registry                      |
| `*BuildEvent_BuildMetrics`             | **extract & persist** (see §2.4) — usually arrives near stream end                    |
| `*BuildEvent_Finished` (`BuildFinished`)| record `exit_code.code` + finish ts; success = (code==0) (overall_success is DEPRECATED); mark terminal; flush row |
| `*BuildEvent_LastMessage` / `last_message=true` | signal end-of-stream → schedule `Stop` after grace                          |

`last_message` on a `BuildEvent` is BEP's authoritative end-of-stream marker — more reliable
than waiting on EOF (the file may stay open). We treat `BuildFinished` + `last_message` as the
build-complete trigger; if neither arrives (crash), a registry timeout finalizes the row.

### 2.4 BuildMetrics fields → cache hit/miss

From `BuildMetrics` we read (field names verified against the upstream
`build_event_stream.proto`; the parenthetical is the proto field number, which matters for the
codegen pin in R1):

```
ActionSummary:
  actions_created            (1, int64)  — total actions REGISTERED in the action graph,
                                           INCLUDING actions never executed this invocation
  actions_executed           (2, int64)  — actions executed this build. Per the proto's own
                                           comment: "includes any remote cache hits, but
                                           EXCLUDES local action cache hits." A --disk_cache hit
                                           is served via the remote-cache protocol, so it is
                                           counted in actions_executed (NOT subtracted out).
  actions_created_not_including_aspects (3, int64)
  action_data[]              (4, repeated ActionData) — per-mnemonic; each ActionData has
                                           mnemonic(1), actions_executed(2), first_started_ms(3),
                                           last_ended_ms(4), system_time(5), user_time(6),
                                           AND actions_created(7).  ← capture BOTH created and
                                           executed per mnemonic (the plan previously dropped
                                           per-mnemonic actions_created).
  remote_cache_hits          (5, int64)  — DEPRECATED in the proto. Do not rely on it; read it
                                           best-effort for old streams only.
  runner_count[]             (6, repeated RunnerCount {name, count, exec_kind}) — the structured
                                           equivalent of Bazel's "N processes: X disk cache hit,
                                           Y internal, Z darwin-sandbox" summary line. THIS is the
                                           authoritative per-execution-kind breakdown (see below).
  action_cache_statistics    (7, blaze.ActionCacheStatistics, from action_cache.proto) — precise
                                           local action-cache hits/misses/loads when present.

TimingMetrics:
  cpu_time_in_ms             (int64)
  wall_time_in_ms            (int64)
  analysis_phase_time_in_ms  (int64)
  execution_phase_time_in_ms (int64)
  actions_execution_start_in_ms

TargetMetrics:   targets_loaded, targets_configured, targets_configured_not_including_aspects
PackageMetrics:  packages_loaded; package_load_metrics[] (per-package load durations)
MemoryMetrics:   used_heap_size_post_build, peak_post_gc_heap_size
ArtifactMetrics: output_artifacts_seen / from_action_cache (bytes), source_artifacts_read
```

**Cache hit/miss semantics (decision surfaced — D-E4-3, this is genuinely subtle, and the
earlier draft of this plan had it WRONG — see the corrected analysis below).**

⚠️ **Do NOT use `actions_created − actions_executed` as the disk-cache-hit count.** The previous
draft proposed `cached = actions_created − actions_executed`. That formula is wrong in two
compounding ways, per the proto's own field documentation:
1. **A `--disk_cache` hit is counted as an *executed* action.** The disk cache is served over
   the remote-cache protocol, and the proto states `actions_executed` "includes any remote cache
   hits." So disk-cache hits *inflate* `actions_executed` rather than being subtracted out —
   `created − executed` therefore **understates** disk-cache hits, exactly inverting the intent.
2. **`actions_created` includes actions that were never executed this invocation** (unused /
   pruned by the build's request). So `created − executed` is contaminated by non-cache,
   never-reached actions. What it actually approximates is *local action-cache* hits plus
   un-evaluated actions — not the shared-disk-cache hit rate we care about.

**Authoritative source — `ActionSummary.runner_count[]` (proto field 6).** Each `RunnerCount`
is `{name, count, exec_kind}`; `name` is the human label Bazel also prints in its summary line
(`"disk cache hit"`, `"remote cache hit"`, `"internal"`, `"darwin-sandbox"`, `"worker"`, …).
This is the *structured* form of the "N processes: …" line and is the correct primary signal:

```
disk_cache_hits   = sum(rc.count for rc in runner_count if rc.name == "disk cache hit")
remote_cache_hits = sum(rc.count for rc in runner_count if rc.name == "remote cache hit")
processes_total   = sum(rc.count for rc in runner_count)
locally_executed  = processes_total − disk_cache_hits − remote_cache_hits − internal_count
hit_ratio         = (disk_cache_hits + remote_cache_hits) / max(processes_total, 1)
```

We **store the full `runner_count[]` breakdown** (every name+count) so ratios are computed at
read time and no interpretation is baked in. The exact `name` strings are version-sensitive
(the join key is the literal Bazel label), so we keep the raw rows rather than bucketing.

**Secondary precision — `action_cache_statistics` (ActionSummary field 7).** When present
(inline in `ActionSummary`, *not* a separate `BuildToolLogs` event), it gives exact local
action-cache hit/miss/load counts; best-effort enrichment.

**Ground-truth cross-check — the summary string.** `BuildFinished` / progress stderr carries
the human "N processes: … disk cache hit …" line; we regex-extract it and store it verbatim as
`summary_line`. The verification step (§5) asserts the `runner_count`-derived disk-cache-hit
count **equals** the number parsed from this line (they are two encodings of the same data, so
they should match exactly, not merely "within tolerance"). Note: E1's `measure.sh` uses a
*different* heuristic (`1 − actionsExecuted/actionsCreated`) for a quick shell estimate; E4 does
**not** adopt that formula — it is a coarse proxy and diverges from `runner_count` for the same
reasons above. **D-E4-3 is the choice of which derived number E6/E7/E8 *display* as "cache
hit %"** (runner_count disk-hits / processes vs including remote vs the action_cache_statistics
ratio); the raw inputs are stored either way. Escalate the displayed definition; do not bake it in.

### 2.5 SQLite metrics schema

Added to the E2 store via migration to **`PRAGMA user_version = 2`** (E2 stamped v1 and reserved
this; E2 §2.4 says E4 "migrates to user_version=2 to **populate, not restructure**").
`modernc.org/sqlite` (pure-Go, no cgo).

**Reconcile with E2's pre-declared table (was a mismatch).** E2 schema v1 already ships an empty
`metrics` table: `metrics(invocation_id PK REFERENCES builds(invocation_id) ON DELETE CASCADE,
actions_total, actions_cached, cache_hit_ratio, wall_ms, json)`. The earlier E4 draft introduced a
*separate* `build_metrics` table, silently orphaning E2's reserved one — a contract break. E4
instead **extends the existing `metrics` table in place** with `ALTER TABLE metrics ADD COLUMN …`
(SQLite supports add-column), preserving the E2-declared columns and the FK to `builds`. Note the
name remap: E2's `actions_total` ← `ActionSummary.actions_created`; E2's `actions_cached` ← the
**`runner_count`-derived disk-cache-hit count** (§2.4), *not* `created − executed`; E2's `wall_ms`
← `TimingMetrics.wall_time_in_ms`; E2's `json` ← the raw `BuildMetrics` protojson blob. We keep
E2's column names as canonical and add the rest:

```sql
-- schema v2 (extends E2's reserved `metrics` table; does NOT create a parallel table)
PRAGMA user_version = 2;

ALTER TABLE metrics ADD COLUMN worktree           TEXT;     -- resolved from registry
ALTER TABLE metrics ADD COLUMN started_at         INTEGER;  -- unix ms from BuildStarted.start_time
                                                           --   (Timestamp; start_time_millis is DEPRECATED)
ALTER TABLE metrics ADD COLUMN finished_at        INTEGER;  -- unix ms (BuildFinished)
ALTER TABLE metrics ADD COLUMN exit_code          INTEGER;  -- BuildFinished.exit_code.code
ALTER TABLE metrics ADD COLUMN success            INTEGER;  -- 0/1 derived from exit_code==0
                                                           --   (overall_success is DEPRECATED — do not read)
ALTER TABLE metrics ADD COLUMN actions_executed   INTEGER;  -- ActionSummary.actions_executed (raw)
-- E2-declared: actions_total (=actions_created), actions_cached (=disk-cache hits from runner_count),
--              cache_hit_ratio (derived; NULL if processes_total=0), json (raw BuildMetrics blob)
ALTER TABLE metrics ADD COLUMN disk_cache_hits    INTEGER;  -- runner_count "disk cache hit"
ALTER TABLE metrics ADD COLUMN remote_cache_hits  INTEGER;  -- runner_count "remote cache hit" (proto field 5 is DEPRECATED)
ALTER TABLE metrics ADD COLUMN processes_total    INTEGER;  -- sum(runner_count.count)
ALTER TABLE metrics ADD COLUMN cpu_time_ms        INTEGER;  -- TimingMetrics
ALTER TABLE metrics ADD COLUMN analysis_ms        INTEGER;
ALTER TABLE metrics ADD COLUMN execution_ms       INTEGER;
ALTER TABLE metrics ADD COLUMN targets_configured INTEGER;  -- TargetMetrics
ALTER TABLE metrics ADD COLUMN packages_loaded    INTEGER;  -- PackageMetrics
ALTER TABLE metrics ADD COLUMN peak_heap_bytes    INTEGER;  -- MemoryMetrics
ALTER TABLE metrics ADD COLUMN tests_total        INTEGER;  -- from TestResult tally
ALTER TABLE metrics ADD COLUMN tests_failed       INTEGER;
ALTER TABLE metrics ADD COLUMN summary_line       TEXT;     -- raw "N processes: …" ground-truth string
ALTER TABLE metrics ADD COLUMN profile_path       TEXT;     -- absolute path to --profile gz (Perfetto)
ALTER TABLE metrics ADD COLUMN bep_path           TEXT;     -- the tailed file (provenance/debug)
ALTER TABLE metrics ADD COLUMN alert              TEXT;     -- NULL | 'low_cache_hit' | 'cache_busting_suspected'
-- (`json` already exists from v1 → the raw_metrics_json escape hatch; reuse it, don't add a dup column)

-- Structured runner-count breakdown (the "N processes: X disk cache hit, Y internal…" line).
-- This is the authoritative per-exec-kind table; cache hit ratio is computed from it at read time.
CREATE TABLE IF NOT EXISTS metrics_runner_counts (
    invocation_id        TEXT NOT NULL REFERENCES builds(invocation_id) ON DELETE CASCADE,
    name                 TEXT NOT NULL,             -- "disk cache hit" | "internal" | "darwin-sandbox" | …
    exec_kind            TEXT,                      -- RunnerCount.exec_kind
    count                INTEGER NOT NULL,
    PRIMARY KEY (invocation_id, name)
);

-- Per-mnemonic action breakdown (from ActionSummary.action_data[]); useful for E7 charts
-- and for spotting which mnemonic (e.g. SwiftCompile) is busting the cache. Capture BOTH
-- actions_created (proto field 7) and actions_executed (field 2) for a per-mnemonic ratio.
CREATE TABLE IF NOT EXISTS action_mnemonics (
    invocation_id        TEXT NOT NULL REFERENCES builds(invocation_id) ON DELETE CASCADE,
    mnemonic             TEXT NOT NULL,
    actions_created      INTEGER,                   -- ActionData.actions_created (was dropped before)
    actions_executed     INTEGER,
    system_time_ms       INTEGER,
    user_time_ms         INTEGER,
    PRIMARY KEY (invocation_id, mnemonic)
);

-- Disk-cache size report snapshots (for trend + GC reporting, §2.8).
CREATE TABLE IF NOT EXISTS disk_cache_reports (
    taken_at             INTEGER PRIMARY KEY,       -- unix ms
    cache_dir            TEXT NOT NULL,
    total_bytes          INTEGER,
    file_count           INTEGER,
    oldest_atime         INTEGER,
    gc_freed_bytes       INTEGER                    -- 0 if report-only
);

CREATE INDEX IF NOT EXISTS idx_metrics_worktree ON metrics(worktree);
CREATE INDEX IF NOT EXISTS idx_metrics_finished ON metrics(finished_at);
```

Writes are **upserts** (`INSERT INTO metrics … ON CONFLICT(invocation_id) DO UPDATE`) because
metrics arrive incrementally (started → metrics → finished). The `metrics` row's FK to `builds`
means a build must be registered first; E4 relies on E2 having upserted the `builds` row (the
registry lifecycle that triggers `Manager.Watch`) before the BEP stream's `BuildMetrics` lands —
true in practice, but the upsert must tolerate a not-yet-present `builds` row by registering a
stub (or the FK will reject the insert). The v1 `json` column preserves every field even when a
newer Bazel adds metrics we lack columns for (R1 mitigation).

### 2.6 `GET /metrics?invocation_id=` JSON shape

Added to the E2 HTTP mux (loopback + bearer token, per E2). ⚠️ **Use E2's frozen param name:**
E2's route table (`E2-broker-core.md` §4.2) declares `GET /metrics?invocation_id=…`. The earlier
E4 draft used `?id=`, which would break the frozen contract. **Standardize on `?invocation_id=`**
(accept `?id=` as a documented alias only if E2's owner signs off). Omitting it returns the
most-recent N (paged) for dashboards.

```jsonc
// GET /metrics?invocation_id=8f3c…   ->  200
{
  "invocation_id": "8f3c2a1e-…",
  "worktree": "/Users/antonios/worktrees/feature-a",
  "started_at": 1750000000000,
  "finished_at": 1750000042000,
  "exit_code": 0,
  "success": true,
  "cache": {
    // raw inputs (stored; never lossily pre-aggregated)
    "actions_created": 1820,              // = ActionSummary.actions_created (E2 col: actions_total)
    "actions_executed": 142,             // = ActionSummary.actions_executed (incl. disk/remote hits!)
    "processes_total": 1842,             // sum(runner_count.count)
    "runner_counts": [                   // the authoritative breakdown (§2.4)
      {"name": "disk cache hit",  "count": 1678},
      {"name": "darwin-sandbox",  "count": 142},
      {"name": "internal",        "count": 22}
    ],
    // derived at read time from runner_counts
    "disk_cache_hits": 1678,             // E2 col: actions_cached
    "remote_cache_hits": 0,
    "hit_ratio": 0.911,                  // (disk+remote hits) / processes_total = 1678/1842
    "summary_line": "1842 processes: 1678 disk cache hit, 142 darwin-sandbox, 22 internal.",
    "by_mnemonic": [
      {"mnemonic": "SwiftCompile", "actions_created": 90, "actions_executed": 88, "user_time_ms": 41200},
      {"mnemonic": "ObjcCompile",  "actions_created": 60, "actions_executed": 41, "user_time_ms": 9100}
    ]
  },
  "timing": {
    "wall_time_ms": 42000, "cpu_time_ms": 311000,
    "analysis_ms": 3800, "execution_ms": 36900
  },
  "targets_configured": 312,
  "packages_loaded": 198,
  "peak_heap_bytes": 1610612736,
  "tests": {"total": 12, "failed": 0},
  "alert": null,                            // or "low_cache_hit" / "cache_busting_suspected"
  "profile": {
    "path": "/Users/.../feature-a/.bazel-broker/command.profile.gz",
    "served_url": "http://127.0.0.1:8765/profile/8f3c2a1e-…/command.profile.gz",
    // perfetto_url is the broker-served postMessage SHIM page (Option B), not a bare ?url= link
    // (the ?url= form needs HTTPS — see §2.9 / D-E4-6). The shim opens ui.perfetto.dev and pushes
    // the trace bytes via postMessage.
    "perfetto_url": "http://127.0.0.1:8765/perfetto/8f3c2a1e-…"
  }
}
```

`404` if unknown id; `200` with `{"builds":[…]}` (trimmed rows) for the no-`id` list form.

### 2.7 Low-hit alert logic (§8.3 / §8.4.1)

Computed when a build's metrics row is finalized:

```
ratio        = (disk_cache_hits + remote_cache_hits) / max(processes_total, 1)   // §2.4 derivation
baseline     = trailing median ratio for the SAME worktree over the last K builds (default K=10)
floor        = config.low_hit_threshold        (default 0.50)

alert = nil
if processes_total >= MIN_PROCS (default 200):         // ignore tiny/no-op builds
    if ratio < floor:                                   alert = "low_cache_hit"
    if baseline != nil && ratio < baseline - DELTA (default 0.20):
                                                        alert = "cache_busting_suspected"
```

`cache_busting_suspected` is the §8.3 signal: a **sudden drop relative to this worktree's own
recent history** is the fingerprint of path/env leakage (absolute worktree paths or `$PATH`
leaking into the action key) busting the shared `--disk_cache`, as opposed to a legitimately
large/clean build. The alert string is stored on the row and surfaced via `/metrics` and the
WS event feed (E7/E8 render a badge). The alert is **advisory only** — it does not change
admission (that's E5). Thresholds live in the E2 config file.

### 2.8 Disk-cache GC + size report (§8.4.3)

Two cooperating mechanisms (decision surfaced — D-E4-4):

**(a) Report (always-on).** A periodic job (default every 15 min, also on-demand via
`GET /diskcache`) walks the shared `--disk_cache` directory:

```
walk cache_dir:
    sum file sizes -> total_bytes
    count files
    track min(atime) -> oldest_atime    // disk cache uses atime/mtime for LRU
insert row into disk_cache_reports
```

Path comes from broker config (the same shared dir E1 points every worktree at). The walk is
cheap (stat-only) but the dir can be large → bounded concurrency + skip on overlap with a
running GC.

**(b) GC.** Prefer **driving Bazel's built-in GC** where the in-use Bazel supports it:
`--experimental_disk_cache_gc_max_size=<N>G` (and `--experimental_disk_cache_gc_idle_delay`),
which lets the *Bazel server* prune the disk cache safely and atomically. The broker's role is
to (i) ensure E1's `.bazelrc` carries the flag and (ii) report. **Fallback direct GC** (when
the Bazel version lacks the flag): broker-side LRU prune.

```
direct LRU prune (fallback, off by default — see SAFETY below):
    target = config.disk_cache_max_bytes      (e.g. 50 GiB)
    if total_bytes <= target: return
    list cache entries sorted by mtime ascending (oldest first)   // see (2) on atime caveat
    while total_bytes > target * HYSTERESIS (0.9):
        e = next oldest entry
        if mtime(e) is within min_age of now: skip                // freshly written; never touch
        os.Remove(e); total_bytes -= e.size; freed += e.size
    record gc_freed_bytes in disk_cache_reports
```

**SAFETY (R4) — concurrency analysis, surfaced for sign-off (D-E4-4), not resolved here.** A
build writing the cache concurrently can race a delete. Precisely:
- The disk cache is a content-addressed store (`<hash-prefix>/<digest>`). Under POSIX, `os.Remove`
  *unlinks the path*; a writer or reader holding an open fd is unaffected (the inode lives until
  the last fd closes), so it does **not** corrupt an in-flight write. The real hazards are:
  (a) a **reader** that `stat`s the path, finds it, then `open`s after we unlink → a recoverable
  *miss* (Bazel re-executes), acceptable; (b) we delete a blob that was written but whose action
  result still references it → a dangling reference and a miss, also recoverable; (c) Bazel's own
  writes are typically **write-to-temp + atomic rename**, so a half-written blob is never visible
  under its final name — but we must still **never delete a file with an open writer** to avoid
  surprising Bazel's own GC bookkeeping. None of these is silent corruption, but (a)/(b) waste work.
- **`atime` is unreliable for LRU** on macOS: volumes are frequently mounted `noatime`/`relatime`
  or atime updates lag, so an LRU keyed on `atime` can evict hot blobs. Prefer `mtime` (write
  time) as the age signal, and treat this as a known imprecision — another reason to **prefer the
  Bazel-native GC**, which tracks real last-use internally.

Mitigations (all gated behind making direct GC **opt-in**): (1) **prefer Bazel-native GC**
(`--experimental_disk_cache_gc_max_size`), which is race-safe and uses real last-use; (2) only
delete entries older than `min_age` (default 24h by `mtime`) so nothing recently written is
touched; (3) hold an advisory broker-wide lock so report/GC/another-GC never overlap; (4) **skip
GC entirely while *any* registered or discovered build targets the same `disk_cache`** unless
`force=true` (the registry already knows in-flight builds and their worktrees → their shared
`disk_cache` is known from config). **Open question for sign-off:** even with (1)–(4), is direct
GC worth shipping at all, or do we ship **report-only** until Bazel-native GC covers every pinned
version? Escalate D-E4-4; do not enable direct GC by default.

### 2.9 Perfetto deep-link construction

Bazel writes a Chrome-trace JSON profile (gzip) per build (`--profile`; E1's locked convention
is `<worktree>/.bazel-broker/command.profile.gz`, §4). There are **two** ways to get it into
ui.perfetto.dev, and the earlier draft picked the one that does not actually work for a local
broker. Per Perfetto's official "deep-linking" doc:

- **`?url=` deep-link (Option A — does NOT work for us as drafted).** `ui.perfetto.dev/#!/?url=`
  fetches the trace with a simple cross-origin GET, **but the doc requires the trace be served
  over HTTPS** and sets CORS allow-origin to `https://ui.perfetto.dev`. It does **not** document
  an exception for `http://127.0.0.1` / `http://localhost` — the earlier claim that localhost is
  on a CORS allowlist is **unsubstantiated**. A plain `http://127.0.0.1:PORT/...` URL will be
  refused (mixed-content / non-HTTPS). Making this work would require the broker to serve HTTPS
  with a cert the browser trusts — heavy for a localhost daemon. **Flagged as D-E4-6.**
- **`postMessage` / `open_trace_in_ui` (Option B — recommended, works from localhost).** The
  Perfetto UI is client-only; you `window.open('https://ui.perfetto.dev')`, wait for its
  `PING`/ready handshake, then `postMessage` the trace bytes (`ArrayBuffer`) with
  `{perfetto:{buffer, title, url}}`. This **bypasses the HTTPS and CORS requirements entirely**
  (the bytes never transit a cross-origin fetch — they're handed over via postMessage) and is the
  documented path for local/non-HTTPS traces. The trade-off: it needs a tiny **broker-served HTML
  shim page** (over plain http on loopback) that does the open+handshake+postMessage, rather than
  a single bare URL the CLI can `open`. The web UI (E7) hosts this naturally; for `brokerctl
  profile` (E6) and the menu-bar app (E8), `open` the shim page `http://127.0.0.1:PORT/perfetto/<id>`.

Serving plan:
1. **Locate the profile.** `profile_path` derived from the registry's `worktree` + E1's relative
   convention, or reported by the wrapper (E5). Stored on the metrics row.
2. **Serve the bytes.** Handler `GET /profile/{invocation_id}/{name}` streams the gz with
   `Content-Type: application/gzip` (and, for Option B, the shim fetches it **same-origin** from
   the broker, so no CORS header is needed). If Option A is ever pursued, the same handler adds
   `Access-Control-Allow-Origin: https://ui.perfetto.dev`. Path is resolved **only** from the
   DB's stored `profile_path` for that id — never from the URL component — to avoid traversal (R6).
3. **Serve the shim.** `GET /perfetto/{invocation_id}` returns a minimal HTML page (from
   `embed.FS`) that fetches `/profile/{id}/...` same-origin and postMessages it to a freshly
   opened ui.perfetto.dev tab. The `perfetto_url` field in `/metrics` becomes the **shim** URL:

```go
// Option B (recommended): the broker shim page drives postMessage; CLI/UIs just open it.
shim := fmt.Sprintf("http://127.0.0.1:%d/perfetto/%s", port, id)
// (Option A, gated on D-E4-6 + HTTPS serving:)
// served := fmt.Sprintf("https://127.0.0.1:%d/profile/%s/%s", port, id, filepath.Base(profilePath))
// deep   := "https://ui.perfetto.dev/#!/?url=" + url.QueryEscape(served)
```

`brokerctl profile <id>` (E6) and the web/menu-bar UIs (E7/E8) `open` the shim URL; Perfetto
loads the trace via postMessage and shows the per-action critical path. The broker must stay up
while the tab loads — fine, it's a daemon.

---

## 3. Sequencing (ordered, checkpointed tasks)

Each task is independently verifiable. Checkpoints marked **✓CP**.

1. **Vendor protos + codegen.** Add `third_party/bazel_protos/` (pinned Bazel version),
   `tools/tools.go`, `make protos`, commit generated `internal/genproto/**`.
   **✓CP:** `go build ./internal/genproto/...` compiles; a tiny `_test.go` `protojson`-decodes
   a hand-written `BuildEvent` JSON with a `BuildMetrics` payload into the struct.

2. **BEP parser core (`internal/bep`).** `dispatch` type-switch over payloads; pure function
   `ExtractMetrics(*bes.BuildMetrics) MetricsRow`. No I/O yet.
   **✓CP:** table-driven unit test feeds recorded BEP lines (a captured real stream in
   `testdata/bep/*.ndjson`) → asserts derived `cache_hit_ratio`, counts, timing.

3. **SQLite schema v2 + store methods.** Migration; `UpsertMetrics`, `GetMetrics(id)`,
   `ListMetrics`, `InsertDiskReport`. Extend E2 store.
   **✓CP:** store unit test round-trips a `MetricsRow` and queries it back; migration is
   idempotent (run twice).

4. **Tailer + truncation supervisor (`bep.Manager`).** `nxadm/tail` wrapper + inode/size
   restart logic + per-stream parse loop wired to the parser and store.
   **✓CP:** `fake-bazel.sh` (E0) writes a BEP file then a second build truncates it →
   integration test asserts both builds' rows land and no stall (the restart fires).

5. **Registry live-feed integration.** Hook `Progress/Configured/Action/TestResult/Started/
   Finished` into the E2 in-memory registry + WS `StreamEvents`.
   **✓CP:** a running fake build shows live progress/action counts in `ls --json` and on the WS.

6. **`GET /metrics` (+ list form) handler.** Wire store → JSON shape §2.6 into E2 mux with
   auth.
   **✓CP:** `curl -H 'Authorization: Bearer …' '/metrics?invocation_id=<id>'` returns the shape;
   `404` on unknown; list form paginates.

7. **Low-hit alert logic.** Finalizer computes ratio vs floor + trailing baseline; writes
   `alert`; emits WS alert event.
   **✓CP:** synthetic low-hit metrics → `alert:"low_cache_hit"`; sudden drop vs seeded history
   → `"cache_busting_suspected"`.

8. **Perfetto serving + postMessage shim.** `GET /profile/{id}/{name}` (traversal-safe) +
   `GET /perfetto/{id}` shim page (embed.FS) + `perfetto_url` in `/metrics` (§2.9, Option B).
   **✓CP:** `curl` the served gz returns valid gzip bytes; the shim page loads; opening
   `perfetto_url` in a browser auto-loads the trace via postMessage (manual once; scripted check
   that `/profile/...` returns a valid gzip Chrome-trace and the shim HTML references
   `ui.perfetto.dev` + `postMessage`).

9. **Disk-cache report job.** Periodic + on-demand `GET /diskcache`; rows in
   `disk_cache_reports`.
   **✓CP:** point at a fixture dir → report row matches `du`-computed size/count.

10. **Disk-cache GC.** Wire Bazel-native flag check (report which mode is active) + opt-in
    direct LRU prune with the §2.8 safety guards.
    **✓CP:** fixture dir over cap → prune frees down to `target*hysteresis`, respects
    `min_age`, records `gc_freed_bytes`; with a "build in progress" flag set, prune is skipped.

11. **End-to-end real-bazel verification + `/verify` recipe + CLAUDE.md.** (§5)
    **✓CP:** two real worktree builds → second shows high hit ratio matching Bazel's summary
    line; `make verify-e4` green.

---

## 4. Interfaces & contracts

**Inbound from E1 — BEP file path convention (ALREADY LOCKED by E1; reproduced here for E4).**
E1 (`E1-cache-config.md` §2.4, §4) is the authoritative owner and has fixed:
- `--build_event_json_file=.bazel-broker/bep.json` (relative → `<worktree>/.bazel-broker/bep.json`)
- `--profile=.bazel-broker/command.profile.gz` (relative → `<worktree>/.bazel-broker/command.profile.gz`)

Both are **per-worktree, relative, reused across builds in that worktree**. E1 guarantees exactly
one writer per worktree at a time (per-output-base exclusive lock) and explicitly **declines
per-invocation filenames**, directing E4 to keep history in SQLite instead (E1 OD-1). The **join
key is NOT in the filename** — E4 derives the path from `{worktree}` (registry/discovery) and binds
the `invocation_id` from the stream's own `BuildStarted.uuid`. Because the file is reused, E4's
truncation supervisor (§2.2) is **required**. All paths use E1's `pwd -P`-resolved worktree so the
string matches what E3's process discovery reports. (D-E4-2 below is therefore *not* "should E1 go
per-invocation" — E1 said no; it is only "does E4 ever need per-invocation history, and if so does
E5's wrapper mint it" — see §6.)

**Inbound from E2 — registry + store + mux.** E4 registers BEP tailers off registry lifecycle
(build-appears → `Manager.Watch`; build-finalized → `Manager.Stop`), writes through the E2
SQLite store (adds schema v2), and mounts handlers on the E2 HTTP mux behind the existing
loopback+bearer-token auth. E4 introduces **no new transport** — it extends E2's.

**Inbound from E2/E5 — invocation→path binding.** The mapping `invocation_id → {bep_path,
profile_path, worktree}` comes from `Register` (active, E5 wrapper) or discovery (passive, E3).
E4 consumes it; it does not own discovery. If only a path is known (passive, pre-`BuildStarted`),
E4 binds the id from the stream's own `BuildStarted.uuid` once it arrives.

**Outbound to E6/E7/E8 — metrics rows + endpoints.** This epic is the producer for:
- `GET /metrics?invocation_id=` and `GET /metrics` (list) — JSON §2.6 — consumed by `brokerctl` (E6),
  web dashboard (E7), menu-bar app (E8).
- WS events on E2's `StreamEvents`: incremental `metrics`/`alert`/`progress` events.
- `GET /diskcache` — size report — for E7/E8 cache panels.
- `GET /profile/{id}/{name}` + the `perfetto_url` field — consumed by `brokerctl profile <id>`
  (E6) and the UIs' "open in Perfetto" buttons (E7/E8).

**Outbound to E2 API table (§6).** E4 fulfills the architecture's `Metrics(invocation_id)` API
row by filling E2's **reserved** `GET /metrics` route (currently a `501` stub) with the
`?invocation_id=` param E2 already declared — E4 does **not** add a new route, it implements the
seam. E4 also adds `GET /diskcache`, `GET /profile/{id}/{name}`, and `GET /perfetto/{id}`, which
are *new* routes E2 did not reserve → **flag to E2's owner** so the route table and auth posture
stay the single source of truth.

---

## 5. Testing & verification

Per architecture §10, every piece is launchable headlessly + observable + fast.

**Unit (sub-second):**
- Parser: `testdata/bep/*.ndjson` (one captured real stream + several hand-crafted edge
  streams: truncated mid-line, missing `BuildMetrics`, `actions_created==0`, failed build).
- Metrics extraction: assert derived `cache_hit_ratio`, per-mnemonic rows, timing fields.
- Alert logic: synthetic histories → expected `alert` value.
- GC: fixture cache dir → prune respects target/hysteresis/min_age; report matches `du`.

**`fake-bazel` synthetic BuildMetrics (extends E0's stub).** Add a mode to
`testdata/fake-bazel.sh` (or a Go `cmd/fake-bazel` helper) that, honoring
`--build_event_json_file`, emits a realistic sequence: `BuildStarted{uuid}` →
`Progress` → `Configured` → a few `ActionExecuted` → a `BuildMetrics` whose `ActionSummary`
carries **parameterized `runner_count[]`** (e.g. N disk-cache-hits + M sandbox + K internal, so we
can script any hit ratio *via the same field the real derivation reads*) plus the matching
`actions_created/executed` → `BuildFinished{exit_code}` → `last_message`. It must also emit the
human `summary_line` consistent with the `runner_count` so the cross-check test is meaningful.
Knobs via env: `FAKE_DISK_HITS`, `FAKE_SANDBOX`, `FAKE_INTERNAL`, `FAKE_ACTIONS_CREATED`,
`FAKE_ACTIONS_EXECUTED`, `FAKE_EXIT_CODE`, `FAKE_TRUNCATE_THEN_REBUILD=1`. ⚠️ The fake MUST drive
the ratio through `runner_count`, not by abusing `created−executed`, or the test would validate
the wrong (discarded) formula. This turns "Metrics matches expected ratio" and "truncation
handled" into ~1s checks.

**Integration (fake bazel, no real iOS build):**
1. Start broker (E2). 2. Register a fake build (worktree → derived bep path per E1). 3. Run the
fake-bazel stub writing `<worktree>/.bazel-broker/bep.json`. 4. Assert: `ls --json` shows live
progress; after finish, `/metrics?invocation_id=` returns the expected ratio + counts; WS emitted a
`metrics` event; second run with `FAKE_TRUNCATE_THEN_REBUILD=1` (same path, truncate-in-place)
produces a second clean row (the supervisor restart fires, no stall).

**Real end-to-end (occasional, `testdata/workspace/` + 2–3 worktrees, E0).** Build the trivial
target in worktree A (cold), then in worktree B (warm via shared disk cache). Assert:
- B's `/metrics` `cache.hit_ratio` is high and the `runner_count`-derived `disk_cache_hits`
  **equals exactly** the count parsed from `cache.summary_line` (they are two encodings of the same
  number — assert equality, not "within tolerance"). This is the "matches Bazel's own cache summary"
  acceptance criterion and the live check for D-E4-3.
- `perfetto_url` (the shim page) opens and Perfetto **auto-loads** the trace via postMessage,
  showing a critical path (manual once; scripted check that `/profile/...` returns a valid gzip
  Chrome-trace and the `/perfetto/{id}` shim HTML references `ui.perfetto.dev` + `postMessage`).

**`make verify-e4` recipe (CLAUDE.md):**
```
make build
make protos            # only if protos changed; otherwise generated files are committed
broker &               # or launchd
WT=$(mktemp -d)        # fake worktree; E1 BEP path is $WT/.bazel-broker/bep.json
brokerctl register --worktree "$WT" --id TESTID
mkdir -p "$WT/.bazel-broker"
FAKE_DISK_HITS=920 FAKE_SANDBOX=70 FAKE_INTERNAL=10 \
FAKE_ACTIONS_CREATED=1000 FAKE_ACTIONS_EXECUTED=80 \
  testdata/fake-bazel.sh --build_event_json_file="$WT/.bazel-broker/bep.json"
# processes_total = 920+70+10 = 1000 ; disk hits = 920 ; hit_ratio = 0.92
curl -fsS -H "Authorization: Bearer $TOK" 'http://127.0.0.1:'$PORT'/metrics?invocation_id=TESTID' \
  | jq -e '.cache.hit_ratio > 0.9 and .cache.disk_cache_hits == 920'
# profile bytes are valid gzip; shim page references perfetto + postMessage
curl -fsS -H "Authorization: Bearer $TOK" \
  'http://127.0.0.1:'$PORT'/profile/TESTID/command.profile.gz' | gunzip -t
curl -fsS -H "Authorization: Bearer $TOK" \
  'http://127.0.0.1:'$PORT'/perfetto/TESTID' | grep -iE 'ui.perfetto.dev.*postMessage|postMessage'
```

**Acceptance criteria (from E4 "Done when"):**
- [ ] A real build streams progress into `ls` live.
- [ ] After it finishes, `Metrics` returns the cache-hit ratio **matching Bazel's own summary**.
- [ ] The Perfetto link **auto-loads** the profile.
Plus internal gates: truncation produces clean per-build rows; low-hit alert fires on a seeded
drop; disk-cache report matches `du`; GC respects safety guards.

---

## 6. Risks, edge cases, open decisions

**Risks / edge cases:**
- **R1 — Proto drift across Bazel versions.** `BuildMetrics` fields are added/renamed over
  Bazel releases (e.g. cache-stat fields moved between versions). Mitigation: pin protos to the
  in-use Bazel version, `protojson.UnmarshalOptions{DiscardUnknown:true}`, store
  the raw `BuildMetrics` blob in E2's `metrics.json` column for fields we lack columns for, and a
  `make protos` bump procedure. A
  mismatched proto silently dropping a field is the top correctness risk → covered by R1 test
  (decode a real captured stream).
- **R2 — Truncation is CERTAIN, not a race (given E1's locked per-worktree reuse).** Every
  rebuild in a worktree truncates `bep.json` in place; naive `nxadm/tail` stalls past EOF; partial
  lines from a mid-write read fail `protojson`. This is a guaranteed code path, not an edge case.
  Mitigation: the **mandatory** inode/size restart supervisor (§2.2) + per-line skip-on-error +
  `Poll:true` on macOS, with per-stream-state reset and prior-row finalization on restart.
- **R3 — Cache-hit metric semantics (D-E4-3) — the formula was WRONG and is now fixed.** The
  earlier `actions_created − actions_executed` is invalid: a disk-cache hit is counted *in*
  `actions_executed` (served over the remote-cache protocol), and `actions_created` includes
  never-executed actions. E4 now derives disk/remote-cache hits from `ActionSummary.runner_count[]`
  (the structured form of Bazel's "N processes: …" line), stores the raw breakdown, and cross-checks
  exactly against the summary string. Residual open item is only which derived ratio E6/E7/E8
  *display* — escalate, don't bake in.
- **R4 — GC vs concurrent writers.** Deleting a blob a running build references causes at worst a
  *recoverable miss* (POSIX unlink doesn't corrupt open fds; Bazel writes via temp+rename). Real
  imprecision: `atime` LRU is unreliable on macOS (use `mtime`). Mitigation: prefer Bazel-native
  GC; direct GC opt-in only; `min_age` floor; advisory lock; skip while any build targets the cache.
  Open: ship report-only until native GC covers all pinned versions? (D-E4-4)
- **R5 — Perfetto cannot fetch `http://localhost` via `?url=` — corrected.** The earlier claim that
  ui.perfetto.dev's `?url=` allows localhost origins is **false**; the `?url=` path requires HTTPS
  (Perfetto deep-linking doc). E4 therefore serves the trace via the **postMessage shim** (§2.9,
  Option B), which bypasses HTTPS/CORS. Residual: ui.perfetto.dev is hosted, so an **offline** user
  can't load it — surface a clear error and offer a `chrome://tracing` / local-Perfetto fallback.
  (Whether to also support the HTTPS `?url=` path is D-E4-6.)
- **R6 — Path/traversal in `/profile/`.** Serve only the DB-recorded `profile_path` for the id;
  ignore the URL's path component beyond the id lookup.
- **R7 — Large/long streams.** Progress events can be voluminous; cap the live ring buffer and
  don't persist raw progress text — only metrics.

**Open decisions to lock (do NOT resolve unilaterally — surfaced per architecture §12 style):**
- **D-E4-1:** Proto sourcing — vendor pinned `.proto` files (recommended) vs git-submodule the
  Bazel repo vs depend on an existing published Go BES module. Affects reproducibility + the
  `make protos` story.
- **D-E4-2 (REFRAMED — E1 already decided its side):** E1 has *locked* a per-worktree relative
  BEP/profile path and **declined per-invocation filenames** (E1 §2.4/§4, OD-1). So this is no
  longer "ask E1 to go per-invocation." The remaining open question is narrower: **does E4 need
  on-disk per-invocation history at all** (vs SQLite rows + the single live file), and if so, is it
  E5's wrapper that mints per-invocation paths — *not* E1? Lock with E1+E5 owners. The truncation
  supervisor ships regardless.
- **D-E4-3 (REFRAMED — the wrong formula is removed):** the displayed "cache hit %" definition —
  `runner_count` disk-hits / processes (recommended) vs including remote hits vs the
  `action_cache_statistics` ratio. Raw inputs are stored either way; this only picks the headline
  number E6/E7/E8 render. (The discarded `created − executed` formula is no longer a candidate.)
- **D-E4-4:** GC mechanism — drive Bazel's `--experimental_disk_cache_gc_max_size`
  (recommended, race-safe) vs broker-side direct LRU prune vs **report-only (recommended interim)**.
  Gated on the in-use Bazel version supporting the flag; do not enable direct GC by default.
- **D-E4-5 (carried from §12 D1):** is broker SQLite + Perfetto enough, or do we later add a
  richer BEP UI (Buildbarn bb-portal)? E4 keeps it self-contained; this is a future fork.
- **D-E4-6 (NEW):** Perfetto integration — **postMessage shim** (recommended; works over plain
  loopback http, §2.9 Option B) vs serving HTTPS so the `?url=` deep-link works (heavier: needs a
  browser-trusted localhost cert). Also: offline fallback (`chrome://tracing` / local Perfetto)
  since ui.perfetto.dev is hosted. Lock before E7/E8 wire their "open in Perfetto" buttons.
- **D-E4-7 (NEW — frozen-contract touch):** E4 adds routes E2 did not reserve (`GET /diskcache`,
  `GET /profile/{id}/{name}`, `GET /perfetto/{id}`) and must use E2's declared `?invocation_id=`
  param (not `?id=`). E2's route table is "FROZEN"; get E2's owner to ratify the additions so the
  API table stays the single source of truth.

---

## 7. Effort & internal ordering

**Critical path:** proto codegen (task 1) → parser (2) → store (3) → tailer (4) gate
everything; they must land first and in order. After the store + tailer exist, the rest
parallelizes.

| Tasks                         | Rough effort | Parallelizable? |
|-------------------------------|--------------|-----------------|
| 1 Protos + codegen            | 0.5–1 d      | no (blocks all) |
| 2 Parser + extraction         | 1 d          | after 1         |
| 3 SQLite schema v2 + store    | 0.5 d        | parallel w/ 2   |
| 4 Tailer + truncation         | 1–1.5 d      | after 2,3       |
| 5 Registry live-feed          | 0.5 d        | after 4         |
| 6 `/metrics` endpoint         | 0.5 d        | after 3         |
| 7 Alert logic                 | 0.5 d        | after 3         |
| 8 Perfetto serving + link     | 0.5 d        | after 3 (indep) |
| 9 Disk-cache report           | 0.5 d        | independent     |
| 10 GC (+ safety)              | 0.5–1 d      | after 9         |
| 11 E2E verify + CLAUDE.md     | 0.5 d        | last            |

**Suggested order:** 1 → (2 ∥ 3) → 4 → 5 → (6 ∥ 7 ∥ 8 ∥ 9) → 10 → 11.
**Total:** ~6–8 engineer-days. **Recommended slice for first reviewable PR:** tasks 1–6
(decode + persist + serve `/metrics` over fake-bazel) — that alone satisfies the core of E4's
"Done when" minus the real-build cross-check and Perfetto, which follow in a second PR (7–11).

Before starting: the BEP path convention is **already locked by E1** (per-worktree, reused) — so
task 4's truncation supervisor is in-scope and non-negotiable. Instead, **pre-lock D-E4-3** (which
derived hit % E6/E7/E8 display) and **D-E4-6** (Perfetto postMessage shim vs HTTPS `?url=`) before
the metrics shape and the `perfetto_url` field are consumed downstream.

---

## Staff Engineer Review

**(a) Verdict: APPROVE WITH REQUIRED CHANGES — now revised to NEEDS-NOTHING-BLOCKING.**
The epic's architecture (tailer → protojson parser → SQLite → `/metrics` + Perfetto) is sound and
well-sequenced. But the original draft shipped a **materially wrong cache-hit formula** and a
**Perfetto integration that would not load**, plus three cross-epic contract drifts (BEP path,
SQLite table, route/param names). All are corrected in place below. With these, E4 is ready to
implement; the remaining items are framed decisions to escalate, not design holes.

**(b) Top findings**
1. **Cache-hit derivation was wrong (highest severity).** `cached = actions_created −
   actions_executed` is invalid per the proto's own field docs: a `--disk_cache` hit is counted
   *in* `actions_executed` (served via the remote-cache protocol — "includes any remote cache
   hits"), and `actions_created` includes never-executed actions. The formula both inverts and
   contaminates the signal. **Authoritative source is `ActionSummary.runner_count[]`** (the
   structured "N processes: X disk cache hit…" line). Rewrote §2.4, schema, JSON, alert logic,
   fake-bazel, and the verify assertions around it.
2. **E1↔E4 BEP-file contract was stale and self-contradictory.** E1 (authoritative, ships first)
   has *locked* a per-worktree **relative reused** path `<worktree>/.bazel-broker/bep.json` and
   *declined* per-invocation filenames. E4 repeatedly "recommended" per-invocation as the fix for
   truncation — directly contradicting the locked decision. Corrected throughout: truncation
   supervisor is **mandatory**, the join key is `BuildStarted.uuid` (not the filename), and D-E4-2
   is reframed to the genuinely-open question (does E4/E5 need on-disk history).
3. **Perfetto `?url=` won't work from localhost.** Perfetto's deep-linking requires **HTTPS** for
   `?url=`; the "localhost is allowlisted" claim is unsubstantiated. Replaced with the documented
   **postMessage shim** (Option B) that bypasses HTTPS/CORS, served by the broker over loopback
   http; added D-E4-6 + offline fallback.
4. **SQLite schema collided with E2's frozen reserved table.** E2 ships an empty `metrics` table
   (FK to `builds`, `user_version=2` to *populate*). E4 invented a parallel `build_metrics` table.
   Reconciled to **`ALTER TABLE metrics ADD COLUMN …`**, mapping E2's `actions_total/actions_cached/
   wall_ms/json` onto the right fields; added a `metrics_runner_counts` table; per-mnemonic table
   now captures `actions_created` too (proto field 7, previously dropped).
5. **API param/route drift vs E2's frozen contract.** E4 used `GET /metrics?id=`; E2 declared
   `?invocation_id=`. Standardized on E2's name; flagged the new routes (`/diskcache`, `/profile`,
   `/perfetto`) for E2-owner ratification (D-E4-7).
6. **Codegen gaps.** Import list omitted `google/protobuf/any.proto`; M-mapping framing risked
   M-mapping well-known types (breaks). Corrected, plus a note to re-derive imports from the pinned
   Bazel tag and `go vet` the generated package.
7. **GC safety over-claimed "corruption."** POSIX unlink doesn't corrupt open fds and Bazel writes
   temp+rename; the real risks are recoverable misses and unreliable `atime` LRU on macOS.
   Tightened to `mtime`, report-only-by-default recommendation.

**(c) What I changed (all in place, 7-section structure preserved)**
- §1: deliverable 4 now points at `runner_count` derivation + the E2 `metrics` table.
- §2.1: corrected proto imports (`any.proto`), hardened M-mapping guidance + verification.
- §2.2: rewrote BEP-path contract to match E1's locked decision; truncation supervisor made
  mandatory; added drain-before-restart and prior-row finalization.
- §2.4: replaced the wrong formula with `runner_count`-based derivation; corrected field-level
  semantics (deprecated `remote_cache_hits`, inline `action_cache_statistics`, ActionData fields).
- §2.5: reconciled schema with E2's reserved `metrics` table via `ALTER TABLE`; added
  `metrics_runner_counts`; fixed indexes/FKs; noted the builds-FK ordering.
- §2.6: `?invocation_id=` (E2 frozen param); JSON cache block reflects `runner_counts`.
- §2.7: alert ratio uses the corrected derivation.
- §2.9: Perfetto postMessage shim replaces the non-working `?url=`; shim route + JSON field.
- §3/§5: tasks 8 + fake-bazel + `make verify-e4` exercise `runner_count`, the shim, and E1's path.
- §6: R2/R3/R4/R5 rewritten; D-E4-2/3/4 reframed; D-E4-6/D-E4-7 added.

**(d) Decisions / risks to escalate (do NOT let an implementer pick silently)**
- **D-E4-3** — which derived "cache hit %" E6/E7/E8 display (disk-only vs disk+remote vs
  action-cache-stats ratio). Raw inputs stored regardless; pick the headline before UIs consume it.
- **D-E4-2 / OD-1 (E1)** — whether E4 needs on-disk per-invocation history; if yes, E5's wrapper
  mints it (E1 will not). Confirm with E1 + E5 owners.
- **D-E4-4** — GC mode; recommend **report-only** until Bazel-native GC covers every pinned Bazel
  version. Never enable direct LRU GC by default.
- **D-E4-6** — Perfetto postMessage shim (recommended) vs HTTPS `?url=`; plus offline fallback.
- **D-E4-7** — E2-owner ratification of E4's new routes + the `?invocation_id=` param, since E2's
  route table is declared frozen.
- **R1** — proto pin: codegen must be regenerated from the *exact* in-use Bazel tag; a silently
  dropped field is the top residual correctness risk. Keep the captured-real-stream decode test.
