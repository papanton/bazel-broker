# E2 — Broker daemon core (/goal)

> The always-on, headless control-plane daemon. Defines the in-memory build registry,
> the SQLite-backed store, and the **localhost HTTP+WS API that every other epic
> (E3/E4/E5/E6/E7/E8) consumes**. This document is the authoritative API contract.

Status: **Plan v2 (contract frozen)** · Epic: E2 · Depends on: E0 · Maps to: M1 · Owner: Antonis
Target: macOS arm64, Go 1.26. Tech: `net/http` + `coder/websocket` + `modernc.org/sqlite` + `slog`.

> **§4 is the FROZEN cross-epic API contract.** During review, E3–E8 were each found to have
> independently *guessed* divergent route paths, JSON field names, the shared Go package name,
> and the WS event envelope. Those guesses are **superseded by §4 of this document** — every
> consumer epic must conform to the shapes frozen here. The specific reconciliations are listed
> in §4.6 ("Reconciliation notes — what consumers must change") and in the Staff Engineer Review
> at the end. Two questions are left genuinely open and escalated, not decided: **D-stack-2**
> (TCP+token vs Unix socket) and **browser auth for E7** (cookie vs token-in-page vs loopback
> exemption).

---

## 1. Goal & scope recap

**Goal (from 02-epics.md E2):** the always-on control-plane process with an observable
localhost API (architecture C2).

**In scope for this epic:**
- A long-lived Go daemon (`cmd/broker`) managed by `launchd` (KeepAlive, RunAtLoad).
- A **Build data model** and an **in-memory registry** that is the source of truth for "what
  is building right now", mirrored to a **SQLite store** (schema v1) for durability across
  daemon restarts and for recent-build history.
- A **loopback-only HTTP + WebSocket API** on `127.0.0.1:PORT` guarded by a **bearer token**:
  - Live, implemented here: `GET /healthz`, `GET /builds`, `WS /events`, `POST /register`,
    `POST /deregister`.
  - **Reserved (routed, but return `501 Not Implemented` until their owning epic lands):**
    `POST /kill` (E3), `POST /admission` (E5), `GET /metrics` (E4), `POST /drain` + `POST /resume` (E5).
- A **config file** at `~/.config/bazel-broker/config.json` (port, token, disk_cache, max_concurrency).
- **Structured logging** via `slog` to a known file path, and **graceful shutdown**.

**Explicitly out of scope (owned by later epics — this epic only reserves the seams):**
- Process discovery / `libproc` / actually killing a PID → **E3**.
- BEP tailing, metrics extraction, Perfetto serving → **E4**.
- Admission policy engine (semaphore, token bucket, stagger), the `tools/bazel` wrapper → **E5**.
- The `brokerctl` CLI → **E6**; web dashboard → **E7**; menu-bar app → **E8**.

**Dependency posture:** E2 depends only on **E0** (repo scaffolding: `go.mod`, `Makefile`,
`testdata/fake-bazel.sh`, `CLAUDE.md`). E3, E4, E6 fan out from E2; E5 sits over E2+E3; E7/E8
are thin clients over the E2 API. **Therefore the data types and JSON shapes in §4 are frozen
contracts** — later epics extend the store and fill in the reserved routes but must not break
these shapes.

---

## 2. Design & implementation details

### 2.1 Package layout (under `cmd/` + `internal/`)

A single Go module (created in E0) holds both `broker` and (later) `brokerctl`. The
daemon-specific code lives under `internal/` so it cannot be imported outside the module,
except the **shared wire types**, which live in a small importable package so `brokerctl`
(E6) and any Go client reuse them.

> **Package name decision (frozen): the shared types package is `internal/api`, not
> `internal/wire`.** E6 and E4 both already assume `internal/api`; E8's Swift `Codable` models
> mirror the same JSON. We standardize on `internal/api` (the name two consumers already wrote
> against) to avoid a rename churn. Inside the module the daemon refers to it as `api`. The
> Go *file* is `internal/api/api.go`. (The earlier draft called this `internal/wire`; that name
> is retired.)

```
bazel-broker/
  cmd/
    broker/
      main.go                 # flag parsing, config load, wiring, launchd-friendly lifecycle
  internal/
    config/
      config.go               # Config struct, Load(), defaults, path resolution, validation
    build/
      build.go                # Build domain struct, State enum, Source enum (the data model)
    registry/
      registry.go             # in-memory registry: thread-safe map + event fan-out
      registry_test.go
    store/
      store.go                # SQLite open/migrate, CRUD; modernc.org/sqlite driver
      schema.sql              # DDL v1 (embedded via //go:embed)
      store_test.go
    httpapi/
      server.go               # http.Server construction, mux, middleware chain
      auth.go                 # bearer-token middleware (constant-time compare)
      handlers.go             # /healthz /builds /builds/{id} /register /deregister
      events.go               # WS /events: coder/websocket hub, per-conn writer pump
      reserved.go             # /builds/{id}/kill /admission /builds/{id}/metrics /metrics /drain /resume → 501 stubs
      handlers_test.go
    api/                      # <-- IMPORTABLE shared types (also used by brokerctl, web, etc.)
      api.go                  # request/response/event JSON structs + enum string consts (frozen §4)
    logging/
      logging.go              # slog.Logger construction (JSON handler → file + stderr)
  deploy/
    com.bazelbroker.broker.plist   # launchd plist template (see §2.8)
    install.sh                      # writes config + plist, launchctl bootstrap (thin helper)
```

> Rationale for `internal/api` vs `internal/build`: `build.Build` is the rich domain object
> (carries non-serialized fields like the `*os.Process` handle E3 will add, internal mutexes,
> tail state E4 will add). `api.Build` is the **flat JSON DTO** sent over the API. Keeping them
> separate prevents the wire contract from accidentally leaking internal fields and lets the
> domain object evolve without breaking clients. A single `build.Build.ToAPI()` mapper bridges them.

### 2.2 The Build data model

`internal/build/build.go`:

```go
package build

import "time"

// State is the lifecycle state of a build. String values are the wire contract.
type State string

const (
    StateQueued    State = "queued"    // admitted-pending (E5); E2 never sets this itself
    StateRunning   State = "running"
    StateFinished  State = "finished"  // exited 0
    StateFailed    State = "failed"    // exited non-zero
    StateKilled    State = "killed"    // terminated by broker (E3)
    StateGone      State = "gone"      // discovered process whose PID vanished, outcome unseen (E3 reap)
    StateUnknown   State = "unknown"   // catch-all / forward-compat for clients
)

// NOTE on `gone` vs `unknown`: E3's reconciler reaps a discovered build whose PID disappeared
// to `gone` (the documented reap target in E3 §2.4). `unknown` is retained as the
// forward-compatible fallback any client maps an unrecognized state string to (E8 decodes
// unknown enum values to `.unknown` rather than crashing). Frozen here so E3 does not invent
// a parallel state value.

// Source records how the build entered the registry.
type Source string

const (
    SourceRegistered Source = "registered" // via POST /register (wrapper or client)
    SourceDiscovered Source = "discovered" // via process discovery (E3)
)

// Build is the in-memory domain object. Fields below the line are NOT serialized to the
// wire DTO and are reserved for later epics.
type Build struct {
    InvocationID string     // Bazel invocation_id (uuid). Primary key. Required.
    Worktree     string     // absolute path of the git worktree (build cwd)
    WorktreeName string     // last path component (display); E3 fills, "" until then
    Targets      []string   // bazel targets/patterns, e.g. ["//app:App"]
    PID          int        // bazel CLIENT pid (0 if unknown at register time)
    State        State
    StartTime    time.Time  // when broker first saw it
    EndTime      time.Time  // zero until terminal
    ExitCode     int        // valid only in terminal states; 0 otherwise
    Source       Source

    // ---- discovery seam (E3 populates; serialized only where noted in §4.1) ----
    ExePath  string    // E3: proc_pidpath — client/server filtering, display
    Cwd      string    // E3: PROC_PIDVNODEPATHINFO — worktree resolution input
    GitDir   string    // E3: resolved .git dir — output-base lookup for D4 Cancel
    LastSeen time.Time // E3: last reconcile pass that saw this PID (reap/staleness)

    // ---- reserved (populated by later epics; never serialized by E2) ----
    // proc      *os.Process // E3: owned handle for SIGINT/SIGKILL
    // bepPath   string      // E4: per-invocation --build_event_json_file
    // profile   string      // E4: --profile gz path for Perfetto
}
```

> **Why these E3 fields are declared in E2 now:** E3 §4.3 explicitly requests E2 *extend* (not
> fork) `build.Build` with `ExePath`, `Cwd`, `GitDir`, `WorktreeName`, `LastSeen`, the `gone`
> state, and the `Upsert`/`ReapMissingDiscovered`/`FindByPID`/`FindByInvocationID` registry
> methods. Declaring the field seams here (even though E2 leaves them empty) keeps the type the
> single source of truth and means E3 adds *behavior*, not *schema*. `FirstSeen` is just
> `StartTime` (renamed in E3's table); E2's `StartTime` is the canonical "first seen" timestamp —
> E3 must reuse it, not add a duplicate `FirstSeen`.

`State` machine (E2 sets only the transitions it owns; E3/E5 add the rest):

```
   (register, source=registered) ─▶ running ─┬─▶ finished   (deregister exit_code=0)
                                             ├─▶ failed     (deregister exit_code!=0)
                                             └─▶ killed      (E3: POST /kill)
   (discovery, source=discovered) ─▶ running ─▶ unknown     (E3: process vanished, outcome unseen)
   (E5) queued ─▶ running                                    (admission)
```

### 2.3 In-memory registry

`internal/registry/registry.go`. Concurrency model: **one `sync.RWMutex` guarding a map**,
plus a non-blocking **fan-out hub** for WS subscribers. Reads (`/builds`, `/healthz`) take the
read lock; mutations (`Register`/`Deregister`/future `Kill`) take the write lock and then emit
an `api.Event` to subscribers.

```go
package registry

type Registry struct {
    mu     sync.RWMutex
    builds map[string]*build.Build // keyed by InvocationID
    store  *store.Store            // write-through persistence
    hub    *Hub                    // WS fan-out
    log    *slog.Logger
    clock  func() time.Time        // injectable for tests
}

func New(s *store.Store, hub *Hub, log *slog.Logger) *Registry

// Register inserts or upserts a build (idempotent on InvocationID). Returns the stored Build.
// If the InvocationID already exists in a non-terminal state, it is treated as a heartbeat/update.
func (r *Registry) Register(b *build.Build) (*build.Build, error)

// Deregister marks a build terminal (finished/failed) by exit code and stamps EndTime.
// Idempotent: deregistering an unknown or already-terminal id is a no-op success.
func (r *Registry) Deregister(invocationID string, exitCode int) (*build.Build, error)

// Snapshot returns a copy of all builds (active + recent), newest StartTime first.
func (r *Registry) Snapshot() []*build.Build

// Get returns one build by id (ok=false if absent).
func (r *Registry) Get(invocationID string) (*build.Build, bool)

// Counts returns live aggregate counts for /healthz.
func (r *Registry) Counts() Counts // {Building, Queued, Total int}

// HydrateFromStore loads the most recent N terminal builds from SQLite into the in-memory map
// at boot so /builds and the WS snapshot are continuous across daemon restarts.
func (r *Registry) HydrateFromStore(n int) error

// ---- E3 reconciliation seams (declared here so the registry stays the single owner of
// concurrency; E3 fills the bodies in its T5). Signatures frozen with E3 §4.3. ----

// Upsert merges a discovered build without clobbering a richer registered record (dedupe
// priority: by PID, then by InvocationID, then insert as discovered). Source precedence:
// a discovered pass must never downgrade SourceRegistered.
func (r *Registry) Upsert(b *build.Build) (*build.Build, error)

// ReapMissingDiscovered marks discovered builds whose PID is absent from `seen` as StateGone.
// It must NOT touch SourceRegistered builds (their lifecycle is Deregister, E5).
func (r *Registry) ReapMissingDiscovered(seen map[int]bool, now time.Time)

// FindByPID / FindByInvocationID back the POST /builds/{id}/kill lookup (E3) and admission
// reaper (E5). ok=false if absent.
func (r *Registry) FindByPID(pid int) (*build.Build, bool)
func (r *Registry) FindByInvocationID(id string) (*build.Build, bool)
```

> Note: E3 §2.4 sketches an `UpsertSpec` struct argument; E2 freezes the canonical signature as
> `Upsert(*build.Build)` (matching `Register`/`Deregister`). E3 may build `*build.Build` from its
> `ProcInfo` at the call site. PID type is `int` (matching `build.Build.PID` and the wire DTO);
> E3's `int32` from `proc_listpids` is narrowed at the procscan boundary, not carried into the
> registry/API.

**Retention:** the in-memory map keeps **all builds for the current process lifetime** plus
the most recent N terminal builds loaded from the store on startup (default N=200, configurable
later). A background sweeper is NOT in E2 scope; recent-build pruning is deferred. On `/builds`
the registry returns its in-memory view (which was hydrated from SQLite at boot — see §2.4).

**The Hub (WS fan-out):**

```go
type Hub struct {
    mu   sync.Mutex
    subs map[*subscriber]struct{}
}
type subscriber struct{ ch chan api.Event } // buffered (cap 64); slow consumers are dropped+closed

func (h *Hub) Subscribe() (*subscriber, func()) // returns sub + unsubscribe
func (h *Hub) Broadcast(ev api.Event)           // non-blocking send; drop on full buffer
```

Slow-client policy: if a subscriber's buffer is full, the hub **drops the connection** (closes
the channel) rather than blocking registry mutations. Clients reconnect and re-fetch `/builds`
to resync — the `snapshot` event on connect (§2.6) makes that lossless-enough.

### 2.4 SQLite store (`modernc.org/sqlite`, pure Go)

`internal/store/store.go`. Opened with the pure-Go driver (no cgo → trivial cross-compile and
static binary). DSN sets WAL + busy timeout to tame the single-writer constraint.

```go
import _ "modernc.org/sqlite"

const dsn = "file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)"

type Store struct {
    db  *sql.DB
    log *slog.Logger
}

func Open(path string, log *slog.Logger) (*Store, error) // mkdir -p, open, migrate to v1
func (s *Store) Close() error

func (s *Store) UpsertBuild(b *build.Build) error
func (s *Store) MarkTerminal(invocationID string, state build.State, exit int, end time.Time) error
func (s *Store) RecentBuilds(limit int) ([]*build.Build, error) // newest first, for boot hydration
func (s *Store) GetBuild(invocationID string) (*build.Build, bool, error)
```

**Single-writer discipline:** `modernc.org/sqlite` permits concurrent reads but SQLite has one
writer. We set `db.SetMaxOpenConns(1)` for the **write path** by funneling **all writes through
the registry's write-lock** (so the app-level mutex already serializes writers), and allow a
small read pool. Simplest correct approach for v1: a **single `*sql.DB` with `SetMaxOpenConns(1)`**
+ WAL + `busy_timeout(5000)`. This eliminates `SQLITE_BUSY` at the cost of serializing reads
behind writes — acceptable at this traffic level (a handful of builds, human/agent-driven). See
§6 for the open decision on relaxing this.

`internal/store/schema.sql` (embedded via `//go:embed schema.sql`), **DDL v1**:

```sql
-- schema v1
PRAGMA user_version = 1;

CREATE TABLE IF NOT EXISTS builds (
    invocation_id TEXT PRIMARY KEY,
    worktree      TEXT NOT NULL,
    targets       TEXT NOT NULL DEFAULT '[]',  -- JSON array of strings
    pid           INTEGER NOT NULL DEFAULT 0,
    state         TEXT NOT NULL,               -- queued|running|finished|failed|killed|unknown
    start_time    INTEGER NOT NULL,            -- unix epoch milliseconds
    end_time      INTEGER NOT NULL DEFAULT 0,  -- 0 = not ended
    exit_code     INTEGER NOT NULL DEFAULT 0,
    source        TEXT NOT NULL                -- registered|discovered
);

CREATE INDEX IF NOT EXISTS idx_builds_state      ON builds(state);
CREATE INDEX IF NOT EXISTS idx_builds_start_time ON builds(start_time DESC);

-- Reserved for E4 (declared now so the schema file is the single source of truth, but no
-- E2 code reads/writes it). Created empty; E4 migrates to user_version=2 to populate.
CREATE TABLE IF NOT EXISTS metrics (
    invocation_id   TEXT PRIMARY KEY REFERENCES builds(invocation_id) ON DELETE CASCADE,
    actions_total   INTEGER,
    actions_cached  INTEGER,
    cache_hit_ratio REAL,
    wall_ms         INTEGER,
    json            TEXT          -- raw BuildMetrics blob
);
```

Migration strategy: `Open()` reads `PRAGMA user_version`; if `0`, executes `schema.sql` and the
file's `PRAGMA user_version = 1` stamps it. Future epics add numbered migration steps keyed off
`user_version` (E4 → 2, etc.). v1 ships the `metrics` table empty so E4 only needs to populate,
not restructure.

Times are stored as **unix epoch milliseconds (INTEGER)**; the Go layer converts to/from
`time.Time`. The wire layer (§4) emits **RFC3339 UTC strings** for human/tool friendliness.

### 2.5 Config (`~/.config/bazel-broker/config.json`)

`internal/config/config.go`:

```go
type Config struct {
    Host           string `json:"host"`            // loopback host, default "127.0.0.1" (clients read this)
    Port           int    `json:"port"`            // loopback TCP port, default 8765
    Token          string `json:"token"`           // bearer token; generated if absent
    ProfileOpen    string `json:"profile_open"`    // "perfetto" | "chrome-tracing"; read by E6/E8, default "perfetto"
    DiskCache      string `json:"disk_cache"`      // shared --disk_cache path (consumed by E1/E5)
    MaxConcurrency int    `json:"max_concurrency"` // admission ceiling (consumed by E5)
    DBPath         string `json:"db_path"`         // SQLite file; default ~/.local/state/bazel-broker/broker.db
    LogPath        string `json:"log_path"`        // slog JSON sink; default ~/.local/state/bazel-broker/broker.log
}

// Load resolves the config path (override via $BAZEL_BROKER_CONFIG, else
// $XDG_CONFIG_HOME/bazel-broker/config.json, else ~/.config/bazel-broker/config.json),
// reads JSON, applies defaults, and validates. If the file is absent, Load writes a default
// config (with a freshly generated 32-byte hex token) and returns it — first-run bootstrap.
func Load() (*Config, error)

func Default() *Config                 // defaults with empty token
func (c *Config) Validate() error      // port in range, token non-empty, paths absolute-ish
func ConfigPath() string               // resolved path
```

Example written on first run:

```json
{
  "host": "127.0.0.1",
  "port": 8765,
  "token": "b9f1c0e2a7d4...32bytes-hex",
  "profile_open": "perfetto",
  "disk_cache": "/Users/antonis/.cache/bazel-disk",
  "max_concurrency": 2,
  "db_path": "/Users/antonis/.local/state/bazel-broker/broker.db",
  "log_path": "/Users/antonis/.local/state/bazel-broker/broker.log"
}
```

Token generation: `crypto/rand` → 32 bytes → hex. File written `0600`. The directory is created
`0700`. (D-stack-2: TCP+token is the recommended transport — see §6.)

> **`host` is advisory for clients, not a bind override.** The daemon **always** binds
> `127.0.0.1` (§2.6) regardless of this field; `host` exists so `brokerctl` (E6) and the
> menu-bar app (E8) — which both read `host` from this file — construct the base URL without
> hard-coding `127.0.0.1`. We do not honor an arbitrary `host` for *binding* (that would defeat
> the loopback-only guarantee and D-stack-2). If D-stack-2 later flips to a Unix socket, this
> field becomes a socket path and clients switch transports; the field is the seam.
>
> **Config ownership (frozen): E2 is the sole writer of `config.json`.** E6 and E8 read it and
> MUST NOT write it (E6 §4.1 already states "E6 never writes this file" — confirmed). E8 reads
> `host`/`port`/`token`; E6 additionally reads `profile_open`. E2 writes all fields with
> defaults on first run so a clean install yields a config every consumer can parse. Re unknown
> fields: E6's OD-2 (strict vs lenient JSON decode) is resolved here as **lenient** — clients
> MUST ignore unknown keys so E2 can add config fields without breaking older clients.

### 2.6 HTTP+WS server

`internal/httpapi/server.go`. Bound **only to loopback** — `net.Listen("tcp", "127.0.0.1:PORT")`
(never `:PORT`, which would expose it on all interfaces).

```go
type Server struct {
    cfg  *config.Config
    reg  *registry.Registry
    hub  *registry.Hub
    log  *slog.Logger
    http *http.Server
}

func New(cfg *config.Config, reg *registry.Registry, hub *registry.Hub, log *slog.Logger) *Server
func (s *Server) Routes() http.Handler   // builds the mux + middleware
func (s *Server) Serve(ln net.Listener) error
func (s *Server) Shutdown(ctx context.Context) error
```

Middleware chain (outer→inner): `recoverer` (panic→500+log) → `requestLog` (slog: method, path,
status, dur) → `auth` (bearer). `/healthz` is **exempt from auth** so health probes don't need
the token; everything else requires it.

**Auth** (`auth.go`): expects `Authorization: Bearer <token>`; compares with
`subtle.ConstantTimeCompare`. Missing/blank/wrong → `401` with body
`{"error":"unauthorized"}`. WS `/events` also requires the header (Go clients — `brokerctl` E6,
the menu-bar app E8 — set it on the upgrade `DialOptions.HTTPHeader`; this works because they are
not browsers).

**Browser auth (E7) is the one genuinely open auth question — escalated, not decided here.**
A browser page (E7) cannot read `config.json`, and the browser `WebSocket` API cannot set an
`Authorization` header on the upgrade. E7 §2.4 lays out three strategies (A: same-origin session
cookie that E2's middleware also accepts; B: token templated into the page + token-on-WS-URL;
C: loopback-origin exemption). **A and C both require a change to *this* auth middleware**, so
the decision is co-owned by E2 and E7 and must be made before E7's T5. The middleware is
therefore written with a small `authMode` seam so cookie-acceptance (Option A) can be added
without restructuring. **E2 ships bearer-only (the recommended default for the non-browser
clients); the browser-auth extension is a tracked follow-up flagged in §6 and the Staff
Engineer Review.** Whichever option is chosen, the `POST` mutation routes
(`/builds/{id}/kill`, admission) need a CSRF defense under a cookie scheme (E7 OD-2).

**WS hub** (`events.go`) using `coder/websocket`:

```go
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
    c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
        OriginPatterns: []string{"127.0.0.1:*", "localhost:*"}, // see browser-auth note in §2.6
    })
    // 1. send {"type":"snapshot","builds":[...],"seq":0,"ts":...} immediately (resync contract)
    // 2. subscribe to hub; pump api.Event JSON frames (type=="build") until ctx done / write error
    // 3. heartbeat: ping every 30s (WS ping frame, not a JSON event); drop on failure
}
```

On connect the server sends a **`snapshot` event** (current `/builds` payload) so a freshly
connected UI is immediately consistent, then streams incremental events. This makes the
drop-on-slow-consumer policy (§2.3) safe and resolves E6's OD-5 / E8's `snapshot` expectation:
**the snapshot-on-connect frame is a hard guarantee, not optional.**

> **Event taxonomy (frozen — resolves the four divergent guesses in E6/E7/E8).** There are
> exactly **two** envelope `type` values: `snapshot` (once, on connect, carries `builds[]`) and
> `build` (every create/update/terminate, carries a single `build`). A terminal transition
> (`finished`/`failed`/`killed`/`gone`) is just a `build` event whose `build.state` is terminal —
> there is **no** separate `build_started`/`build_updated`/`build_finished`/`build_removed`/
> `build_added` event. Clients upsert by `invocation_id` and read `state` to decide rendering.
> E4 later adds a third type, `metrics` (and optionally `alert`), carrying a metrics payload
> keyed by `invocation_id`; E2 reserves those type strings now so E4 only adds a case. Heartbeats
> are **WS protocol ping frames**, never a JSON `{"type":"heartbeat"}` event — E8 must drop its
> `heartbeat` event case and rely on the `URLSessionWebSocketTask` ping handler. The `seq` field
> is per-connection monotonic so a client can detect a gap (and, if it ever cares, re-`GET
> /builds`). Removal/retention: E2 does **not** emit a removal event; terminal builds remain in
> the snapshot/registry per the retention policy (§2.3) and age out of the in-memory map only on
> restart. A client that wants to hide terminal rows filters on `state` locally.

### 2.7 main.go lifecycle (graceful shutdown)

`cmd/broker/main.go`:

```go
func main() {
    cfg := must(config.Load())
    log := logging.New(cfg.LogPath)        // slog JSON → file + stderr (launchd captures stderr too)
    st  := must(store.Open(cfg.DBPath, log))
    hub := registry.NewHub()
    reg := registry.New(st, hub, log)
    must(reg.HydrateFromStore(200))        // load recent builds so /builds survives restart

    ln  := must(net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", cfg.Port)))
    srv := httpapi.New(cfg, reg, hub, log)

    ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
    defer stop()
    go func() { log.Info("listening", "addr", ln.Addr()); _ = srv.Serve(ln) }()

    <-ctx.Done()                            // launchd sends SIGTERM on unload
    log.Info("shutting down")
    sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()
    _ = srv.Shutdown(sctx)                  // drain in-flight HTTP, close WS conns
    _ = st.Close()
}
```

Graceful shutdown: on SIGTERM/SIGINT, stop accepting new conns, give in-flight requests up to
5s, close all WS subscribers, flush+close SQLite. launchd `KeepAlive` restarts the process; the
registry rehydrates from SQLite so `/builds` is continuous across restarts.

### 2.8 launchd plist

`deploy/com.bazelbroker.broker.plist` (template; `install.sh` substitutes the user's home and
binary path):

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>            <string>com.bazelbroker.broker</string>
    <key>ProgramArguments</key>
    <array>
        <string>__BINARY__</string>          <!-- e.g. /usr/local/bin/broker -->
        <string>--config</string>
        <string>__CONFIG__</string>          <!-- ~/.config/bazel-broker/config.json -->
    </array>
    <key>RunAtLoad</key>        <true/>
    <key>KeepAlive</key>
    <dict>
        <key>SuccessfulExit</key> <false/>   <!-- restart on crash; not on clean exit during unload -->
    </dict>
    <key>ProcessType</key>      <string>Background</string>
    <key>ThrottleInterval</key> <integer>5</integer>
    <key>StandardOutPath</key>  <string>__HOME__/.local/state/bazel-broker/stdout.log</string>
    <key>StandardErrorPath</key><string>__HOME__/.local/state/bazel-broker/stderr.log</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>BAZEL_BROKER_CONFIG</key> <string>__CONFIG__</string>
    </dict>
</dict>
</plist>
```

Install path: `~/Library/LaunchAgents/com.bazelbroker.broker.plist` (per-user agent, not a
system daemon — runs as the dev's user, can see their worktrees). `install.sh`:
`launchctl bootstrap gui/$(id -u) <plist>` (modern replacement for `load`), and
`launchctl bootout gui/$(id -u)/com.bazelbroker.broker` to remove.

### 2.9 Logging

`internal/logging/logging.go`: `slog.New(slog.NewJSONHandler(io.MultiWriter(file, os.Stderr), opts))`
with `opts.Level = slog.LevelInfo` (overridable via `--log-level` / `$BAZEL_BROKER_LOG_LEVEL`).
Every request and every state transition logs a structured line:
`{"time":...,"level":"INFO","msg":"build registered","invocation_id":"...","worktree":"...","state":"running"}`.

---

## 3. Sequencing (checkpointed, each independently verifiable)

> Assumes E0 has landed (`go.mod`, `Makefile`, `testdata/fake-bazel.sh`, `CLAUDE.md`).

**T1 — API types + domain model + golden fixtures.** Implement `internal/api/api.go` (all §4
structs + enum consts) and `internal/build/build.go` (`Build`, `State`, `Source`, `ToAPI()`).
Emit the `testdata/api/*.json` golden fixtures (§4.5) from the structs in a test.
✔ Verify: `go build ./...`; a table-test round-trips `build.Build ⇄ api.Build` JSON; the golden
fixtures regenerate byte-identically (so they cannot silently drift from the contract).

**T2 — Config.** `internal/config`: `Load`, `Default`, `Validate`, first-run token generation
and default-file write.
✔ Verify: unit test — Load on empty dir writes a `0600` config with a 64-hex-char token; Load
again reads it back identically; invalid port rejected.

**T3 — Store + schema v1.** `internal/store` with embedded `schema.sql`, `Open`/migrate,
`UpsertBuild`, `MarkTerminal`, `RecentBuilds`, `GetBuild`.
✔ Verify: unit test against a temp-file DB — upsert → read back; `MarkTerminal` flips state +
end_time + exit; `PRAGMA user_version` == 1; targets JSON round-trips.

**T4 — Registry + hub.** `internal/registry`: `Register`/`Deregister`/`Snapshot`/`Get`/`Counts`/
`Upsert`/`HydrateFromStore`, write-through to store, broadcast on mutation; `Hub` fan-out with
drop-on-full.
✔ Verify: unit test — register N builds concurrently (race detector on), assert Snapshot count,
assert a subscriber receives `build` events, assert a full-buffer subscriber is dropped not blocking.

**T5 — HTTP server + auth + healthz + builds.** `internal/httpapi`: `Server`, middleware chain,
loopback listener, `GET /healthz` (no auth), `GET /builds` (auth).
✔ Verify: httptest — `/healthz` 200 without token; `/builds` 401 without token, 200 with token;
body matches §4 shapes.

**T6 — register / deregister handlers.** `POST /register`, `POST /deregister` wired to the
registry. Validation: 400 on missing `invocation_id`/`worktree`; idempotent re-register.
✔ Verify: httptest — register → `/builds` shows it `running`; deregister exit 0 → `finished`;
deregister exit 1 → `failed`; bad body → 400.

**T7 — WS /events.** `coder/websocket` accept, `snapshot`-on-connect, incremental `build`
events, 30s ping, drop-on-slow.
✔ Verify: test client connects → receives `snapshot`; a concurrent register produces a `build`
event frame matching §4.

**T8 — Reserved routes.** Register every reserved path from §4.2
(`POST /builds/{invocation_id}/kill`, `GET /builds/{id}/metrics`, `GET /builds/{id}/profile`,
`GET /metrics`, `GET /profile/{id}/{name}`, `GET /diskcache`, `POST /admission`,
`/admission/release`, `/admission/{pause,resume,drain}`, `GET /admission/status`) → `501` with
`{"error":"not_implemented","message":"owned by E3","epic":"E3"}` etc. Use Go 1.22 method+wildcard
mux patterns.
✔ Verify: httptest — each returns 501 (with token), 401 (without). Confirms the seam other epics
fill, with the **exact paths** the consumers will call (so E3/E4/E5 swap handler bodies only).

**T9 — main wiring + graceful shutdown.** `cmd/broker/main.go`, `logging`, signal handling,
`HydrateFromStore`.
✔ Verify: `make run` → `curl /healthz` → `{builds:0,...}`; SIGTERM exits within 5s; logs land
at the configured path.

**T10 — launchd plist + install.sh.** `deploy/` template + installer + `make install`/`uninstall`.
✔ Verify (manual, on a Mac): `make install` → `launchctl print gui/$(id -u)/com.bazelbroker.broker`
shows it running; `kill -9 <pid>` → KeepAlive restarts it; `/healthz` still answers.

**T11 — CLAUDE.md recipes + Make targets.** Document `make build/run/install/uninstall/test`,
the curl recipes, and the register→ls→deregister flow for `/verify`.
✔ Verify: a fresh reader can run the §5 acceptance flow from CLAUDE.md alone.

Checkpoints map to "Done when" (§5): after **T6** the register→ls→deregister flow works; after
**T9** `curl /healthz`→`{builds:0}` works headlessly; after **T10** launchd restart survival works.

---

## 4. Interfaces & contracts (FROZEN — consumed by E3/E4/E5/E6/E7/E8)

**Base URL:** `http://127.0.0.1:<port>` (default `8765`).
**Auth:** every endpoint except `GET /healthz` requires `Authorization: Bearer <token>` where
`<token>` is `config.json.token`. Missing/wrong → `401 {"error":"unauthorized"}`.
**Content type:** requests and responses are `application/json; charset=utf-8` unless noted.
**Errors:** non-2xx responses carry `{"error":"<machine_code>","message":"<human optional>"}`.
**Times:** all timestamps are **RFC3339 in UTC** (e.g. `2026-06-17T09:41:12Z`). `end_time` is
omitted (or empty string) until terminal.

### 4.1 Shared Go types (`internal/api`)

> **Field names are frozen.** Consumers independently used `id`/`started_at` (E6, E8) and
> `cache_hit_pct` (E7); those are **rejected** in favor of the names below so there is exactly
> one spelling on the wire. E6/E8 must rename their `id`→`invocation_id` and `started_at`→
> `start_time` (or add `CodingKeys`/json tags mapping to these). See §4.6.

```go
package api

// Build is the JSON DTO for one build (returned by /builds, /builds/{id}, /events, /register).
type Build struct {
    InvocationID string   `json:"invocation_id"`       // primary key everywhere; NOT "id"
    Worktree     string   `json:"worktree"`            // absolute path
    WorktreeName string   `json:"worktree_name,omitempty"` // display basename (E3 fills)
    Targets      []string `json:"targets"`             // always present; [] not null
    PID          int      `json:"pid"`
    State        string   `json:"state"`               // see State* consts
    StartTime    string   `json:"start_time"`          // RFC3339 UTC; NOT "started_at"
    EndTime      string   `json:"end_time,omitempty"`  // RFC3339 UTC; omitted until terminal
    ExitCode     int      `json:"exit_code"`           // meaningful only when terminal
    Source       string   `json:"source"`              // "registered" | "discovered"
    ElapsedMS    int64    `json:"elapsed_ms"`          // now-StartTime (or EndTime-StartTime if terminal)

    // ---- enrichment, omitempty so E2-only builds stay valid; filled by later epics ----
    CacheHitRatio *float64 `json:"cache_hit_ratio,omitempty"` // 0.0–1.0; E4. nil/absent until reported.
    ProfileURL    string   `json:"profile_url,omitempty"`     // ready-to-open Perfetto deep-link; E4.
}
```

> **Enrichment-field freeze (resolves E4/E6/E7/E8 divergence):** the cache ratio is
> `cache_hit_ratio` (a 0.0–1.0 float, pointer so "not yet reported" is `null`/absent), **not**
> `cache_hit_pct` (E7) or `cache_hit_rate` (E8) or a bare percent. The Perfetto link is
> `profile_url` (a **fully-formed URL E4 builds**, resolving E6's OD-6 and E8's D-E8-5: clients
> stay dumb and just `open` it). E2 never sets these two fields; E4 does. They live on `Build`
> (not a separate type) so the `/builds` list and WS `build` events carry them once E4 lands,
> with no schema change — E2 ships them as `omitempty` zero values.

```go
const (
    StateQueued   = "queued"
    StateRunning  = "running"
    StateFinished = "finished"
    StateFailed   = "failed"
    StateKilled   = "killed"
    StateGone     = "gone"
    StateUnknown  = "unknown"  // forward-compat fallback for clients

    SourceRegistered = "registered"
    SourceDiscovered = "discovered"
)

// ---- request/response bodies ----

type RegisterRequest struct {
    InvocationID string   `json:"invocation_id"`        // required
    Worktree     string   `json:"worktree"`             // required (absolute path)
    Targets      []string `json:"targets,omitempty"`
    PID          int      `json:"pid,omitempty"`
    Source       string   `json:"source,omitempty"`     // default "registered"
}

type DeregisterRequest struct {
    InvocationID string `json:"invocation_id"`          // required
    ExitCode     int    `json:"exit_code"`              // 0 => finished, non-0 => failed
}

type HealthResponse struct {
    Status   string `json:"status"`     // "ok"
    Builds   int    `json:"builds"`     // count in non-terminal states (building)
    Queued   int    `json:"queued"`     // count in "queued" (always 0 until E5)
    Total    int    `json:"total"`      // all known builds in registry
    Version  string `json:"version"`    // broker build version
    Uptime   int64  `json:"uptime_ms"`
}

type BuildsResponse struct {
    Builds []Build `json:"builds"`      // newest StartTime first
}

type ErrorResponse struct {
    Error   string `json:"error"`            // machine code
    Message string `json:"message,omitempty"`
}

// ---- WS event envelope (FROZEN — see §2.6 taxonomy note) ----

type EventType string
const (
    EventSnapshot EventType = "snapshot" // full list, sent once on connect (carries Builds)
    EventBuild    EventType = "build"    // a single build created/updated/terminated (carries Build)
    EventMetrics  EventType = "metrics"  // RESERVED for E4: metrics update keyed by invocation_id
    EventAlert    EventType = "alert"    // RESERVED for E4: low-cache-hit alert
)

type Event struct {
    Type   EventType `json:"type"`
    Seq    uint64    `json:"seq"`              // monotonically increasing per connection
    Build  *Build    `json:"build,omitempty"`  // set for type=="build"
    Builds []Build   `json:"builds,omitempty"` // set for type=="snapshot"
    Ts     string    `json:"ts"`               // RFC3339 UTC emit time
    // Metrics *Metrics `json:"metrics,omitempty"` // E4 adds this field with the metrics type
}

// ---- E3/E4 response bodies declared here so the shapes are frozen before those epics land ----

// KillResult is the POST /builds/{id}/kill response (E3 fills the handler).
type KillResult struct {
    Killed       bool   `json:"killed"`
    InvocationID string `json:"invocation_id"`
    PID          int    `json:"pid"`
    Outcome      string `json:"outcome"`        // sigint | sigkill | cancelled | already_gone | error
    ElapsedMS    int64  `json:"elapsed_ms"`
}

// ProfileRef is the GET /builds/{id}/profile response (E4 fills the handler).
type ProfileRef struct {
    PerfettoURL string `json:"perfetto_url"`     // ready-to-open ui.perfetto.dev deep-link
    LocalPath   string `json:"local_path"`       // absolute path to the --profile .gz (fallback)
}
```

### 4.2 Route table (FROZEN paths — supersedes every consumer's guesses)

> The single biggest review finding: **E3/E4/E5/E6/E7/E8 each invented different paths for the
> same endpoints** (`/kill` vs `/builds/{id}/kill`; `/metrics?invocation_id` vs `/metrics?id` vs
> `/builds/{id}/metrics`; `/drain` vs `/admission/drain`; `/pause` vs `/admission/pause`). The
> table below is the **one true routing**. Consumers conform to this; §4.6 lists each rename.
> Convention: **per-build actions are nested under `/builds/{invocation_id}/…`**; admission policy
> lives under `/admission/…`; collection/aggregate reads are top-level.

| Method | Path | Auth | Owner | Request | Success response |
|---|---|---|---|---|---|
| GET  | `/healthz`                    | none   | E2 | — | `200 HealthResponse` |
| GET  | `/builds`                     | bearer | E2 | — | `200 BuildsResponse` |
| GET  | `/builds/{invocation_id}`     | bearer | E2 | — | `200 {"build": Build}` / `404` |
| WS   | `/events`                     | bearer | E2 | — (upgrade) | stream of `Event` (first frame `snapshot`) |
| POST | `/register`                   | bearer | E2 | `RegisterRequest` | `200 {"build": Build}` |
| POST | `/deregister`                 | bearer | E2 | `DeregisterRequest` | `200 {"build": Build}` |
| POST | `/builds/{invocation_id}/kill`| bearer | **E3** | `{}` or `{"pid":N,"force":bool,"use_cancel":bool}` | reserved → `501`; later `200 KillResult` |
| GET  | `/builds/{invocation_id}/metrics` | bearer | **E4** | — | reserved → `501`; later `200 Metrics` |
| GET  | `/builds/{invocation_id}/profile` | bearer | **E4** | — | reserved → `501`; later `200 ProfileRef` |
| GET  | `/metrics`                    | bearer | **E4** | `?recent=N` (list) or `?invocation_id=…` (one) | reserved → `501`; later metrics list/one |
| GET  | `/profile/{invocation_id}/{name}` | bearer*| **E4** | — | reserved → `501`; later streams the `.gz` (see note) |
| GET  | `/diskcache`                  | bearer | **E4** | — | reserved → `501`; later disk-cache size report |
| POST | `/admission`                  | bearer | **E5** | `{invocation_id,worktree,targets,pid}` | reserved → `501`; later **status-code + one-word body** (see note) |
| POST | `/admission/release`          | bearer | **E5** | `{invocation_id}` | reserved → `501`; later `204` |
| POST | `/admission/pause`            | bearer | **E5** | — | reserved → `501`; later `200` |
| POST | `/admission/resume`           | bearer | **E5** | — | reserved → `501`; later `200` |
| POST | `/admission/drain`            | bearer | **E5** | — | reserved → `501`; later `200` |
| GET  | `/admission/status`           | bearer | **E5** | — | reserved → `501`; later `200` policy JSON |

Reserved routes are **registered now** and return
`501 {"error":"not_implemented","message":"owned by <epic>","epic":"E3"}` so clients (E6 CLI) can
probe capability and later epics only swap the handler body — the path/method/auth contract is
fixed. The `{invocation_id}` path segment uses Go 1.22+ `ServeMux` wildcard patterns
(`mux.HandleFunc("POST /builds/{invocation_id}/kill", …)`).

**Critical notes on the two endpoints that broke their own contract in the consumer plans:**

- **`POST /admission` does NOT return JSON.** The earlier E2 draft said it would return
  `{"decision":"allow|queue|deny"}`. That is **wrong** and is corrected here: E5's wrapper is
  bash 3.2 with no guaranteed `jq`, so `/admission` returns the verdict as the **HTTP status
  code plus a single-word body** (`200 ALLOW` / `202 QUEUE` / `403 DENY`; `5xx`/unreachable →
  wrapper fails open). Optional detail rides `X-Broker-*` response headers. This matches E5 §2.2
  exactly. E2's reserved stub returns `501` with the standard error body until E5 lands; the
  `501` is fine because the wrapper treats any non-200/202/403 as fail-open.

- **`GET /profile/{id}/{name}` auth is special (`bearer*`).** Perfetto (`ui.perfetto.dev`)
  fetches this `.gz` cross-origin from the browser and **cannot** send a bearer token. E4 §2.9
  serves it with `Access-Control-Allow-Origin: https://ui.perfetto.dev` and resolves the file
  path from the DB (never the URL) to stay traversal-safe. Whether this single read-only,
  content-addressed-by-id route is **token-exempt** (like `/healthz`) or guarded by an
  unguessable per-profile token is an **open item co-owned by E2 and E4** — flagged in §6. All
  *other* routes are strictly bearer-guarded.

### 4.3 Concrete request/response examples

`GET /healthz`:
```json
{"status":"ok","builds":0,"queued":0,"total":0,"version":"0.1.0","uptime_ms":1423}
```

`POST /register` ← `{"invocation_id":"a1b2","worktree":"/wt/feature-a","targets":["//app:App"],"pid":4242}`
```json
{"build":{"invocation_id":"a1b2","worktree":"/wt/feature-a","targets":["//app:App"],
  "pid":4242,"state":"running","start_time":"2026-06-17T09:41:12Z","exit_code":0,
  "source":"registered","elapsed_ms":0}}
```

`GET /builds`:
```json
{"builds":[{"invocation_id":"a1b2","worktree":"/wt/feature-a","targets":["//app:App"],
  "pid":4242,"state":"running","start_time":"2026-06-17T09:41:12Z","exit_code":0,
  "source":"registered","elapsed_ms":3120}]}
```

`POST /deregister` ← `{"invocation_id":"a1b2","exit_code":0}`
```json
{"build":{"invocation_id":"a1b2","worktree":"/wt/feature-a","targets":["//app:App"],
  "pid":4242,"state":"finished","start_time":"2026-06-17T09:41:12Z",
  "end_time":"2026-06-17T09:45:01Z","exit_code":0,"source":"registered","elapsed_ms":228900}}
```

`WS /events` first frame, then an update:
```json
{"type":"snapshot","seq":0,"builds":[...],"ts":"2026-06-17T09:41:00Z"}
{"type":"build","seq":1,"build":{"invocation_id":"a1b2","state":"running",...},"ts":"2026-06-17T09:41:12Z"}
```

### 4.4 Contract guarantees other epics rely on

- **E3 (discovery/kill):** uses `registry.Upsert`/`ReapMissingDiscovered`/`FindByPID`/
  `FindByInvocationID` (all declared in §2.3) to merge discovered processes (`source:"discovered"`)
  and a `Kill` path to set `state:"killed"`. Fills `POST /builds/{invocation_id}/kill` →
  `api.KillResult`. Populates the `ExePath`/`Cwd`/`GitDir`/`WorktreeName`/`LastSeen` seam fields.
- **E4 (BEP/metrics):** keys everything on `invocation_id`; reads `worktree` to locate the BEP
  json / `--profile`. Fills `GET /builds/{id}/metrics`, `GET /metrics`, `GET /profile/{id}/{name}`,
  `GET /diskcache`; sets `Build.CacheHitRatio` and `Build.ProfileURL`; adds the `metrics` table
  (already declared in DDL v1) and emits the reserved `metrics`/`alert` WS event types.
- **E5 (admission):** fills `POST /admission` (status-code body), `/admission/release`,
  `/admission/{pause,resume,drain}`, `GET /admission/status`; consumes `config.max_concurrency`
  and the `queued` state already in the enum; extends `/healthz` with the live `queued` count.
- **E6 (CLI):** imports `internal/api`; `ls` ← `GET /builds`, `watch` ← `WS /events`,
  `kill` ← `POST /builds/{id}/kill`, `drain` ← `POST /admission/drain`,
  `profile` ← `GET /builds/{id}/profile`. **`internal/api` types must stay importable and stable.**
- **E7/E8 (web/menu-bar):** consume `WS /events` (snapshot+incremental) and `GET /builds`. The
  `snapshot`-on-connect contract (now a hard guarantee, §2.6) is what makes their UIs
  self-syncing. E8's `Codable` structs and E7's JS must use the §4.1 field names verbatim.

### 4.5 Golden JSON fixtures (the executable cross-epic handshake)

E8 §4.4 asks E2/E4 to commit golden JSON fixtures that the Swift `Codable` decode test (and any
other client) verifies against. **Accepted and made an E2 deliverable:** E2 commits, under
`testdata/api/`, canonical samples that every consumer decodes verbatim — `healthz.json`,
`builds.json` (list with one running + one finished build), `build.json` (single), and
`event_snapshot.json` + `event_build.json` (WS frames). These are generated from the real Go
structs in a test so they cannot drift from the code. E4 adds `metrics.json` and `event_metrics.json`
in its epic. **If a client decodes these fixtures, client and daemon agree** — this is the
contract test that catches field-name drift early. Added to the T1 checkpoint (§3).

### 4.6 Reconciliation notes — what each consumer must change

Concrete deltas from the consumer plans' guesses to this frozen contract. **Each consumer epic
should be patched to match (tracked in the consolidated cross-epic review).**

| Consumer | Wrote | Must become |
|---|---|---|
| E3 | `POST /kill`; `registry.Build` `FirstSeen`; `UpsertSpec`; PID `int32`; reap→`gone` | `POST /builds/{invocation_id}/kill`; reuse `StartTime` as first-seen; `Upsert(*build.Build)`; narrow PID to `int` at procscan edge; `gone` is in the enum ✓ |
| E4 | `GET /metrics?id=`; `internal/api` ✓; `cache_hit_rate`/`cache_hit_pct` ad-hoc | `?invocation_id=` (one) or `?recent=N` (list); set `Build.CacheHitRatio` (0–1 float ptr) + `Build.ProfileURL`; `metrics`/`alert` WS types reserved here ✓ |
| E5 | `/pause` `/resume` `/drain` top-level; JSON `{decision}` worry | `/admission/{pause,resume,drain}`; `/admission` returns status-code + one-word body (E5 already specced this — E2's old stub note was the bug, now fixed) ✓ |
| E6 | `internal/api` ✓; `id`/`started_at`; `POST /builds/{id}/kill` ✓; `/admission/drain` ✓; `Health{builds,queued}` | rename `BuildInfo.ID`→json `invocation_id`, `StartedAt`→`start_time`; `Health` matches `HealthResponse` (§4.1); OD-1 (api pkg) ✓ resolved, OD-2 (lenient json) resolved lenient, OD-5 (snapshot) resolved guaranteed, OD-6 (perfetto url) resolved E4-owned `profile_url` |
| E7 | `/kill`; `cache_hit_pct`; `build_started/updated/finished/removed` WS types; `profile_url` ✓ | `POST /builds/{id}/kill`; `cache_hit_ratio`; **two** event types (`snapshot`,`build`) upsert-by-id; OD-1 browser auth still open (escalated) |
| E8 | `id`/`started_at`; `building` state; `POST /builds/{id}/kill` ✓; `cache_hit_rate`; `heartbeat` event; `metrics_updated` event | `invocation_id`/`start_time`; state `running` (not `building`); `cache_hit_ratio`; drop `heartbeat` JSON event (use WS ping); `metrics` event type (not `metrics_updated`) |

> **`running` vs `building`:** E7 and E8 render the active state as `building`; E5/E6 use `building`
> in prose too. The frozen wire value is **`running`** (§2.2/§4.1). UIs may *label* it "building"
> in the view layer, but the JSON `state` string is `running`. This must be fixed in E7's
> `applyEvent`/`renderRow` and E8's `BuildState` enum.

---

## 5. Testing & verification

### 5.1 Unit / integration (fast, headless — the `/verify` core)
- `go test ./... -race` covers store round-trip, registry concurrency, auth, handlers (httptest),
  and WS snapshot+event (in-process `coder/websocket` client).
- `make verify` runs `go vet ./... && go test ./... -race`.

### 5.2 Manual smoke (the "Done when" flow)

Assume `TOKEN=$(jq -r .token ~/.config/bazel-broker/config.json)` and `PORT=8765`.

```bash
# 1. health (no auth)
curl -s localhost:8765/healthz | jq .
# => {"status":"ok","builds":0,"queued":0,"total":0,...}

# 2. register a build
curl -s -X POST localhost:8765/register \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"invocation_id":"smoke1","worktree":"/wt/a","targets":["//app:App"],"pid":4242}' | jq .

# 3. ls --json equivalent
curl -s localhost:8765/builds -H "Authorization: Bearer $TOKEN" | jq '.builds[].state'
# => "running"

# 4. live events (separate terminal) — should print snapshot then a build event on deregister
websocat -H "Authorization: Bearer $TOKEN" ws://127.0.0.1:8765/events

# 5. deregister
curl -s -X POST localhost:8765/deregister \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"invocation_id":"smoke1","exit_code":0}' | jq '.build.state'
# => "finished"

# 6. auth negative
curl -s -o /dev/null -w '%{http_code}\n' localhost:8765/builds   # => 401
```

### 5.3 launchd restart-survival
```bash
make install
launchctl print gui/$(id -u)/com.bazelbroker.broker | grep -i state   # running
PID=$(pgrep -f 'broker --config'); kill -9 "$PID"; sleep 6
curl -s localhost:8765/healthz | jq .status                          # "ok" again (KeepAlive restarted it)
# register a build, kill -9 the broker, confirm /builds still lists it after restart (SQLite hydration)
```

### 5.4 `/verify` recipe (for CLAUDE.md)
> "Run `make build && make run` (foreground), then in another shell run `make smoke` which
> executes the curl flow in §5.2 and asserts states `running`→`finished` and a 401 on the
> unauthenticated `/builds`. For launchd: `make install && make verify-launchd`."

### 5.5 Acceptance criteria (from E2 "Done when")
- [ ] `launchctl` starts the broker (`RunAtLoad` + manual bootstrap both work).
- [ ] `curl /healthz` → `{builds:0,...}` on a fresh start.
- [ ] Manual `/register` then `/builds` (== `ls --json`) shows the build as `running`.
- [ ] After `kill -9` of the daemon, KeepAlive restarts it and `/builds` still reflects prior
      builds (SQLite hydration) — survives restart.
- [ ] All reserved routes (every path in §4.2 owned by E3/E4/E5) return `501` (not `404`) at the
      **exact path/method** the consumer will call, proving the contract seams exist.
- [ ] The committed `testdata/api/*.json` golden fixtures (§4.5) decode against `internal/api`.

---

## 6. Risks, edge cases, open decisions

**Open decisions (surfaced, NOT resolved here — defer to project-level decision log):**

- **D-stack-2 — transport: loopback TCP + bearer token (recommended) vs Unix domain socket.**
  This plan implements the **recommended** TCP+token path because (a) E8's SwiftUI
  `URLSessionWebSocketTask` and E7's browser both speak HTTP/WS trivially, and (b) `curl`/`/verify`
  is one-liner. **Trade-off:** a loopback TCP port is reachable by any local process/user on the
  machine, so the bearer token is the only guard (file-permission `0600` on config). A Unix socket
  with filesystem perms would be stronger isolation but adds friction for the browser/Swift
  clients. **Decision left open**; the `httpapi.Server` takes a `net.Listener` so swapping to a
  `net.Listen("unix", …)` later is localized. *Flag for owner sign-off before E7/E8.*

- **SQLite write concurrency / single-writer.** v1 funnels all writes through the registry
  write-lock + `SetMaxOpenConns(1)` + WAL + `busy_timeout`. **Open:** if E4's BEP ingest writes
  metrics on a hot path concurrently with registry mutations, we may want a dedicated writer
  goroutine + channel (serialized writes, parallel reads via a second read-only `*sql.DB`).
  Deferred to E4; the `store.Store` API is written so an internal writer queue can be added without
  changing callers.

- **Browser auth for E7 (co-owned E2↔E7) — OPEN, ESCALATED.** A browser cannot send a bearer
  header on the `WebSocket` upgrade nor read `config.json`. E7 §2.4 proposes A (same-origin
  session cookie that *this* middleware also accepts), B (token templated into the page + token on
  the WS URL/subprotocol), or C (loopback-`Origin` exemption). **Recommendation: A**, because it
  keeps the token out of the DOM and authenticates the WS upgrade via the cookie — but A and C
  both modify E2's auth middleware, so the call is **not E2's to make alone**. E2 ships
  bearer-only and exposes an `authMode` seam in `auth.go`; the chosen option (plus a CSRF defense
  on the `POST` mutations under a cookie scheme, E7 OD-2) lands with E7's T5. *Flag for E2+E7
  joint sign-off before E7 starts.*

- **`GET /profile/{id}/{name}` token exemption (co-owned E2↔E4) — OPEN.** Perfetto fetches this
  `.gz` from `ui.perfetto.dev` and cannot present the bearer token, so this single read-only,
  id-addressed route likely must be token-exempt (with `Origin`-restricted CORS) or guarded by an
  unguessable per-profile token embedded in the deep-link. Decide with E4 when the Perfetto path
  lands; until then the reserved stub `501`s. *No code in E2 now; flagged so it isn't forgotten.*

- **Registry locking model.** A single `RWMutex` is simple and correct for the expected load
  (handful of builds, human/agent cadence). If `WS /events` fan-out under many subscribers ever
  contends with mutations, move broadcasts off the write-lock (snapshot the event, release lock,
  then `hub.Broadcast`). The code already broadcasts *after* releasing the write-lock to pre-empt
  this. *No action needed now; noted.*

**Edge cases handled:**
- **Idempotent register:** re-`POST /register` with the same `invocation_id` updates (heartbeat),
  does not duplicate. Important because the wrapper (E5) and discovery (E3) may both report the
  same build.
- **Deregister of unknown/terminal id:** no-op `200` (avoids races where discovery already
  reaped it).
- **Discovered vs registered merge (E3 seam):** `Upsert` must not let a later `discovered` record
  clobber a richer `registered` one (keep targets/source registered). Rule documented in T4.
- **Clock skew / elapsed:** `elapsed_ms` computed server-side from monotonic-ish wall clock; UI
  doesn't compute it from possibly-skewed client time.
- **Port already in use:** `net.Listen` fails fast → log fatal → launchd `ThrottleInterval` backs
  off (won't hot-loop). Surfaced clearly in stderr log.
- **Missing config on first run:** `Load` bootstraps a default config + token (so `make run`
  works with zero setup); token printed once at INFO for the operator to copy.
- **WS slow consumer:** dropped (channel closed) rather than blocking the registry — see §2.3.
- **Loopback binding:** must be `127.0.0.1`, never `0.0.0.0`/`:port`. Asserted by a test that the
  listener address is loopback.

**Risks:**
- **Token in a plaintext config** is the whole security model. Mitigation: `0600` perms, loopback
  only, document that this is single-user-Mac scope (matches N4). If multi-user Macs become a
  concern, revisit D-stack-2 (Unix socket).
- **`coder/websocket` API drift** (it's actively maintained) — pin the version in `go.mod`.
- **launchd per-user vs system daemon** confusion — we deliberately use a **LaunchAgent** (per-user)
  so the daemon runs as the developer and can read their worktrees; documented in §2.8.

---

## 7. Effort & internal ordering

Rough sizing (one engineer, Go-fluent), to be tracked as the executable task list in §3:

| Tasks | Area | Est. |
|---|---|---|
| T1–T2 | `internal/api` types, domain model, golden fixtures, config (+tests) | 0.5 day |
| T3 | SQLite store + schema v1 + migration (+tests) | 0.5 day |
| T4 | registry + hub, write-through, fan-out (+race tests) | 1 day |
| T5–T6 | HTTP server, auth, healthz/builds/register/deregister (+httptest) | 1 day |
| T7 | WS `/events` (snapshot + incremental, ping, drop) (+test) | 0.5 day |
| T8 | reserved 501 routes | 0.25 day |
| T9 | main wiring, slog, graceful shutdown, hydration | 0.5 day |
| T10 | launchd plist + install.sh + make install/uninstall | 0.5 day |
| T11 | CLAUDE.md recipes + make targets | 0.25 day |

**Total ≈ 5.25 engineer-days.** Critical path: T1→T3→T4→T5/6→T9 (a working authenticated
register→ls→deregister daemon). T7 (WS) and T8 (reserved) parallelize after T4/T5. T10/T11
(launchd + docs) come last and unblock the "survives restart" acceptance criterion.

**Recommended commit order:** ship in the T-order above; each task is a self-contained PR with its
own passing test, so `/verify` stays green at every step and E3/E4/E6 can start against the frozen
§4 contract as soon as T6 + T7 land (even before T10 launchd packaging).

---

## Staff Engineer Review

**Reviewer:** Staff Eng · **Date:** 2026-06-17 · **Scope:** E2 as the shared API contract for E3–E8.

### (a) Verdict

**Approve with mandatory contract changes — now applied (Plan v2).** The E2 design itself is
sound: the registry/store/hub decomposition, the RWMutex + broadcast-after-unlock concurrency
model, the `SetMaxOpenConns(1)`+WAL+busy_timeout single-writer discipline, loopback-only binding,
graceful shutdown, and the LaunchAgent posture are all correct and appropriately scoped. **But the
document under review was *not* a usable contract**, because every downstream epic had independently
guessed incompatible route paths, JSON field names, the shared package name, and the WS envelope —
and worse, E2's own reserved-route note for `/admission` contradicted E5's wire format. Left
unreconciled, E3–E8 would each have built against a different API and integration would have failed
late. I froze §4 and patched the inconsistencies in place. With those changes the contract is
coherent and consumers can build against it.

### (b) Top findings (API-contract gaps that ripple across epics)

1. **Route-path anarchy (highest impact).** Six different spellings existed for the same handful of
   endpoints (`/kill` vs `/builds/{id}/kill`; `/metrics?invocation_id` vs `?id` vs
   `/builds/{id}/metrics`; `/drain` vs `/admission/drain`; `/pause` vs `/admission/pause`). Frozen
   to one routing in §4.2 with a stated convention (per-build actions nested under `/builds/{id}`,
   policy under `/admission`).
2. **`/admission` self-contradiction.** E2's reserved stub said it would return JSON
   `{"decision":...}`; E5 (correctly) specs a **status-code + one-word body** because the wrapper is
   bash 3.2 with no `jq`. E2 was the bug. Corrected, with the rationale inline so it can't regress.
3. **Shared package name split.** E2 said `internal/wire`; E6 and E4 already wrote `internal/api`.
   Standardized on `internal/api` (the name two consumers assumed) — `wire` retired.
4. **WS envelope: four taxonomies.** E2 (`snapshot`/`build`), E6
   (`snapshot`/`build_update`/`build_removed`), E7 (`build_started/updated/finished/removed`), E8
   (`+build_added/metrics_updated/heartbeat`). Frozen to exactly **two** types (`snapshot`,
   `build`, upsert-by-`invocation_id`, read `state`), reserved `metrics`/`alert` for E4, and ruled
   that heartbeats are WS ping frames not JSON events. Snapshot-on-connect is now a hard guarantee
   (resolves E6-OD5, E8 assumption).
5. **Field-name drift.** Clients invented `id`/`started_at`/`cache_hit_pct`/`cache_hit_rate` and the
   state value `building`. Frozen: `invocation_id`, `start_time`, `cache_hit_ratio` (0–1 float, ptr),
   `running`. Per-consumer renames tabulated in §4.6.
6. **E3 type seams missing from E2.** E3 asks E2 to *extend* `build.Build` and the registry; the
   fields (`ExePath`/`Cwd`/`GitDir`/`WorktreeName`/`LastSeen`), the `gone` state, and the
   `Upsert`/`ReapMissingDiscovered`/`FindByPID`/`FindByInvocationID` methods are now declared as
   seams in §2.2/§2.3 so E3 adds behavior, not schema.
7. **Config shape gap.** E6/E8 read `host` (and E6 `profile_open`) that E2's config never had. Added,
   with a note that `host` is advisory for clients only (daemon still binds loopback). Resolved
   E6-OD2 as lenient JSON decoding.
8. **Enrichment ownership.** `Build.CacheHitRatio`/`ProfileURL` added as `omitempty` fields E2 leaves
   zero and E4 fills — so `/builds` and WS events carry them with no later schema break (resolves
   E6-OD6, E8-D5: clients just `open` `profile_url`).

### (c) What I changed in this file

- Froze §4 and stamped the header "Plan v2 (contract frozen)" with a pointer to §4.6.
- Renamed `internal/wire`→`internal/api` (layout, rationale, `ToAPI()`, T1).
- Rewrote §4.1 types (frozen field names; added `gone`/`unknown`; added `KillResult`,
  `ProfileRef`, enrichment fields, reserved `metrics`/`alert` event types).
- Rewrote §4.2 route table to the canonical paths; added the `/admission` status-code-body
  correction and the `/profile` CORS/auth note; updated T8 and the §5.5 acceptance to match.
- Added §2.2/§2.3 E3 seam fields + registry methods + `HydrateFromStore`; added the WS event
  taxonomy freeze in §2.6 and the browser-auth framing.
- Added §4.5 golden-fixtures deliverable and §4.6 per-consumer reconciliation table.
- Added `host`/`profile_open` to config; added the two escalated open decisions to §6.

### (d) Decisions / risks to escalate to the consolidated cross-epic review

- **ESCALATE — Browser auth (E7 OD-1), co-owned E2↔E7.** Genuinely open; not decided here.
  Recommend Option A (same-origin session cookie that E2's middleware also accepts + CSRF on
  mutations). A and C modify E2's auth middleware, so E2+E7 must sign off jointly **before E7 T5**.
- **ESCALATE — `GET /profile/{id}/{name}` token exemption, co-owned E2↔E4.** Perfetto can't send
  the bearer token; decide token-exempt+CORS vs per-profile unguessable token when E4's Perfetto
  path lands.
- **ESCALATE — D-stack-2 (TCP+token vs Unix socket).** Unchanged recommendation (TCP+token, because
  browser + SwiftUI speak HTTP/WS trivially and `curl` is one-liner). Owner sign-off still pending;
  the `net.Listener` seam keeps a later flip localized. Note the token-in-plaintext-config model is
  the *entire* security boundary on a loopback TCP port — acceptable only under the single-user-Mac
  scope (N4).
- **ESCALATE — patch the consumer epics to §4.6.** E3–E8 docs still contain their old spellings;
  the consolidated review should apply the §4.6 renames to each so no epic is implemented against a
  stale shape. Highest-value pre-implementation cleanup.
- **RISK (noted, deferred) — SQLite write contention with E4.** `SetMaxOpenConns(1)` serializes E4's
  hot-path BEP metric writes behind registry mutations. The `store.Store` API is written so a
  dedicated writer goroutine + separate read-only pool can be added without changing callers;
  revisit in E4 if contention shows up. No E2 change now.
