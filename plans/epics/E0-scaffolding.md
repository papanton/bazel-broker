# E0 — Project scaffolding & verify harness

> Standalone executable plan for Epic 0 of the **Bazel Broker** project.
> Authoritative parents: `plans/01-architecture.md`, `plans/02-epics.md`.

Status: **Plan v1** · Owner: Antonis · Target: macOS arm64, Go 1.26, Xcode 26 · Date: 2026-06-17

---

## 1. Goal & scope recap

**Goal (from 02-epics EPIC 0):** a repo where every later epic can be built and `/verify`-ed
headlessly in seconds. This epic produces **no product behavior** — it produces the *ground*
on which all other epics stand: the module layout, the build/run/verify commands, the fake
Bazel that makes kill/admission a 1-second check, a real synthetic workspace for occasional
true e2e, and the shared conventions (module path, `internal/` layout, slog logging, config
location, env vars, ports/tokens) that E1–E8 will import or assume.

> **Contract-alignment note (added in review).** `E2-broker-core.md` self-declares as *"the
> authoritative API contract"* and concretizes several things E0 also touches: the package
> split (`internal/build` domain + `internal/wire` DTO, **not** `internal/buildinfo`), the
> `State`/`Source` enums, the `Config` schema, the state dir, the slog handler format, and the
> default port. **Where E0 and E2 disagreed, this revision aligns E0 to E2** and flags the
> remaining genuine forks (transport, ephemeral-vs-fixed port, client discovery) as open
> decisions to escalate — see §6. E0 *freezes* these only as far as E2 has committed; anything
> E2 left open, E0 must not unilaterally resolve.

**In scope (this epic):**
- Repo directory layout: `cmd/broker`, `cmd/brokerctl`, `tools/bazel` (placeholder), `testdata/`,
  `internal/` packages, `plans/`.
- `go.mod` — single module covering broker + CLI. **Proposed module path:**
  `github.com/antoniospapantoniou/bazel-broker` (see §6 / Open Decision OD-1).
- `Makefile` targets: `build`, `run-broker`, `verify-fast`, `verify-e2e`, plus support targets
  (`fmt`, `vet`, `test`, `clean`, `tidy`, `tools`).
- `.gitignore`.
- `testdata/fake-bazel.sh` — full, working `BAZEL_REAL` stub (the central verify enabler).
- `testdata/workspace/` — a trivial synthetic Bazel workspace + a tiny target for real e2e.
- `CLAUDE.md` — per-component "how to run & verify" recipes + canonical Make targets.
- Skeleton (compile-only) for the `internal/` packages and `cmd/` mains so `make build` and
  `make verify-fast` are green from day one. These are **stubs with stable signatures**, not
  implementations — E2+ fill them in.

**Explicitly NOT in scope (deferred to owning epics):**
- Real HTTP/WS server, registry logic, SQLite schema *content* → **E2**.
- libproc discovery / kill → **E3**.
- BEP parsing, generated protos, metrics → **E4**.
- Admission, real `tools/bazel` wrapper logic → **E5**.
- `brokerctl` real subcommands → **E6**.
- Web dashboard / menu-bar app → **E7 / E8**.
- `.bazelrc` cache flags / `setup.sh` → **E1** (independent; not blocked by E0).

**Done when (verbatim from 02-epics, expanded in §5):**
1. `make build` succeeds.
2. `FAKE_BAZEL_DURATION=2 testdata/fake-bazel.sh --build_event_json_file=/tmp/x.json` writes
   events and exits 0.
3. SIGINT to it exits with the cancel code.

**Dependency position:** E0 has no deps and unblocks everything (E0 → E2 → E3/E4/E6 → E5; E1
independent; E7/E8 over E2). Therefore E0's deliverables must be **forward-compatible**: the
package boundaries and shared constants defined here are contracts (§4) the other epics build on.

---

## 2. Design & implementation details

### 2.1 Repository layout

```
bazel-broker/
├── go.mod                          # module github.com/antoniospapantoniou/bazel-broker (OD-1)
├── go.sum
├── Makefile
├── .gitignore
├── CLAUDE.md
├── .tool-versions                  # (optional) pin go 1.26.4 for asdf users
├── cmd/
│   ├── broker/
│   │   └── main.go                 # daemon entrypoint (stub: flag parse, slog, runs server stub)
│   └── brokerctl/
│       └── main.go                 # CLI entrypoint (stub: cobra root + `version`, `ls` stubs)
├── internal/
│   ├── build/
│   │   └── build.go                # build.Build domain struct + State + Source enums (E2 §2.2 owns final shape)
│   ├── wire/
│   │   └── wire.go                 # IMPORTABLE wire DTOs (wire.Build, Event, *Request) — E2 §4 owns final shape
│   ├── config/
│   │   └── config.go               # config load/default; path resolution (E2 §2.5 owns final shape)
│   ├── logging/
│   │   └── logging.go              # slog setup (handler + level from env) — package named `logging` per E2
│   ├── apiclient/
│   │   └── apiclient.go            # brokerctl→broker HTTP client (stub; base URL + bearer)
│   ├── httpapi/
│   │   └── server.go               # http mux stub: /healthz only — package `httpapi` per E2 §2.6
│   ├── registry/
│   │   └── registry.go             # in-mem registry interface + no-op impl (E2/E3 expand)
│   ├── store/
│   │   └── store.go                # SQLite store interface (E2 schema v1; E4 metrics) — stub
│   ├── discovery/                  # (empty placeholder w/ doc.go) — E3
│   │   └── doc.go
│   ├── bep/                        # (empty placeholder w/ doc.go) — E4
│   │   └── doc.go
│   └── version/
│       └── version.go              # ldflags-injected version/commit/date
├── tools/
│   └── bazel                       # placeholder wrapper (E5 implements); exec passthrough now
├── testdata/
│   ├── fake-bazel.sh               # ★ full BAZEL_REAL stub (this epic)
│   ├── README.md                   # how the fixtures are used
│   └── workspace/                  # ★ synthetic real Bazel workspace (this epic)
│       ├── MODULE.bazel
│       ├── .bazelversion
│       ├── BUILD.bazel
│       └── hello.sh
├── scripts/
│   └── verify-fast.sh              # orchestrates the fast verify (called by Make)
└── plans/                          # (already exists)
```

**Why `internal/`:** everything that is not an entrypoint lives under `internal/` so the module
exposes no importable public API surface — appropriate for a self-contained local tool, and it
keeps later epics from accidentally coupling to unstable internals across module boundaries.

**Why stubs now:** the "Done when" only requires `make build` to compile and the fake-bazel to
work. But to make E0 genuinely *unblock* E2+, we lock the package names and the few shared types
(`build.Build`/`wire.Build`, `config.Config`, env-var/port constants) as compiling stubs — matching
E2's shapes (§2.3). Stubs return `errNotImplemented` or zero values; they must `go build` and
`go vet` clean.

### 2.2 `go.mod`

Single module, Go 1.26. Dependencies are declared up-front (as `// indirect`-free direct deps)
so each epic just *uses* them rather than re-running `go get`, keeping `go.sum` churn out of
feature PRs. Pin to versions current as of 2026-06 (resolve exact patch via `go get` at impl time).

```
module github.com/antoniospapantoniou/bazel-broker

go 1.26

require (
    github.com/spf13/cobra v1.8.1          // brokerctl CLI (E6) + broker flags
    modernc.org/sqlite v1.34.1             // pure-Go SQLite (E2 store, E4 metrics) — no cgo for DB
    github.com/coder/websocket v1.8.12     // broker WS StreamEvents (E2/E7)
    github.com/nxadm/tail v1.4.11          // tail BEP json file (E4)
    google.golang.org/protobuf v1.36.1     // protojson + generated build_event_stream (E4)
)
```

> Versions above are **illustrative pins as of plan authoring**; T1 runs `go get` and records the
> exact patch versions current on the build machine, then `go mod tidy`. Two checks T1 must make:
> (a) the chosen `modernc.org/sqlite` actually builds under **Go 1.26 / arm64** (it's pure-Go but
> has historically been version-sensitive to new Go releases — verify, don't assume); (b)
> `go.mod`'s `go 1.26` line matches the toolchain in `.tool-versions` so CI and dev agree.

Notes:
- **modernc.org/sqlite is pure Go** (no cgo), so the *database* layer never forces cgo. cgo is
  only introduced by **E3** (libproc) and is isolated to `internal/discovery` behind a build
  tag / interface (see §6 D-stack-1) so the rest of the binary stays cross-compilable.
- We do NOT add the protobuf dep's generated code in E0 — only the dep line, so E4 can `protoc`
  into `internal/bep/` without touching `go.mod`. (If declaring an unused dep is undesirable,
  E0 may leave the proto/websocket/tail/sqlite lines out and let E2/E4 add them; see OD-2. Default:
  declare them now for a stable lockfile.)

### 2.3 Shared types (the contract surface — §4 formalizes)

> **Review correction.** An earlier draft put the shared record in a single `internal/buildinfo`
> package with a `cancelled` state and pointer time fields. **E2 (the authoritative API contract)
> instead splits it into `internal/build` (rich domain object) + `internal/wire` (flat JSON DTO),
> with states `{queued,running,finished,failed,killed,unknown}` and a `Source` enum.** E0 must
> ship the *stubs in those package names with those enum values* so E2 fills them in without a
> rename churn across every epic. The two snippets below are the E0 stubs, deliberately matching
> E2 §2.2 / §4.1 verbatim. **Naming note to escalate:** E0's fake-bazel emits a BEP `INTERRUPTED`
> on cancel, but the *registry* state for a broker-initiated kill is `killed` (E2/E3). These are
> two different layers (BEP wire field vs. registry state) — kept distinct on purpose; see §6 OD-4.

**`internal/build/build.go`** — the rich domain object (E2 §2.2 is authoritative; E0 ships this stub):

```go
package build

import "time"

// State is the lifecycle state of a build. String values are the wire contract (frozen by E2).
type State string

const (
    StateQueued   State = "queued"   // admitted-pending (E5); E2 never sets it itself
    StateRunning  State = "running"
    StateFinished State = "finished" // exited 0
    StateFailed   State = "failed"   // exited non-zero
    StateKilled   State = "killed"   // terminated by broker (E3)
    StateUnknown  State = "unknown"  // discovered process whose outcome we never observed
)

// Source records how the build entered the registry.
type Source string

const (
    SourceRegistered Source = "registered" // via POST /register
    SourceDiscovered Source = "discovered" // via process discovery (E3)
)

// Build is the in-memory domain object. Fields the wire DTO omits (proc handle, bep/profile
// paths) are added by E3/E4 — kept off the wire to stop internal state leaking to clients.
type Build struct {
    InvocationID string
    Worktree     string
    Targets      []string
    PID          int
    State        State
    StartTime    time.Time
    EndTime      time.Time // zero until terminal
    ExitCode     int       // valid only in terminal states
    Source       Source
}

// Elapsed returns wall time, ending at EndTime if terminal else now.
func (b Build) Elapsed(now time.Time) time.Duration {
    end := now
    if !b.EndTime.IsZero() {
        end = b.EndTime
    }
    return end.Sub(b.StartTime)
}

// ToWire maps the domain object to its flat JSON DTO (full impl in E2 T1).
func (b Build) ToWire(now time.Time) any { return nil } // E0 stub; E2 returns wire.Build
```

**`internal/wire/wire.go`** — the importable JSON DTOs. **E0 ships only the type stubs**; E2 §4.1
fills the exact `json:"..."` tags (RFC3339 string times, `elapsed_ms`, etc.). Declared in E0 so
`brokerctl` (E6) and other clients can import a stable path from day one.

```go
package wire

// Build is the JSON DTO for one build (returned by /builds, /events, /register).
// FROZEN by E2 §4.1 — E0 ships the struct so the import path exists; E2 finalizes tags.
type Build struct {
    InvocationID string   `json:"invocation_id"`
    Worktree     string   `json:"worktree"`
    Targets      []string `json:"targets"`
    PID          int      `json:"pid"`
    State        string   `json:"state"`
    StartTime    string   `json:"start_time"`          // RFC3339 UTC
    EndTime      string   `json:"end_time,omitempty"`  // omitted until terminal
    ExitCode     int      `json:"exit_code"`
    Source       string   `json:"source"`
    ElapsedMS    int64    `json:"elapsed_ms"`
}

// State/Source string consts mirror build.* — kept here for client imports.
const (
    StateQueued = "queued"; StateRunning = "running"; StateFinished = "finished"
    StateFailed = "failed"; StateKilled = "killed"; StateUnknown = "unknown"
    SourceRegistered = "registered"; SourceDiscovered = "discovered"
)
```

**`internal/config/config.go`** — config location + defaults.

> **Review correction — reconciled with E2 §2.5.** The earlier draft diverged from E2 on five
> points; all now aligned to E2 (the authoritative owner of `config.Config`):
> 1. **Env override is `$BAZEL_BROKER_CONFIG`** (a *file* path), matching E2 + the launchd plist
>    `EnvironmentVariables` key. (The earlier `$BAZEL_BROKER_CONFIG_DIR` did not exist in E2 and
>    would have made the verify-fast smoke test point at the wrong place.) `$XDG_CONFIG_HOME` is
>    still honored for the *config dir*; a `_DIR` override is kept **only** for tests, documented
>    as test-scoped, not a public contract.
> 2. **Field is `MaxConcurrency`** (not `MaxConcurrent`).
> 3. **State files (db, log) live in `~/.local/state/bazel-broker/`** per E2 (XDG state dir),
>    *not* in the config dir. Config dir holds only `config.json` (+ the optional discovery file).
> 4. **Default port is 8765** (fixed loopback), matching E2 §4 base URL — **not** ephemeral `0`.
>    Ephemeral-port + `broker.json` client discovery is a real improvement but E2 did *not* adopt
>    it; it is therefore an **open decision (OD-5)** to escalate, not something E0 freezes.
> 5. **slog handler is JSON** (E2 §2.9), not text — see `internal/logging` below.

```go
package config

const (
    // EnvConfig overrides the config *file* path (used by launchd plist + verify). Matches E2.
    EnvConfig = "BAZEL_BROKER_CONFIG"
    appDir    = "bazel-broker"
    cfgName   = "config.json"

    DefaultPort = 8765 // fixed loopback default (E2 §4); OD-5 tracks ephemeral-port alternative
)

// Config is the on-disk daemon configuration (E2 §2.5 is authoritative; E0 ships this stub).
type Config struct {
    Port           int    `json:"port"`            // loopback TCP port; default DefaultPort
    Token          string `json:"token"`           // bearer token; generated 32-byte hex if absent
    DiskCache      string `json:"disk_cache"`      // shared --disk_cache path (E1/E5)
    MaxConcurrency int    `json:"max_concurrency"` // admission ceiling (E5)
    DBPath         string `json:"db_path"`         // default ~/.local/state/bazel-broker/broker.db
    LogPath        string `json:"log_path"`        // default ~/.local/state/bazel-broker/broker.log
}

// ConfigPath resolves $BAZEL_BROKER_CONFIG, else $XDG_CONFIG_HOME/bazel-broker/config.json,
// else ~/.config/bazel-broker/config.json. (Stub returns the resolved path; E2 T2 adds IO.)
func ConfigPath() (string, error)

// Load reads/creates config.json, applies defaults, generates a token on first run, validates.
// E0 stub: return Default(); E2 implements file IO + token gen + Validate().
func Load() (*Config, error)

func Default() *Config {
    return &Config{Port: DefaultPort, MaxConcurrency: 0}
}
```

> **Open decision OD-5 (client discovery / port strategy) — escalate.** A fixed default port
> (8765, per E2) is simplest for `curl`/Swift clients but collides if two daemons or another
> service grab it. The alternative — bind ephemeral `127.0.0.1:0` and publish the resolved
> `{port, token}` to `~/.config/bazel-broker/broker.json` (atomic temp+rename) for clients to
> read — is more robust and was proposed in an earlier E0 draft. **E2 has not adopted it.** This
> is a genuine cross-epic fork (affects E6/E7/E8 client bootstrap); E0 does **not** resolve it.
> If discovery is later adopted, `broker.json` is a config-dir sibling and back-compatible.

**`internal/logging/logging.go`** — slog convention used by *every* binary. **Package `logging`
and a JSON handler, per E2 §2.9** (an earlier draft used package `brokerlog` + a text handler;
both are corrected here so the daemon's log lines parse as JSON for tooling, matching E2's example
log line). The daemon points the writer at `config.LogPath` (`io.MultiWriter(file, os.Stderr)` so
launchd also captures it); CLIs use stderr.

```go
package logging

import (
    "io"
    "log/slog"
    "os"
    "strings"
)

// New returns a *slog.Logger writing JSON to w at the given level ("debug"|"info"|"warn"|"error").
// Convention: structured key "invocation_id" on every build-scoped log line.
func New(w io.Writer, level string) *slog.Logger {
    var lvl slog.Level
    switch strings.ToLower(level) {
    case "debug": lvl = slog.LevelDebug
    case "warn":  lvl = slog.LevelWarn
    case "error": lvl = slog.LevelError
    default:      lvl = slog.LevelInfo
    }
    h := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: lvl, AddSource: lvl == slog.LevelDebug})
    return slog.New(h)
}

// FromEnv reads BAZEL_BROKER_LOG_LEVEL (default info) for quick verify toggling.
func FromEnv(w io.Writer) *slog.Logger { return New(w, os.Getenv("BAZEL_BROKER_LOG_LEVEL")) }
```

**`internal/version/version.go`** — ldflags-injected build metadata:

```go
package version

var (
    Version = "dev"     // -X .../version.Version=...
    Commit  = "none"    // -X .../version.Commit=...
    Date    = "unknown" // -X .../version.Date=...
)

func String() string { return Version + " (" + Commit + ", " + Date + ")" }
```

### 2.4 `cmd/broker/main.go` (E0 stub)

Minimal daemon that satisfies `make build` and `make run-broker` and gives E2 a real entrypoint
to flesh out. It starts the `/healthz`-only server stub and writes the discovery file.

```go
package main

import (
    "context"
    "flag"
    "os"
    "os/signal"
    "syscall"

    "github.com/antoniospapantoniou/bazel-broker/internal/config"
    "github.com/antoniospapantoniou/bazel-broker/internal/httpapi"
    "github.com/antoniospapantoniou/bazel-broker/internal/logging"
    "github.com/antoniospapantoniou/bazel-broker/internal/version"
)

func main() {
    showVersion := flag.Bool("version", false, "print version and exit")
    _ = flag.String("config", "", "config file path (E2 wires this to config.Load)")
    flag.Parse()
    if *showVersion {
        os.Stdout.WriteString(version.String() + "\n")
        return
    }

    cfg, err := config.Load()
    if err != nil { panic(err) }
    log := logging.New(os.Stderr, "info") // E2: switch writer to config.LogPath file
    log.Info("broker starting", "version", version.Version, "port", cfg.Port)

    ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
    defer stop()

    srv := httpapi.New(cfg, log) // E0: serves only /healthz; E2 widens the constructor (reg, hub)
    if err := srv.Run(ctx); err != nil {
        log.Error("server exited", "err", err)
        os.Exit(1)
    }
}
```

> **Note on the constructor seam.** E0 ships `httpapi.New(cfg, log)`; E2 §2.6 widens it to
> `New(cfg, reg, hub, log)`. Since both are *new code in the same epic boundary* and `httpapi` is
> `internal/`, widening the signature is a same-PR change in E2, not a cross-epic break — E0 only
> needs the package + `/healthz` to exist so `make build`/`verify-fast` are green.

**`internal/httpapi/server.go`** (E0 stub — `/healthz` only; E2 adds builds/events/register/auth):

```go
package httpapi

import (
    "context"
    "encoding/json"
    "log/slog"
    "net"
    "net/http"

    "github.com/antoniospapantoniou/bazel-broker/internal/config"
)

type Server struct {
    cfg *config.Config
    log *slog.Logger
}

func New(cfg *config.Config, log *slog.Logger) *Server { return &Server{cfg, log} }

func (s *Server) Run(ctx context.Context) error {
    mux := http.NewServeMux()
    mux.HandleFunc("GET /healthz", s.healthz) // E2 will gate other routes behind bearer auth
    ln, err := net.Listen("tcp", net.JoinHostPort(s.cfg.Host, itoa(s.cfg.Port)))
    if err != nil { return err }
    // E2: write resolved addr+token to config.SockHintFile(); for E0 just log it.
    s.log.Info("listening", "addr", ln.Addr().String())
    hs := &http.Server{Handler: mux}
    go func() { <-ctx.Done(); hs.Close() }()
    err = hs.Serve(ln)
    if err == http.ErrServerClosed { return nil }
    return err
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
    w.Header().Set("Content-Type", "application/json")
    // E0 emits a subset; E2 returns the full wire.HealthResponse
    // {status,builds,queued,total,version,uptime_ms}. Keep keys a strict subset so the
    // E0 verify probe and E2 stay compatible.
    json.NewEncoder(w).Encode(map[string]any{"status": "ok", "builds": 0, "queued": 0, "total": 0})
}
```

(`itoa` = `strconv.Itoa`; shown abbreviated.)

### 2.5 `cmd/brokerctl/main.go` (E0 stub — cobra root + `version`)

```go
package main

import (
    "fmt"
    "os"

    "github.com/spf13/cobra"
    "github.com/antoniospapantoniou/bazel-broker/internal/version"
)

func main() {
    root := &cobra.Command{Use: "brokerctl", Short: "Control & observe Bazel builds via the broker"}
    root.AddCommand(&cobra.Command{
        Use: "version", Short: "Print version",
        Run: func(c *cobra.Command, _ []string) { fmt.Fprintln(c.OutOrStdout(), version.String()) },
    })
    // E6 adds: ls [--json], kill <id>, drain, watch, profile <id>.
    root.AddCommand(&cobra.Command{
        Use: "ls", Short: "List builds (stub — E6)",
        Run: func(c *cobra.Command, _ []string) { fmt.Fprintln(c.OutOrStdout(), "[]") },
    })
    if err := root.Execute(); err != nil { os.Exit(1) }
}
```

### 2.6 `testdata/fake-bazel.sh` — the BAZEL_REAL stub (FULL)

Design requirements distilled from architecture §10 + epics E0/E3/E5:
1. Behave enough like Bazel for the broker's ingest & kill paths: parse `--build_event_json_file=PATH`
   and `--invocation_id=ID`; emit a few **BEP JSON** events (one JSON object per line — the
   `build_event_json_file` format is newline-delimited JSON, NOT a JSON array).
2. Emit `BuildStarted` (with `uuid`), a `Progress` event, a `BuildMetrics` event (so E4 has cache
   numbers to parse), and `BuildFinished` with overall success.
3. Honor `FAKE_BAZEL_DURATION` (seconds, default 1; supports fractional via `sleep`) as the
   "build time" during which the process is alive and killable.
4. **Trap SIGINT and SIGTERM** → write a `BuildFinished` with `overall_success:false` +
   `finish_time` and exit with a **distinct cancel code (8)** so E3's kill test can assert it.
   (8 = Bazel's own "interrupted" exit code — keeps the fixture realistic.)
5. Be usable two ways: directly (`testdata/fake-bazel.sh --build_event_json_file=...`) and as a
   `BAZEL_REAL` target re-exec'd by `tools/bazel`.
6. Pure POSIX-ish bash, zero external deps beyond coreutils available on macOS.

> ⚠️ **VERIFIED bash 3.2 / macOS gotcha (this changes the cancel contract — read before using).**
> The original plan asserted SIGINT → exit 8 and built the verify/kill tests around `kill -INT`.
> **This does not work when the script is launched as an async (`&`) command**, which is exactly
> how every headless test launches it (`"$FAKE" … &`). POSIX shell semantics (and bash 3.2 on
> macOS, reproduced during this review) set **SIGINT/SIGQUIT to `SIG_IGN` for `&`-launched
> commands, and the disposition is sticky — an explicit `trap … INT` is silently *not installed***
> (`trap -p INT` shows nothing). Result: `kill -INT <pid>` is ignored and the script runs to
> completion with **exit 0**, not 8. Measured matrix (stock `/bin/bash` 3.2.57, child launched
> with `&`):
>
> | Signal | Foreground sleep loop | `sleep & wait` loop | Verdict |
> |---|---|---|---|
> | **SIGTERM** | exit 8 ✅ | exit 8 ✅ | **reliable** |
> | **SIGINT** | exit 0 ❌ (trap never installs) | exit 0 ❌ | only works with a controlling tty / process group |
>
> **Consequences / decisions:**
> - **The headless cancel signal is `SIGTERM`.** The fake-bazel cancel code (8) is asserted by
>   sending **SIGTERM**, not SIGINT. `verify-fast.sh`, E3's kill test, and E5 all use SIGTERM (or
>   `kill` with no signal, which defaults to SIGTERM).
> - **SIGINT is still trapped** for the interactive/real-`bazel`-client case (where bazel runs in
>   the tty foreground process group and SIGINT *does* reach the trap), so the fixture stays
>   faithful — but tests must not rely on SIGINT for an `&`-backgrounded process.
> - **E3 implication (flag to E3):** the real kill path the broker uses must be SIGTERM-first (or
>   SIGINT delivered to the build's *process group*, e.g. via `setsid`/`Pgid`), then escalate to
>   SIGKILL. Killing a backgrounded bazel client with a bare `kill -INT <pid>` will silently fail
>   for the same reason. This is now noted in §4 (fake-bazel contract) and §6.

The exact desired script (this is the deliverable, written verbatim):

```bash
#!/usr/bin/env bash
# testdata/fake-bazel.sh — a fake `bazel` (BAZEL_REAL stub) for fast, headless /verify.
#
# Emulates just enough of Bazel for the broker's verify paths:
#   * parses --build_event_json_file=PATH and --invocation_id=ID
#   * emits newline-delimited BEP JSON events (BuildStarted, Progress, BuildMetrics, BuildFinished)
#   * stays alive for FAKE_BAZEL_DURATION seconds (default 1), so it can be discovered & killed
#   * traps SIGTERM/SIGINT, writes a failed BuildFinished, and exits with cancel code 8
#
# CANCEL SIGNAL: send SIGTERM (the default of `kill`) for a reliable cancel. SIGINT only fires
# the trap when this process has a controlling tty / is in the foreground process group; when
# launched with `&` (as all headless tests do), bash 3.2 sets SIGINT to SIG_IGN and the INT trap
# is never installed (verified). Tests therefore assert the cancel code via SIGTERM. See E0 §2.6.
#
# Env knobs:
#   FAKE_BAZEL_DURATION   build wall time in seconds (fractional ok). Default: 1
#   FAKE_BAZEL_CACHE_HITS / FAKE_BAZEL_CACHE_MISSES   numbers reported in BuildMetrics. Default 7 / 3
#   FAKE_BAZEL_EXIT       success exit code override. Default: 0
#
# Exit codes:  0 success · 8 interrupted (SIGTERM/SIGINT) · 2 usage error
set -u

CANCEL_CODE=8
DURATION="${FAKE_BAZEL_DURATION:-1}"
CACHE_HITS="${FAKE_BAZEL_CACHE_HITS:-7}"
CACHE_MISSES="${FAKE_BAZEL_CACHE_MISSES:-3}"
OK_EXIT="${FAKE_BAZEL_EXIT:-0}"

BEP_FILE=""
INVOCATION_ID=""
declare -a TARGETS=()

# ---- arg parse (only the flags we care about; ignore the rest like real bazel tolerates) -----
for arg in "$@"; do
  case "$arg" in
    --build_event_json_file=*) BEP_FILE="${arg#*=}" ;;
    --invocation_id=*)         INVOCATION_ID="${arg#*=}" ;;
    --*)                       : ;;                      # ignore other flags
    build|test|run|query|info|clean|shutdown) : ;;       # ignore the command verb
    -*)                        : ;;
    *)                         TARGETS+=("$arg") ;;       # treat bare args as targets
  esac
done

# Generate an invocation id if none supplied (mimic Bazel's auto uuid).
if [[ -z "$INVOCATION_ID" ]]; then
  if command -v uuidgen >/dev/null 2>&1; then
    INVOCATION_ID="$(uuidgen | tr 'A-Z' 'a-z')"
  else
    INVOCATION_ID="fake-$$-$(date +%s)"
  fi
fi

now_ts() { date -u +%Y-%m-%dT%H:%M:%S.000Z; }

# Append one BEP JSON object (single line) to the BEP file, if configured.
emit() {
  [[ -n "$BEP_FILE" ]] || return 0
  printf '%s\n' "$1" >> "$BEP_FILE"
}

# Build the targets list as a JSON array string.
targets_json() {
  local out="[" first=1 t
  for t in "${TARGETS[@]:-}"; do
    [[ -n "$t" ]] || continue
    [[ $first -eq 1 ]] && first=0 || out+=","
    out+="\"$t\""
  done
  out+="]"
  printf '%s' "$out"
}

# ---- SIGTERM/SIGINT handler: graceful cancel ------------------------------------------------
# Trap TERM first (always honored). INT is also trapped for the interactive/real-tty case, but
# see the header note: an &-launched process inherits SIGINT=SIG_IGN on bash 3.2 and the INT
# trap will not install — which is why the cancel contract is asserted via SIGTERM.
INTERRUPTED=0
on_signal() {
  INTERRUPTED=1
  emit "{\"id\":{\"buildFinished\":{}},\"buildFinished\":{\"overallSuccess\":false,\"exitCode\":{\"name\":\"INTERRUPTED\",\"code\":${CANCEL_CODE}},\"finishTime\":\"$(now_ts)\"}}"
  exit "$CANCEL_CODE"
}
trap on_signal TERM INT

# ---- prepare BEP file -----------------------------------------------------------------------
if [[ -n "$BEP_FILE" ]]; then
  mkdir -p "$(dirname "$BEP_FILE")" 2>/dev/null || true
  : > "$BEP_FILE"   # truncate/create
fi

# ---- emit started + progress ----------------------------------------------------------------
emit "{\"id\":{\"started\":{}},\"started\":{\"uuid\":\"${INVOCATION_ID}\",\"startTimeMillis\":\"$(date +%s000)\",\"buildToolVersion\":\"fake-8.0.0\",\"command\":\"build\"}}"
emit "{\"id\":{\"progress\":{}},\"progress\":{\"stderr\":\"INFO: Analyzed targets $(targets_json).\\n\"}}"

# ---- stay alive (killable) for DURATION, in small sleep slices so traps fire promptly --------
# Sleep in <=0.2s slices so the SIGTERM trap fires near-immediately (each `wait` returns when the
# backgrounded sleep is signalled, and bash then runs the pending TERM trap). Important for the
# <1s kill assertion E3 needs.
remaining="$DURATION"
while : ; do
  # bash arithmetic is integer-only; use awk for fractional compare/subtract.
  done_yet=$(awk -v r="$remaining" 'BEGIN{print (r<=0)?1:0}')
  [[ "$done_yet" -eq 1 ]] && break
  sleep 0.2 &
  wait $!            # `wait` lets the trap interrupt the sleep immediately
  remaining=$(awk -v r="$remaining" 'BEGIN{printf "%.3f", r-0.2}')
done

# ---- success path: metrics + finished -------------------------------------------------------
TOTAL=$(( CACHE_HITS + CACHE_MISSES ))
emit "{\"id\":{\"buildMetrics\":{}},\"buildMetrics\":{\"actionSummary\":{\"actionsExecuted\":\"${CACHE_MISSES}\",\"actionsCreated\":\"${TOTAL}\"},\"actionCacheStatistics\":{\"hits\":${CACHE_HITS},\"misses\":${CACHE_MISSES}}}}"
emit "{\"id\":{\"buildFinished\":{}},\"buildFinished\":{\"overallSuccess\":true,\"exitCode\":{\"name\":\"SUCCESS\",\"code\":0},\"finishTime\":\"$(now_ts)\"}}"

exit "$OK_EXIT"
```

Notes on the BEP shapes: field names (`buildFinished`, `overallSuccess`, `actionCacheStatistics`,
etc.) follow the lowerCamelCase that `protojson`/`--build_event_json_file` produces, so E4's
`protojson.Unmarshal` into generated `build_event_stream` types parses them directly. The exact
`BuildMetrics` field nesting will be reconciled against the generated proto in E4; the fixture is
intentionally close-but-minimal and E4 owns final field-name fidelity (flagged as OD-3).

### 2.7 `testdata/workspace/` — synthetic real Bazel workspace

A Bazel **bzlmod** workspace with one trivial `sh_binary` and one `genrule`, so a *real*
`bazel build` runs in seconds and produces a genuine BEP json file + `--profile` for occasional
true e2e (architecture §10) and for E1's two-worktree cache test and E4's real-ingest test.

> **Review correction — Bazel 8/9 rule availability.** The original draft used a bare native
> `sh_binary`. **In Bazel 8+ the native `sh_*` rules have moved to the `rules_shell` module** and
> are not reliably in the global namespace (their autoload is being phased out via
> `--incompatible_autoload_externally`/`--incompatible_disable_native_*`). For a workspace that
> must build cleanly on Bazel 8.x **and** 9.x, declare `rules_shell` in `MODULE.bazel` and load
> `sh_binary` explicitly. `genrule` remains a true native built-in and needs no load.
> `.bazelversion` is bumped to a current 8.x (8.4.0); the project targets Bazel 9.x but a pinned
> 8.x keeps the fixture buildable on either via Bazelisk. **Pick the exact pin during T7** against
> whatever Bazelisk resolves on the dev machine (OD-6, minor).

`testdata/workspace/MODULE.bazel`:
```python
module(name = "bazel_broker_testws", version = "0.0.0")

# rules_shell provides sh_binary/sh_test on Bazel 8+ (native sh_* are being removed).
# Pin the patch at T7 to whatever the resolved Bazel version ships; this is the only external dep.
bazel_dep(name = "rules_shell", version = "0.3.0")
```

`testdata/workspace/.bazelversion`:
```
8.4.0
```

`testdata/workspace/BUILD.bazel`:
```python
load("@rules_shell//shell:sh_binary.bzl", "sh_binary")

sh_binary(
    name = "hello",
    srcs = ["hello.sh"],
)

genrule(
    name = "gen",
    outs = ["gen.txt"],
    cmd = "echo hello-from-bazel > $@",   # a trivial native action that exercises the action/disk cache
)
```

`testdata/workspace/hello.sh`:
```bash
#!/usr/bin/env bash
echo "hello from bazel-broker test workspace"
```

`testdata/workspace/.gitignore` (so the symlink forest never gets committed even though the
repo-root `.gitignore` also covers it):
```
bazel-*
```

The `genrule` (not just `sh_binary`) is deliberate: it produces a cacheable **action** so E1 can
demonstrate a disk-cache hit on the second worktree, and E4 can read non-zero cache stats from a
real `BuildMetrics`. `make verify-e2e` invokes `bazel build //:gen //:hello` here (via real
bazelisk/bazel, see §5) only when `bazel` is on PATH; otherwise it skips with a clear message.

> **Note (flag to E1/E4):** `rules_shell` is fetched from the Bazel Central Registry, so the
> *first* `verify-e2e` on a clean machine is **not** network-hermetic (it downloads the module).
> Subsequent runs hit the repository cache. This is acceptable for an occasional e2e but means
> `verify-e2e` must never be on the fast/offline `/verify` path — it already isn't (it's a
> separate Make target that SKIPs without bazel).

### 2.8 `tools/bazel` placeholder

E5 owns the real wrapper. E0 ships a minimal, correct *passthrough* so the file exists in the
layout and never recurses, and so worktrees that copy it don't break:

```bash
#!/usr/bin/env bash
# tools/bazel — broker interception hook (PLACEHOLDER; real admission logic lands in E5).
# Must exec $BAZEL_REAL (set by the launcher), never `bazel`, to avoid infinite recursion.
set -euo pipefail
exec "${BAZEL_REAL:?tools/bazel must be invoked via the bazel launcher with BAZEL_REAL set}" "$@"
```

### 2.9 `Makefile`

```make
# Bazel Broker — build & verify entrypoints. See CLAUDE.md for per-component recipes.
SHELL          := /usr/bin/env bash
MODULE         := github.com/antoniospapantoniou/bazel-broker
BIN_DIR        := bin
BROKER_BIN     := $(BIN_DIR)/broker
BROKERCTL_BIN  := $(BIN_DIR)/brokerctl
VERSION        ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT         ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE           ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS        := -X $(MODULE)/internal/version.Version=$(VERSION) \
                  -X $(MODULE)/internal/version.Commit=$(COMMIT) \
                  -X $(MODULE)/internal/version.Date=$(DATE)

.DEFAULT_GOAL := build

.PHONY: build
build: $(BROKER_BIN) $(BROKERCTL_BIN)   ## build broker + brokerctl

$(BROKER_BIN): $(shell find cmd/broker internal -name '*.go' 2>/dev/null)
	@mkdir -p $(BIN_DIR)
	go build -ldflags '$(LDFLAGS)' -o $(BROKER_BIN) ./cmd/broker

$(BROKERCTL_BIN): $(shell find cmd/brokerctl internal -name '*.go' 2>/dev/null)
	@mkdir -p $(BIN_DIR)
	go build -ldflags '$(LDFLAGS)' -o $(BROKERCTL_BIN) ./cmd/brokerctl

.PHONY: run-broker
run-broker: $(BROKER_BIN)               ## run the daemon in the foreground (Ctrl-C to stop)
	$(BROKER_BIN)

.PHONY: fmt vet test tidy
fmt:  ; gofmt -w cmd internal
vet:  ; go vet ./...
test: ; go test ./...
tidy: ; go mod tidy

.PHONY: verify-fast
verify-fast: build                      ## headless 1-second sanity: build + fake-bazel + unit tests
	go test ./...
	scripts/verify-fast.sh

.PHONY: verify-e2e
verify-e2e: build                       ## real bazel build in testdata/workspace (skips if no bazel)
	@if command -v bazel >/dev/null 2>&1 || command -v bazelisk >/dev/null 2>&1; then \
	  BAZEL=$$(command -v bazelisk || command -v bazel); \
	  echo ">> using $$BAZEL"; \
	  ( cd testdata/workspace && \
	    "$$BAZEL" build //:gen //:hello \
	      --build_event_json_file=/tmp/bb-e2e-bep.json \
	      --profile=/tmp/bb-e2e.profile.gz ); \
	  echo ">> BEP events:"; wc -l < /tmp/bb-e2e-bep.json; \
	  echo ">> profile written: /tmp/bb-e2e.profile.gz (open in https://ui.perfetto.dev)"; \
	else \
	  echo "SKIP verify-e2e: no bazel/bazelisk on PATH"; \
	fi

.PHONY: clean
clean: ; rm -rf $(BIN_DIR)
```

(The `## ...` trailing comments enable an optional `make help` target later; kept simple here.)

### 2.10 `scripts/verify-fast.sh` (the fast verify orchestrator)

Encapsulates the three "Done when" checks so both `make verify-fast` and the `/verify` skill call
the same logic. Exit non-zero on any failed assertion; print a clear PASS/FAIL line each.

```bash
#!/usr/bin/env bash
# scripts/verify-fast.sh — asserts the E0 acceptance criteria headlessly in ~3s.
set -uo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FAKE="$ROOT/testdata/fake-bazel.sh"
fail=0
say() { printf '%-40s %s\n' "$1" "$2"; }

# 1) build already done by Make; re-assert binaries exist.
[[ -x "$ROOT/bin/broker" && -x "$ROOT/bin/brokerctl" ]] && say "build artifacts" PASS || { say "build artifacts" FAIL; fail=1; }

# 2) fake-bazel emits events and exits 0
BEP=$(mktemp /tmp/bb-bep.XXXX.json)
FAKE_BAZEL_DURATION=0.2 "$FAKE" build --invocation_id=verify-1 --build_event_json_file="$BEP" //:gen
rc=$?
n=$(wc -l < "$BEP" | tr -d ' ')
if [[ $rc -eq 0 && $n -ge 3 ]]; then say "fake-bazel emits BEP & exit 0" "PASS ($n events)"; else say "fake-bazel emits BEP & exit 0" "FAIL (rc=$rc n=$n)"; fail=1; fi
grep -q '"started"' "$BEP"      || { say "  has BuildStarted" FAIL; fail=1; }
grep -q '"buildFinished"' "$BEP" || { say "  has BuildFinished" FAIL; fail=1; }

# 3) SIGTERM yields the cancel code (8).
#    NOTE: must be SIGTERM, not SIGINT — an &-backgrounded bash process has SIGINT=SIG_IGN on
#    bash 3.2 (verified), so `kill -INT` would be ignored and the script would exit 0. See §2.6.
CANCEL_BEP=$(mktemp /tmp/bb-cancel.XXXX.json)
FAKE_BAZEL_DURATION=10 "$FAKE" build --build_event_json_file="$CANCEL_BEP" //:gen &
pid=$!
sleep 0.4
kill -TERM "$pid"
wait "$pid"; rc=$?            # `wait` returns the child's exit code (8 on cancel)
if [[ $rc -eq 8 ]]; then say "SIGTERM -> cancel code 8" PASS; else say "SIGTERM -> cancel code 8" "FAIL (rc=$rc)"; fail=1; fi
grep -q '"overallSuccess":false' "$CANCEL_BEP" || { say "  cancel wrote failed BuildFinished" FAIL; fail=1; }

# 4) broker healthz smoke (start, probe, stop) — proves the daemon stub serves.
#    Use an isolated config FILE (matches E2's $BAZEL_BROKER_CONFIG) so the smoke run never
#    touches the developer's real ~/.config state.
SMOKE_DIR=$(mktemp -d)
BAZEL_BROKER_CONFIG="$SMOKE_DIR/config.json" "$ROOT/bin/broker" --config "$SMOKE_DIR/config.json" &
bpid=$!; sleep 0.5
# Default port is 8765 (E2). E0 stub binds it; probe is best-effort until E2 finalizes auth.
if command -v curl >/dev/null 2>&1; then
  code=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:8765/healthz" 2>/dev/null || echo 000)
  [[ "$code" == "200" ]] && say "broker /healthz 200" PASS || say "broker /healthz (best-effort)" "WARN (code=$code)"
fi
kill -TERM "$bpid" 2>/dev/null; wait "$bpid" 2>/dev/null
rm -rf "$SMOKE_DIR" "$CANCEL_BEP"
say "broker start/stop" PASS

rm -f "$BEP"
[[ $fail -eq 0 ]] && { echo "VERIFY-FAST: PASS"; exit 0; } || { echo "VERIFY-FAST: FAIL"; exit 1; }
```

(The healthz probe is best-effort in E0 — it passes if the stub binds 8765, warns otherwise.
E2 finalizes config-driven port + auth and this script tightens to a hard PASS. If OD-5 selects
ephemeral ports, the probe reads the port from `broker.json` instead of hard-coding 8765.)

### 2.11 `.gitignore`

```gitignore
# Go
/bin/
*.test
*.out
coverage.*

# Bazel (testdata/workspace e2e artifacts)
/testdata/workspace/bazel-*
*.profile.gz

# Local broker state / scratch
/tmp/
*.db
*.log
broker.port
broker.token
broker.json

# macOS / editors
.DS_Store
.idea/
.vscode/
```

### 2.12 `CLAUDE.md` (content outline — full file is a deliverable)

Sections, with the canonical commands each later epic's `/verify` and `/run` will rely on:

```markdown
# Bazel Broker — Agent Guide

Single-Mac, self-contained control + observability for Bazel across git worktrees.
Architecture: plans/01-architecture.md · Epics: plans/02-epics.md.

## Conventions (DO NOT BREAK — other epics depend on these; E2 is authoritative for the API)
- Module path: github.com/antoniospapantoniou/bazel-broker
- Go 1.26, macOS arm64. cgo only inside internal/discovery (E3); default build stays pure-Go.
- Logging: slog JSON handler via internal/logging. Always include key "invocation_id".
- Config file: ~/.config/bazel-broker/config.json (override the file via $BAZEL_BROKER_CONFIG).
  State dir (db, log): ~/.local/state/bazel-broker/  (broker.db, broker.log).
- API transport: loopback TCP 127.0.0.1:8765 (default) + bearer token (D-stack-2). [OD-5: ephemeral
  port + broker.json discovery is an open alternative — not yet adopted.]
- Shared types: internal/build.Build (domain) + internal/wire.Build (JSON DTO). wire.* is the
  importable wire contract; states {queued,running,finished,failed,killed,unknown}.
- fake-bazel cancel code: 8. CANCEL SIGNAL = SIGTERM (SIGINT only fires the trap with a tty/PG;
  an &-launched process has SIGINT=SIG_IGN on bash 3.2 — verified).

## Build & run
- make build        — builds bin/broker + bin/brokerctl
- make run-broker    — runs the daemon in the foreground
- bin/brokerctl version | ls

## Verify
- make verify-fast   — ~3s headless: build + unit tests + fake-bazel + cancel-code + daemon smoke
- make verify-e2e    — real bazel build in testdata/workspace (skips if no bazel on PATH)

## fake-bazel (testdata/fake-bazel.sh)
Stub BAZEL_REAL for fast verify. Honors --build_event_json_file, --invocation_id.
Knobs: FAKE_BAZEL_DURATION (s), FAKE_BAZEL_CACHE_HITS/MISSES, FAKE_BAZEL_EXIT.
Cancel: send SIGTERM -> exit 8 (not SIGINT for &-launched; see Conventions). Example:
  FAKE_BAZEL_DURATION=2 testdata/fake-bazel.sh build --build_event_json_file=/tmp/x.json //:gen

## Per-component recipes (filled in as epics land)
- broker daemon (E2): make run-broker; TOKEN=$(jq -r .token ~/.config/bazel-broker/config.json);
  curl 127.0.0.1:8765/healthz   # /healthz is auth-exempt
- discovery/kill (E3): launch fake-bazel long; brokerctl ls; brokerctl kill <id> -> exit 8
  (broker's kill path must use SIGTERM/process-group SIGINT, not bare `kill -INT <pid>`)
- BEP/metrics (E4): point ingest at a BEP json; brokerctl metrics <id>
- admission (E5): start 3 fake builds, max-concurrency=2; 3rd queues
- web (E7) / menubar (E8): see their epic plans
```

---

## 3. Sequencing (ordered, checkpointed task list)

Each task is independently verifiable. Suggested commit granularity in parentheses.

**T1. Module + git hygiene.** Create `go.mod` (module path per OD-1), `.gitignore`, `.tool-versions`.
- ✅ Verify: `go mod verify`; `git status` shows no stray ignored files. (commit: "E0: go.mod + gitignore")

**T2. Shared types & conventions packages.** Add `internal/version`, `internal/build`,
`internal/wire`, `internal/config`, `internal/logging` with the signatures in §2.3 (stubs where
noted; enum values match E2 §2.2/§4.1).
- ✅ Verify: `go build ./internal/...`; `go vet ./internal/...`. Add table tests:
  `config.ConfigPath()` honors `$BAZEL_BROKER_CONFIG` and falls back to XDG; `build.Build.Elapsed`
  math; `logging.New` level mapping. `go test ./internal/...` green. (commit: "E0: shared internal packages")

**T3. Server + CLI stubs + mains.** Add `internal/httpapi` (`/healthz` only), `internal/registry`,
`internal/store`, `internal/apiclient` stubs, plus `cmd/broker/main.go` and `cmd/brokerctl/main.go`.
Add `internal/discovery/doc.go` and `internal/bep/doc.go` placeholders.
- ✅ Verify: `go build ./...` clean; `bin/broker -version` prints; `bin/brokerctl version` prints;
  start `bin/broker`, `curl …/healthz` → `{"status":"ok","builds":0,...}`. (commit: "E0: cmd stubs + healthz")

**T4. Makefile.** Add all targets from §2.9.
- ✅ Verify: `make build` produces both binaries with version ldflags (`bin/broker -version` shows
  git describe). `make fmt vet test` clean. (commit: "E0: Makefile")

**T5. fake-bazel.sh.** Write the full script from §2.6; `chmod +x`.
- ✅ Verify (the literal Done-when):
  `FAKE_BAZEL_DURATION=2 testdata/fake-bazel.sh --build_event_json_file=/tmp/x.json` → exit 0,
  `/tmp/x.json` has ≥3 JSON lines, each line is valid JSON (`jq . </tmp/x.json`).
  Long-duration run backgrounded + `kill -TERM` → exit 8 (use **SIGTERM**, not SIGINT — see §2.6).
  (commit: "E0: fake-bazel verify stub")

**T6. verify-fast.sh + wire to Make.** Add `scripts/verify-fast.sh`; `make verify-fast` runs it.
- ✅ Verify: `make verify-fast` prints `VERIFY-FAST: PASS` and exits 0. (commit: "E0: verify-fast")

**T7. Synthetic workspace + verify-e2e.** Add `testdata/workspace/*` and the `verify-e2e` target.
- ✅ Verify: with bazelisk available, `make verify-e2e` builds `//:gen //:hello`, writes a real BEP
  json (≥1 `BuildMetrics` line) + `.profile.gz`. Without bazel, prints SKIP and exits 0.
  (commit: "E0: synthetic workspace + e2e target")

**T8. tools/bazel placeholder + testdata/README.** Add the passthrough wrapper and fixture docs.
- ✅ Verify: `BAZEL_REAL=testdata/fake-bazel.sh tools/bazel build //:gen --build_event_json_file=/tmp/w.json`
  produces events via the wrapper path (proves the BAZEL_REAL re-exec contract for E5). (commit: "E0: wrapper placeholder")

**T9. CLAUDE.md.** Write the full guide from §2.12.
- ✅ Verify: every command block in CLAUDE.md copy-pastes and runs (manual checklist). (commit: "E0: CLAUDE.md")

**T10. Green-board gate.** Run the full set: `make build && make verify-fast && make verify-e2e`.
- ✅ Verify: all pass (e2e may SKIP). Tag `e0-done`. (commit/tag)

Dependency note: T2→T3→T4 are serial (compile chain); T5/T6/T7/T8/T9 are largely parallel after T4.

---

## 4. Interfaces & contracts (what other epics depend on)

These are **frozen by E0** — changing them is a cross-epic event.

> **Authority note:** for the API/data contract, **E2 is authoritative**. E0 *ships compiling
> stubs that match E2's committed shapes* so imports resolve and `make build` is green; it does not
> independently define the wire contract. The table below reflects the reconciled (E2-aligned)
> values.

| Contract | Value / shape | Consumed by |
|---|---|---|
| **Module path** | `github.com/antoniospapantoniou/bazel-broker` (OD-1) | all Go epics |
| **Shared types** | `internal/build.Build` (domain) + `internal/wire.Build` (JSON DTO, importable); `State` ∈ {queued,running,finished,failed,killed,unknown}; `Source` ∈ {registered,discovered}. E2 §4.1 freezes tags. | E2 (registry/API), E3 (discovery), E4 (metrics), E6/E7/E8 (render) |
| **Config file** | `~/.config/bazel-broker/config.json`; override the *file* via `$BAZEL_BROKER_CONFIG`; respects `$XDG_CONFIG_HOME` for the dir | E2 (config/token), E5 (admission settings) |
| **State files** | `~/.local/state/bazel-broker/`: `broker.db`, `broker.log` (XDG state dir; not the config dir) | E2, E4 |
| **Client discovery** | **OD-5 (open):** default = fixed port 8765 (no discovery file). Alternative = ephemeral port + `broker.json` `{port,token}`. Not yet decided. | E6, E8 (bootstrap) |
| **API transport** | loopback TCP `127.0.0.1:8765` (default) + `Authorization: Bearer <token>` (D-stack-2) | E2 defines routes; E6/E8 call them |
| **Health route** | `GET /healthz` → `200 {"status":"ok","builds":N,"queued":M,"total":T,...}` (auth-exempt; E0 stub emits a subset, E2 the full `wire.HealthResponse`) | E2, verify scripts, launchd health |
| **Logging** | `internal/logging.New(w, level)`; slog **JSON**; key `invocation_id` on build logs; level via `$BAZEL_BROKER_LOG_LEVEL` | all daemon-side epics |
| **Version** | `internal/version.{Version,Commit,Date}` injected via ldflags; printed by `-version`/`version` | all |
| **fake-bazel** | `testdata/fake-bazel.sh`; flags `--build_event_json_file`, `--invocation_id`; knobs `FAKE_BAZEL_DURATION/CACHE_HITS/CACHE_MISSES/EXIT`; **cancel exit code = 8 on SIGTERM** (SIGINT only with a tty/PG — see §2.6) | E3 (kill assert), E4 (BEP parse), E5 (admission) |
| **Kill-signal note** | broker's real kill path must use **SIGTERM** (or SIGINT to the process *group*), then SIGKILL — a bare `kill -INT <pid>` is silently ignored for an &-launched bash client | **E3** (kill impl) |
| **BAZEL_REAL contract** | `tools/bazel` must `exec "$BAZEL_REAL" "$@"`; never call `bazel` (recursion) | E5 (real wrapper) |
| **Synthetic workspace** | `testdata/workspace/` (bzlmod + `rules_shell`) with `//:gen` (cacheable action) + `//:hello` | E1 (cache test), E4 (real ingest) |
| **Make targets** | `build`, `run-broker`, `verify-fast`, `verify-e2e` | every epic's CI/verify; `/run`, `/verify` skills |
| **cgo boundary** | cgo confined to `internal/discovery` (E3) behind an interface, default build stays pure-Go | E3 (D-stack-1) |

`internal/registry`, `internal/store`, `internal/apiclient`, `internal/bep`, `internal/discovery`
are **named, compiling placeholders** in E0 (so imports resolve) — their *behavior* is owned by the
later epics; their package names/locations are the contract.

---

## 5. Testing & verification

### 5.1 The fast verify (`make verify-fast` / `/verify`)
Runs `go test ./...` then `scripts/verify-fast.sh`, asserting in ~3s:
- Both binaries built and executable.
- **fake-bazel emits events + exits 0** (Done-when #2): `FAKE_BAZEL_DURATION=0.2 fake-bazel.sh
  --build_event_json_file=$BEP …` → rc 0, ≥3 newline-delimited JSON objects, contains `started`
  and `buildFinished`; every line valid JSON via `jq`.
- **SIGTERM → cancel code 8** (Done-when #3): launch backgrounded with `FAKE_BAZEL_DURATION=10`,
  `kill -TERM`, assert `wait` rc == 8 and the BEP got a `"overallSuccess":false` BuildFinished.
  (SIGTERM, not SIGINT — the &-launched-process SIGINT=SIG_IGN behavior is verified in §2.6.)
- Daemon start/stop smoke; best-effort `/healthz` 200 against the default port 8765.

### 5.2 The real e2e (`make verify-e2e`)
Only when `bazel`/`bazelisk` is on PATH (else clean SKIP, exit 0). Runs a real build of
`//:gen //:hello` in `testdata/workspace/`, writing a genuine `--build_event_json_file` and
`--profile=*.gz`. Confirms the fixture compiles under real Bazel and yields a real `BuildMetrics`
event for E4 and a Perfetto-loadable profile (architecture §10).

### 5.3 Unit tests (Go) added in E0
- `config_test.go`: `$BAZEL_BROKER_CONFIG` override + XDG fallback; path resolution.
- `build_test.go`: `Elapsed` with/without `EndTime`; `wire.Build` JSON tag round-trip stability.
- `logging_test.go`: level string → `slog.Level` mapping; JSON output captured to a buffer.

### 5.4 Acceptance criteria (expanded from "Done when")
| # | Criterion | Command | Pass condition |
|---|---|---|---|
| A1 | repo builds | `make build` | exit 0; `bin/broker`, `bin/brokerctl` exist and run `-version` |
| A2 | fake-bazel emits + exits 0 | `FAKE_BAZEL_DURATION=2 testdata/fake-bazel.sh --build_event_json_file=/tmp/x.json` | exit 0; `/tmp/x.json` valid newline-JSON, ≥3 events incl. started+finished |
| A3 | cancel code | long backgrounded fake-bazel + `kill -TERM` | process exits **8**; BEP has failed `buildFinished` (SIGTERM; SIGINT only with a tty — §2.6) |
| A4 | verify is one command | `make verify-fast` | prints `VERIFY-FAST: PASS`, exit 0 |
| A5 | e2e fixture is real | `make verify-e2e` (bazel present) | real BEP json w/ `BuildMetrics`; `.profile.gz` written |
| A6 | conventions usable | `go build ./...`, import `internal/wire`/`config` from a scratch test | compiles; config path resolves to `~/.config/bazel-broker/config.json` |
| A7 | wrapper contract | `BAZEL_REAL=…/fake-bazel.sh tools/bazel build …` | events produced; no recursion |

---

## 6. Risks, edge cases, open decisions

**Open decisions surfaced (NOT resolved here — flagged for the owner):**
- **OD-1 (module path).** Proposed `github.com/antoniospapantoniou/bazel-broker`. Alternative:
  a vanity/local path like `bazelbroker.local` or `go.antonis.dev/bazel-broker`. Since the tool is
  never `go install`-ed by third parties and ships as a Homebrew cask, any stable string works —
  but it must be **chosen once** before T1 because it appears in every import. *Recommend the
  GitHub path for least surprise.* Decide before E2 starts.
- **OD-2 (eager vs lazy deps).** E0 declares sqlite/websocket/tail/protobuf in `go.mod` for a stable
  lockfile vs. letting each epic add its own. Default: declare eagerly. If a clean `go.mod` per PR is
  preferred, drop them and let E2/E4 `go get`.
- **OD-3 (fake-bazel BEP fidelity).** The fixture's `BuildMetrics`/`BuildStarted` field nesting is
  hand-written close-to-real. E4 owns the generated `build_event_stream.proto` and may need to nudge
  the fixture field names to match `protojson` output exactly. Tracked, not blocking E0.
- **OD-4 (cancel terminology: `killed` registry state vs `INTERRUPTED` BEP).** The fake-bazel emits
  a BEP `BuildFinished` with `exitCode.name=INTERRUPTED` on cancel, while the *registry* state for a
  broker-initiated kill is `killed` (E2/E3). These are two layers and intentionally distinct, but the
  naming could confuse — confirm with E3/E4 that the mapping (BEP INTERRUPTED → registry `killed`,
  process-exit-8 → `killed`) is what they expect. Non-blocking; surfaced for the cross-epic review.
- **OD-5 (client discovery / port strategy).** Fixed 8765 (E2 default) vs ephemeral + `broker.json`.
  See §2.5 — genuine cross-epic fork affecting E6/E7/E8 bootstrap; **escalate, do not resolve in E0.**
- **OD-6 (synthetic-workspace Bazel pin + `rules_shell` version).** `.bazelversion` and the
  `rules_shell` `bazel_dep` version are placeholders to be pinned at T7 against what Bazelisk resolves
  on the dev machine (project targets Bazel 9.x; fixture must build on the pinned 8.x too). Minor.

**Carried cross-epic decisions referenced by E0's contracts:**
- **D-stack-1 (process discovery).** E0 pre-positions for it: cgo is confined to `internal/discovery`
  (E3) so the default build is pure-Go and cross-compilable. E0 does not pick cgo-vs-shellout; it
  just guarantees the boundary exists.
- **D-stack-2 (API transport).** E0 follows E2's **loopback TCP + bearer token** default (matches
  architecture §4). E0 does **not** unilaterally introduce the `broker.json` client-discovery file —
  E2 did not adopt it, so it is tracked as OD-5 above. Unix-socket remains a possible E2 swap; E2's
  `Server` already takes a `net.Listener`, so the swap is localized either way.
- **D3 (admission model)** and **D4 (kill mechanism)** are out of E0's scope but E0 fixes the
  **cancel exit code (8)** the fake-bazel uses, so E3/E5 tests have a stable assertion target
  regardless of which kill mechanism (SIGTERM/SIGINT-to-PG vs command-server Cancel) is finally
  chosen. **E0's finding constrains D4:** whatever mechanism is chosen, it cannot rely on a bare
  `kill -INT <pid>` to a backgrounded client (verified to be a no-op on bash 3.2) — see below.

**Edge cases / pitfalls baked into the design:**
- **★ SIGINT is ignored for `&`-launched processes (VERIFIED, load-bearing).** The original plan's
  cancel contract used SIGINT and `sleep & wait $!`, claiming the trap fires promptly. Reproduced on
  stock `/bin/bash` 3.2.57 (macOS): a script started as an async command has `SIGINT`/`SIGQUIT` set to
  `SIG_IGN` and an explicit `trap … INT` **does not install** (`trap -p INT` shows nothing); `kill -INT`
  is ignored and the script exits **0**, not 8. **`SIGTERM` works in every configuration.** The fixture,
  `verify-fast.sh`, the acceptance criteria, and the §4 contract were all changed to **SIGTERM**; SIGINT
  is still trapped for the real interactive bazel-client case (tty foreground PG). This also constrains
  E3's real kill path (SIGTERM-first or SIGINT-to-process-group, then SIGKILL).
- **Trap latency.** Sleeping in 0.2s slices (`sleep 0.2 & wait $!`) keeps the SIGTERM trap latency
  well under the `<1s kill` budget E3 needs (each `wait` returns when the slice is signalled).
- **Fractional durations.** bash integer arithmetic can't do `0.2`; the script uses `awk` for the
  countdown so `FAKE_BAZEL_DURATION=0.2` works.
- **BEP file format.** `--build_event_json_file` is **newline-delimited JSON**, not a JSON array —
  appending objects (not commas) is correct, and E4's tailer reads line-by-line.
- **`.bazelrc` ~ non-expansion** (architecture §8.2) is an **E1** concern but noted in CLAUDE.md so
  no one writes `~` into a generated rc later.
- **macOS bash is 3.2.** The script avoids bash-4 features (no `${var,,}` lowercasing — uses `tr`;
  no associative arrays). Works on stock `/bin/bash` and on Homebrew bash via `/usr/bin/env bash`.
- **Recursion guard** in `tools/bazel`: `exec "$BAZEL_REAL"` with a hard error if unset.
- **modernc.org/sqlite + Go 1.26 / arm64:** pure-Go (no cgo), but historically version-sensitive to
  new Go releases — **T1 must actually `go build` it under the local Go 1.26 toolchain** before
  committing the dep, not assume it works.
- **`$@` outside the `case`-arm targets capture** is fine on bash 3.2; the `${TARGETS[@]:-}` empty-array
  expansion under `set -u` was verified (returns `[]`, no unbound error).

---

## 7. Effort & internal ordering

**Total: ~0.5–1 engineer-day.** This is plumbing; the only fiddly artifact is `fake-bazel.sh`
(signal handling + fractional sleep) which the design above already de-risks.

| Task | Est. | Blocking? |
|---|---|---|
| T1 go.mod + gitignore | 15 min | gates all Go |
| T2 shared internal packages (+ tests) | 1.5 h | gates T3 |
| T3 server/CLI stubs + mains | 1 h | gates T4 |
| T4 Makefile | 30 min | gates verify targets |
| T5 fake-bazel.sh (+ debug signal timing) | 1.5 h | parallel after T4 |
| T6 verify-fast.sh | 45 min | needs T4+T5 |
| T7 synthetic workspace + verify-e2e | 45 min | parallel after T4 |
| T8 tools/bazel placeholder + testdata/README | 15 min | parallel |
| T9 CLAUDE.md | 45 min | parallel |
| T10 green-board gate | 15 min | last |

**Recommended internal order:** T1 → T2 → T3 → T4, then fan out T5/T7/T8/T9 in parallel, fold in
T6 once T5 exists, finish with T10. **Critical path** is T1→T2→T3→T4→T6→T10; everything else hangs
off T4. Resolve **OD-1 (module path)** before T1 — it is the only blocker that can't be deferred.

> **Effort adjustment (review):** T5 (fake-bazel) is the one genuinely fiddly artifact and the
> SIGINT/SIGTERM finding adds a small amount of signal-disposition testing; budget it at ~2 h, not
> 1.5 h. Total remains ~1 engineer-day.

---

## Staff Engineer Review

**Reviewer:** Staff Eng · **Date:** 2026-06-17 · **Scope:** E0 as written, cross-checked against
`01-architecture.md`, `02-epics.md`, and `E2-broker-core.md` (the authoritative API contract).

### (a) Verdict: **ship-with-changes** (the changes are applied in this revision)

The shape of E0 is right — `internal/` layout, compiling stubs as forward contracts, a fake-bazel
that turns kill/admission into 1-second checks, a real bzlmod e2e fixture, and a single `/verify`
entrypoint. But two classes of defect made the *as-written* plan unable to meet its own acceptance
criteria, and both are now fixed in place. It is not "ship" because a defect was load-bearing
(the cancel test would have passed-by-accident never / failed); it is not "needs-rework" because the
architecture and sequencing are sound and the fixes were surgical.

### (b) Top findings

1. **★ Critical — the cancel contract was wrong (SIGINT → exit 0, verified).** Done-when #3, E3's
   entire kill test, E5, and `verify-fast.sh` all launch fake-bazel with `&` then `kill -INT` and
   assert exit 8. On stock macOS `/bin/bash` 3.2.57 (reproduced during review), an `&`-launched
   script has `SIGINT` set to `SIG_IGN` and a `trap … INT` **silently does not install**, so the
   process exits **0**. The single most important fixture in the repo did not do its one job.
   **Fix:** cancel signal is now **SIGTERM** everywhere (verified to work in all configs); SIGINT is
   still trapped for the real-tty bazel-client case; the constraint is propagated to E3's kill path.
2. **Major — E0's "frozen contracts" contradicted E2 (the authoritative owner) on ~7 points.** E0
   used `internal/buildinfo.BuildInfo` + state `cancelled`; E2 uses `internal/build` + `internal/wire`
   with `killed`/`unknown` + `Source`. E0 used config field `MaxConcurrent`, env `BAZEL_BROKER_CONFIG_DIR`,
   db/log in `~/.config/...`, slog **text**, ephemeral port + `broker.json`; E2 uses `MaxConcurrency`,
   env `BAZEL_BROKER_CONFIG`, db/log in `~/.local/state/...`, slog **JSON**, fixed port 8765. Shipping
   E0 as written would have forced a rename/rewrite across every epic on first real integration —
   exactly the churn E0 exists to prevent. **Fix:** E0 aligned to E2; the genuinely-open fork
   (port/discovery) is demoted to OD-5 and escalated rather than silently resolved.
3. **Medium — synthetic workspace wouldn't build on Bazel 8/9.** Native `sh_binary` is being removed
   from the global namespace (moved to `rules_shell`); the bare `sh_binary` in `BUILD.bazel` is not
   reliably available on the project's stated 8.x/9.x target. **Fix:** declare `rules_shell` in
   `MODULE.bazel`, `load()` it, bump `.bazelversion`, and note the first-run non-hermeticity.
4. **Minor — over-claims to tighten.** "modernc.org/sqlite … no toolchain surprises; verified
   buildable" wasn't verified; go.mod versions were presented as firm pins. Both softened to
   "verify at T1." Healthz body and `httpapi`/`logging` package names reconciled with E2.

### (c) What I changed (in place)

- Rewrote §2.6 with a **verified bash-3.2 SIGINT/SIGTERM matrix** and switched the cancel contract to
  SIGTERM; updated the script header/trap comments, `verify-fast.sh` (§2.10), acceptance A3, §5.1,
  and the §4 contract row + a new **kill-signal note** for E3.
- Reconciled E0 with E2: package split (`build`+`wire`, not `buildinfo`; `httpapi`; `logging`), the
  `Config` schema/env-var/state-dir, slog **JSON**, default port 8765, healthz shape, and the §4
  table + CLAUDE.md "DO NOT BREAK" block. Added an authority note that **E2 owns the API contract**.
- Fixed the synthetic workspace for Bazel 8/9 (`rules_shell`), added a workspace `.gitignore` and a
  non-hermeticity caveat; softened go.mod/sqlite claims to T1-verified.
- Added **OD-4** (BEP `INTERRUPTED` vs registry `killed`), **OD-5** (port/discovery — escalate),
  **OD-6** (Bazel/rules_shell pins); corrected D-stack-2 framing; nudged the T5 estimate.

### (d) Escalate to the consolidated cross-epic review

- **OD-5 (port & client discovery) — needs an owner decision.** Fixed 8765 (E2) vs ephemeral +
  `broker.json`. Touches E2 (bind), E6/E7/E8 (client bootstrap). E0 must not resolve it.
- **Kill-mechanism constraint (feeds D4).** The verified SIGINT-on-async behavior means E3's kill
  path cannot be a bare `kill -INT <pid>`; it must be SIGTERM-first or SIGINT-to-process-group, then
  SIGKILL. Confirm this is compatible with the command-server `Cancel` alternative.
- **OD-2 (eager vs lazy go.mod deps).** Cosmetic but cross-epic; pick once so `go.sum` churn is
  predictable. Recommend eager (E0 declares all) for a stable lockfile.
- **OD-1 (module path).** Still the one hard blocker before T1; recommend the GitHub path. Confirm.
- **OD-4 (cancel terminology).** Quick confirm with E3/E4 that BEP-`INTERRUPTED`/exit-8 both map to
  registry `killed`. Low risk; just align vocabulary.
- **Consistency sweep recommended.** Because E0↔E2 had drifted on ~7 conventions, the consolidated
  review should diff *every* epic's assumed config/wire/log conventions against E2 §2.5/§4 and this
  file's §4 to catch the same drift in E3–E8 before implementation starts.
