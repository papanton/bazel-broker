# Bazel Broker — Epics

Decomposition of `01-architecture.md` into independently-shippable epics.
Distribution is out of scope — the tool ships as a **Homebrew cask** (Developer ID +
notarization for others; ad-hoc signing for personal use). No App Store, no sandbox.

Status: **Draft v1** · Last updated: 2026-06-17

---

## Ordering & value map

```
        ┌────────────────────────────┐
        │ E0 Scaffolding & Verify     │ (foundation — enables /verify)
        └──────────┬─────────────────┘
                   │
      ┌────────────┼───────────────────────────────┐
      ▼            ▼                                ▼
┌───────────┐ ┌──────────────────┐         (independent, config-only)
│ E2 Broker │ │ E1 Cache &        │◀── delivers cross-worktree cache
│ Core      │ │    Profiling cfg  │     win with ZERO code
└─────┬─────┘ └──────────────────┘
      │
  ┌───┼───────────────┬───────────────┐
  ▼   ▼               ▼               ▼
┌──────────┐  ┌──────────────┐  ┌──────────────┐
│ E3 Disco │  │ E4 BEP/      │  │ E6 brokerctl │
│  & Kill  │  │  Metrics     │  │  CLI         │
└────┬─────┘  └──────────────┘  └──────────────┘
     │                                  │
     ▼                          ┌───────┴───────┐
┌──────────────┐                ▼               ▼
│ E5 Admission │         ┌──────────────┐ ┌──────────────┐
│  + Wrapper   │         │ E7 Web       │ │ E8 Menu-bar  │
└──────────────┘         │  Dashboard   │ │  App         │
                         └──────────────┘ └──────────────┘
```

**Early standalone value:** E1 (cache) + E2 + E3 (kill) + E6 (CLI) already give you
shared cache, live trace, and targeted kill — **with no wrapper**. E5 adds the only piece
that needs interception (block-before-build admission).

---

## EPIC 0 — Project scaffolding & verify harness
**Goal:** a repo where every later epic can be built and `/verify`-ed headlessly in seconds.
**Depends on:** —
**Maps to:** M1 (pre-work)

**Scope**
- Repo layout: `cmd/broker`, `cmd/brokerctl`, `tools/bazel`, `testdata/`, `plans/`.
- `go.mod` (single module for broker + CLI), `Makefile`, `.gitignore`.
- `testdata/fake-bazel.sh` — `BAZEL_REAL` stub: emits a couple of BEP json events, honors
  `--build_event_json_file`, traps SIGINT (assert graceful cancel), sleeps a configurable duration.
- `testdata/workspace/` — trivial synthetic Bazel workspace + a tiny target for real e2e.
- `CLAUDE.md` — per-component "how to run & verify" recipes + canonical Make targets.

**Deliverables:** buildable empty repo, fake-bazel, Makefile, CLAUDE.md.
**Done when:** `make build` succeeds; `FAKE_BAZEL_DURATION=2 testdata/fake-bazel.sh --build_event_json_file=/tmp/x.json`
writes events and exits 0; SIGINT to it exits with the cancel code.

---

## EPIC 1 — Cache sharing & profiling config
**Goal:** cross-worktree cache hits + per-build profiling, zero application code. (§8)
**Depends on:** — (config in the iOS project; optional setup script in this repo)
**Maps to:** M0

**Scope**
- `.bazelrc` block: shared `--disk_cache` (absolute path), `--profile`, per-worktree relative
  `--build_event_json_file`.
- Relocatability flags: `-ffile-prefix-map`/`-fdebug-prefix-map` (copt/objccopt/swiftcopt),
  `--incompatible_strict_action_env`, `--experimental_output_paths=strip`.
- `setup.sh` to write the absolute-path lines (`.bazelrc` doesn't expand `~`).
- Doc: which flags do what + the "never share output base" rule.

**Deliverables:** `.bazelrc` fragment + `setup.sh` + cache doc.
**Done when:** building the same target in two worktrees → second build shows high cache-hit
ratio (BEP `BuildMetrics`); profile `.gz` opens in Perfetto with a critical path.

---

## EPIC 2 — Broker daemon core
**Goal:** the always-on control-plane process with an observable localhost API. (C2)
**Depends on:** E0
**Maps to:** M1

**Scope**
- Go daemon: registry of active/recent builds (in-memory + SQLite-backed).
- HTTP+WS API (loopback TCP + bearer token): `GET /healthz`, `ListBuilds` (`ls --json`),
  `StreamEvents` (WS), lifecycle stubs (`Register`/`Deregister`).
- SQLite store skeleton (`modernc.org/sqlite`); config file in `~/.config/bazel-broker/`.
- `launchd` plist (KeepAlive, RunAtLoad); structured logs to a known path.

**Deliverables:** `broker` binary, launchd plist, config, store schema v1.
**Done when:** `launchctl` starts it; `curl /healthz` → `{builds:0}`; manual `Register` then
`ls --json` shows the build; survives restart (launchd KeepAlive).

---

## EPIC 3 — Process discovery & kill
**Goal:** see and stop running builds with no wrapper required. (C2 passive path)
**Depends on:** E2
**Maps to:** M2

**Scope**
- macOS `libproc` (cgo): enumerate `bazel`/`bazelisk` client PIDs; resolve each PID's cwd → worktree.
- Reconcile discovered processes into the registry (merge with Register'd builds).
- `Kill(invocation_id|pid)`: SIGINT → grace → SIGKILL; optional command-server `Cancel`
  (`<output_base>/server/command_port`) as out-of-band path.

**Deliverables:** discovery module, kill module, registry reconciliation.
**Done when:** launch fake-bazel (long duration) in a worktree → appears in `ls` with correct
worktree → `kill <id>` makes it exit with the cancel code in <1s.

---

## EPIC 4 — BEP ingest, metrics & cache insight
**Goal:** live trace + cache%/timing/failures + Perfetto deep-links. (C3, C4, §8.4)
**Depends on:** E2, E1
**Maps to:** M3

**Scope**
- Tail each build's per-worktree `--build_event_json_file` (`nxadm/tail`, handle truncation).
- Parse BEP via generated protos + `protojson` (`build_event_stream.proto`).
- Extract `BuildMetrics` → SQLite: action counts, **cache hit/miss**, durations, pass/fail.
- API: `Metrics(invocation_id)`; **low-hit alerts**; disk-cache size report + GC (size/age).
- Perfetto: locate the build's `--profile`, serve it over localhost, expose a deep-link URL.

**Deliverables:** ingest pipeline, metrics schema, alerts, GC, Perfetto serving.
**Done when:** a real build streams progress into `ls`; afterwards `Metrics` returns the
cache-hit ratio matching Bazel's own summary; Perfetto link auto-loads the profile.

---

## EPIC 5 — Admission control & `tools/bazel` wrapper
**Goal:** block/queue builds before they oversubscribe the machine. (C1, §8.4)
**Depends on:** E2, E3
**Maps to:** M4

**Scope**
- `tools/bazel` wrapper (bash): mint `--invocation_id`, call `RequestAdmission` (block until
  ALLOW/QUEUE), inject E1 flags, `exec "$BAZEL_REAL"`; `BROKER_BYPASS=1` escape hatch.
- Broker admission: global semaphore (max N) → CPU/RAM token bucket; `Pause/Resume/Drain`.
- **Anti–thundering-herd stagger:** detect duplicate concurrent actions, let the first
  populate disk_cache so the second hits.
- **Keep servers warm:** tune `--max_idle_secs` / avoid needless `shutdown`.
- Guards: skip admission in CI / optional bypass for Xcode-triggered builds.

**Deliverables:** wrapper, admission policy engine, stagger, server-warmth tuning.
**Done when:** start 3 fake builds with max-concurrency=2 → third shows `queued` then admits
when one finishes; `BROKER_BYPASS=1` skips the gate; `drain` stops new admissions.

---

## EPIC 6 — `brokerctl` CLI
**Goal:** terminal control/visibility (first front-end). (C5a)
**Depends on:** E2 (richer with E3/E4)
**Maps to:** M5

**Scope**
- Go CLI (same module): `ls [--json]`, `kill <id>`, `drain`, `watch`, `profile <id>` (open Perfetto).
- Talks to broker over HTTP; table + `--json` output for `/verify` assertions.

**Deliverables:** `brokerctl` binary + Make target + CLAUDE.md recipe.
**Done when:** `brokerctl ls` mirrors `/healthz`; `kill` works end-to-end; `--json` is parseable.

---

## EPIC 7 — Local web dashboard
**Goal:** glanceable browser view served by the broker. (C5b)
**Depends on:** E2 (best with E4)
**Maps to:** M5

**Scope**
- Broker serves a static page via `embed.FS`: live build list over WebSocket, kill buttons.
- Cache% / duration charts (uPlot or CSS bars); Perfetto deep-links. No build step.

**Deliverables:** embedded web UI at `http://127.0.0.1:PORT/`.
**Done when:** opening the page shows live builds updating over WS; kill button works; cache%
renders from E4 metrics.

---

## EPIC 8 — SwiftUI menu-bar app
**Goal:** the native "small Mac app" — glance + kill from the menu bar. (C5c)
**Depends on:** E2 (best with E3/E4)
**Maps to:** M5

**Scope**
- SwiftUI `MenuBarExtra` (macOS 13+); `URLSession` + `URLSessionWebSocketTask` to the broker.
- Menu list: "N building, M queued", per-build worktree + elapsed + kill; cache%; open-in-Perfetto.
- Logic-free view over the daemon; ad-hoc signed local `.app`; brew-cask later.

**Deliverables:** `.app` + Xcode/SwiftPM project.
**Done when:** menu bar reflects live builds (verified via `xcodebuildmcp-cli` build/launch/
screenshot); kill button stops a build; cache% matches `brokerctl`.

---

## Cross-cutting decisions (carried from architecture §12)
- **D-stack-1:** cgo+libproc (recommended) vs pure-Go shell-out for process discovery (E3).
- **D-stack-2:** loopback TCP + token (recommended) vs Unix socket for the API (E2).
- **D3:** admission model — block (E5) vs kill-based throttling (E3-only) vs hybrid.
- **D4:** kill via owned-PID SIGINT (simple) vs command-server `Cancel` (E3).
