# Bazel Broker — Architecture Plan

> A local, self-contained control + observability layer for Bazel builds running across
> multiple git worktrees, triggered by multiple agents (Claude instances) and humans, on macOS.

Status: **Draft v3** · Owner: Antonis · Last updated: 2026-06-17

---

## 1. Problem statement

An iOS app is built with **Bazel + Bazelisk** across **multiple git worktrees**.
Multiple **Claude instances** (and the developer) trigger builds concurrently. Today
this is uncontrolled and opaque:

- **No admission control** — N worktrees building at once oversubscribe CPU/RAM
  (each build spawns dozens of `clang`/`swiftc` processes), thrashing the machine
  and making *total* throughput worse.
- **No visibility** — there's no single view of "what is building right now, in which
  worktree, for how long."
- **No control** — no easy way to kill/queue a specific runaway build.
- **Fragmented cache** — each worktree gets its own Bazel server + output base, so
  identical work is redone across worktrees instead of being shared.

We want to **trace** builds, **kill/allow** them, **visualize cache + timing**, and
**triage** build-time / runtime issues — ideally surfaced in a small native macOS app —
**without depending on any third-party SaaS or remote-cache service.**

---

## 2. Goals & non-goals

### Goals
- G1. **Trace**: live list of in-flight builds (worktree, targets, invocation id, elapsed, state).
- G2. **Control**: kill a build; allow / queue / deny builds before they consume resources.
- G3. **Cache optimization**: share caches across worktrees; visualize hit ratios + misses.
- G4. **Triage**: per-build timing / critical-path analysis; surface slow/failed builds.
- G5. **Effective `/verify` loop**: every component launchable headlessly, observable, fast.
- G6. **Self-contained**: runs entirely on the local Mac; no external service required.

### Non-goals
- N1. Building a remote-cache / remote-execution backend. We share a **local** disk cache only.
- N2. OS-level CPU/process monitoring as a product (Activity Monitor / Stats already do this).
- N3. iOS/Xcode-specific build insight beyond generic Bazel BEP data.
- N4. Multi-machine / team orchestration. This is **single-Mac, single-developer** scope.
- N5. Any third-party build-results SaaS (e.g. hosted dashboards). Observability is in-house.

---

## 3. Design drivers (Bazel facts that shape everything)

These are the non-obvious constraints that the architecture is built around:

1. **Server keyed by output base.** Each `bazel` invocation talks to a long-lived JVM
   server identified by the **output base** = `output_user_root / md5(workspace_path)`.
   Different worktree path → **different output base → its own server, lock, and local cache.**

2. **Exclusive lock per output base.** Only one command runs per server at a time; others
   block (or fail fast with `--block_for_lock=false`). Cross-worktree builds are independent
   and genuinely parallel — which is exactly why they oversubscribe the machine.

3. **Cache is fragmented by default.** Separate output bases ⇒ separate analysis + local
   action caches. **Fix: share a `--disk_cache` directory, but NEVER share the output base**
   (that would serialize all builds on one lock). Cross-worktree hits also require *relocatable*
   actions so differing worktree paths don't bust the action key — see §8.

4. **Bazel auto-generates a unique `invocation_id`** per build and reports it via BEP
   (`BuildStarted.uuid`). We don't strictly need to mint our own.

5. **`tools/bazel` wrapper hook.** If `<workspace>/tools/bazel` exists & is executable, the
   launcher re-execs it with `BAZEL_REAL` set. Bazelisk respects this. Because it's committed
   to the repo, it **auto-propagates to every worktree** — a clean, zero-setup interception point.
   (Must exec `$BAZEL_REAL`, never `bazel`, or it recurses infinitely.)

6. **Kill primitives.** SIGINT to the bazel *client* process triggers graceful cancellation;
   or call `Cancel` on the server's command-server gRPC (`<output_base>/server/command_port`).

7. **Built-in profiling.** Bazel writes a Chrome-format JSON trace profile per build
   (`--profile=<path>`, default `command.profile.gz` in the output base) with a per-action
   **critical path** — openable directly in **Perfetto** (`ui.perfetto.dev`) with no server.

---

## 4. Architecture overview — control plane + self-hosted observability

We build a single self-contained daemon that does both control (admission + kill) and
lightweight observability (BEP ingest + local metrics). Deep per-build timing reuses
Bazel's own profile via **Perfetto**. No third-party service.

```
  Claude #1 ┐                 ┌─ tools/bazel wrapper ─┐   (OPTIONAL — only for true pre-admission)
  Claude #2 ┼─ invoke bazel ─▶│  intercept + register  │
  Claude #3 ┘                 └───────────┬───────────┘
        (or: builds run unwrapped; broker discovers them passively)
                                          │ register / request admission
                                          ▼
                              ┌────────────────────────────────┐
                              │        BUILD BROKER (daemon)     │  headless · launchd-managed
                              │  CONTROL: registry · admission   │  localhost API (HTTP+WS)
                              │          · kill / drain          │
                              │  OBSERVE: tail BEP json →         │
                              │          metrics store (SQLite)  │
                              │          cache% · timing · fails │
                              └───┬───────────────┬──────────────┘
                  CLI / web / app │               │ injected flags on every build:
                                  ▼               ▼   --build_event_json_file=<per-invocation>  → broker tails
                       ┌──────────────┐      --profile=<per-invocation>            → open in Perfetto
                       │ Front-ends   │      --disk_cache=/shared --repository_cache=/shared
                       │  • brokerctl │
                       │  • localhost │      deep per-build critical path:
                       │    web page  │      ┌─────────────────────────┐
                       │  • menubar   │◀─────│  Perfetto (ui.perfetto)  │  local, no server
                       │    app       │ open │  loads Bazel .profile     │
                       └──────────────┘      └─────────────────────────┘
```

**Split rationale:** the broker must **outlive any UI**. Builds are triggered by Claude
instances regardless of whether a window is open; if admission state lived in a GUI app,
quitting it would orphan the gate. So the broker is a background daemon; UIs are thin clients.

---

## 5. Components

### C1. `tools/bazel` wrapper — *optional, ~30 lines*
- Committed to the repo → present in every worktree automatically.
- On invoke: optionally request admission (block until ALLOW/QUEUE), inject flags
  (`--build_event_json_file`, `--profile`, `--disk_cache`, `--repository_cache`),
  then `exec "$BAZEL_REAL" "$@"`.
- **Only needed for true pre-admission (block-before-build).** Tracing, viz, cache sharing,
  and kill all work *without* it (see §7). Add it only when passive throttling isn't enough.

### C2. Build Broker daemon — *the core*
- **Headless**, `launchd`-managed (KeepAlive, start at login, restart on crash).
- **Control:** registry of active/recent builds + admission policy
  (max concurrent, CPU/RAM token bucket, per-worktree / per-priority limits, drain mode).
- **Observability:** tails each build's `--build_event_json_file`, parses BEP, extracts
  `BuildMetrics` (action counts, **cache hit/miss**, timing) → stores in **SQLite** →
  serves cache% / slow-build / failure summaries to the front-ends.
- Populated two ways:
  - **Active**: wrapper calls `RequestAdmission` / `Deregister`.
  - **Passive**: discover running `bazel` client processes via macOS `libproc`
    (`proc_pidinfo`), map each to its worktree by cwd. Works with no wrapper.
- Exposes a localhost API (see §6).

### C3. Live-trace ingest — *built into the broker, no external collector*
- Builds set `--build_event_json_file=<per-invocation path>`; broker tails + parses BEP
  (`Progress`, `Configured`, `ActionExecuted`, `TestResult`, `BuildMetrics`, `BuildFinished`).
- ⚠️ Per-invocation file paths only — a static path in `.bazelrc` would clobber across
  concurrent builds. Generated by the wrapper, or derived from the discovered invocation id.

### C4. Deep timing = Perfetto (reused, local, no server)
- Each build sets `--profile=<per-invocation>.gz`. For critical-path / per-action drill-down,
  open the profile in `ui.perfetto.dev` (or `chrome://tracing`). The broker links to / opens it.
- The broker's own SQLite metrics cover the aggregate view (cache%, durations, pass/fail);
  Perfetto is the on-demand deep dive.

### C5. Front-ends (thin clients over the broker API)
- **C5a. `brokerctl` CLI** — `ls --json`, `kill <id>`, `drain`, `watch`. First to build.
- **C5b. localhost web page** — broker serves HTML+WS on `:port`. Cheap, cross-everything, iterates fast.
- **C5c. SwiftUI `MenuBarExtra` app** — last/optional. Glanceable "3 building, 1 queued" + kill
  buttons + cache% + "open profile in Perfetto". Logic-free view over the daemon.

---

## 6. APIs & protocols

### Broker API (we define) — localhost only
| Method | Purpose |
|---|---|
| `RequestAdmission(worktree, targets, pid)` | → `ALLOW \| QUEUE \| DENY` (blocking; wrapper only) |
| `Heartbeat / Deregister(invocation_id)` | lifecycle |
| `ListBuilds() → [BuildInfo]` | registry snapshot (CLI/UI) |
| `StreamEvents()` | live feed (WS) for UIs |
| `Metrics(invocation_id)` | cache hit/miss, durations, pass/fail from the SQLite store |
| `Kill(invocation_id)` | SIGINT→SIGKILL the client, or command-server `Cancel` |
| `Pause / Resume / Drain` | admission policy controls |
| `GET /healthz` | `{builds:N, queued:M}` — verify/health probe |

Transport: **HTTP + WebSocket** (trivial from Swift & curl) — gRPC optional later.

### Bazel-side APIs (we consume / configure)
| Need | Mechanism |
|---|---|
| Live build feed | BEP `build_event_stream.proto` via `--build_event_json_file` (tail) |
| Cache & timing metrics | BEP `BuildMetrics` event (action counts, cache stats, timing) |
| Deep critical-path | JSON trace profile (`--profile`) → Perfetto |
| Out-of-band cancel | command-server gRPC `Cancel`; `<output_base>/server/command_port` |
| Correlation | `--invocation_id` (or read Bazel's auto-generated id) |
| Interception | `tools/bazel` + `BAZEL_REAL` |
| Server introspection | `bazel info server_pid output_base` |
| Process discovery | macOS `libproc` / `proc_pidinfo` (PID → cwd → worktree) |
| Cache sharing | `--disk_cache`, `--repository_cache` (shared local dirs) |

---

## 7. Capability → requirement matrix

| Capability | Needs wrapper (C1)? | How |
|---|---|---|
| Trace what's building | ❌ | passive process discovery + BEP tail |
| Visualize cache / timing | ❌ | broker BEP metrics (SQLite) + `--profile` → Perfetto |
| Optimize cache across worktrees | ❌ | shared `--disk_cache` + relocatability flags (§8); broker GC/stagger |
| Kill a build | ❌ | discover client PID → SIGINT; or command-server `Cancel` |
| **Allow / queue before start** | ✅ | only a pre-exec hook can block; else kill-based throttling |

---

## 8. Cache strategy

Cross-worktree cache sharing is the **single highest-leverage win** in this project. There
are three cache layers, each needing different treatment.

### 8.1 What can and can't be shared
| Cache layer | Scope | Shareable across worktrees? |
|---|---|---|
| Analysis cache (Skyframe, in-memory) | per Bazel server / output base | ❌ no — instead keep servers warm (§8.4) |
| Local action cache | per output base | ❌ not directly — superseded by the disk cache |
| **Disk cache** (`--disk_cache`) | a directory | ✅ **yes — the main lever** |
| Repository cache (`--repository_cache`) | per-user | ✅ already shared by default (explicit setting is belt-and-suspenders) |

### 8.2 The lever: one shared disk cache
- Point every worktree's `--disk_cache` at a single directory (absolute path — `.bazelrc`
  does **not** expand `~`/`$HOME`, so it's written by the setup script or the wrapper).
- It's a shared CAS + action cache: worktree A's executed action result becomes a **hit**
  for worktree B whenever the action hash matches.
- **Never share the output base** — that just serializes all builds on one lock.

### 8.3 Relocatability — making the shared cache *actually* hit
The subtle failure mode: worktree paths differ (`/worktrees/feature-a` vs `…/feature-b`),
and those absolute paths leak into action command lines / debug info → **different action
hash → cache miss**, even for byte-identical source. Normalize so actions are relocatable:
- `--copt=-ffile-prefix-map=…` / `-fdebug-prefix-map` (plus `--objccopt` / `--swiftcopt`) to
  strip absolute paths from ObjC/Swift/C++ outputs and debug info.
- `--incompatible_strict_action_env` so the action environment doesn't vary with each shell's `$PATH`/env.
- `--experimental_output_paths=strip` to drop config-specific path segments.

Without this, cross-worktree hit rates are mediocre (paths bust the key); with it, you
approach single-tree hit rates. **This is the improvement most setups miss.**

### 8.4 Broker cache features (beyond config)
1. **Hit-ratio visualization + low-hit alerts** — from the BEP `BuildMetrics` we already
   ingest; a drop is the early signal that path/env leakage (§8.3) is busting the cache.
2. **Admission stagger (anti–thundering-herd)** — if two worktrees start the *same* action
   simultaneously, both miss the disk cache (neither has written yet) and both compute it.
   Admission can detect the overlap and let the first populate the cache so the second hits.
   This cache win is only possible from the control plane.
3. **Disk-cache GC / size cap + reporting** — the shared cache grows unbounded; the broker
   prunes by size/age (driving Bazel's built-in disk-cache GC where available, or directly).
4. **Keep servers warm** — the one cache we can't share is the in-memory analysis cache
   (per server). Tune `--max_idle_secs` / avoid needless `shutdown` so each worktree's
   analysis cache stays hot between builds.

### 8.5 Build less (optional, higher-level)
- **`bazel-diff`** — compute the impacted target set from a git diff and build only those.
  Complementary to caching (caching makes redundant work cheap; bazel-diff avoids requesting
  it at all). Good fit since worktrees are per-branch.

---

## 9. Tech stack

- **Daemon + CLI:** Go — sub-second compile (fast `/verify` loop), single static binary,
  launchd-friendly, good BEP proto + SQLite support.
- **Wrapper:** bash (or a tiny Go binary) — must be dependency-free.
- **Metrics store:** SQLite (embedded, zero-ops).
- **Deep timing:** Perfetto (browser, local; loads Bazel's profile).
- **Menu-bar app:** SwiftUI `MenuBarExtra` — native, cheap for an iOS dev, thin view layer.
- **Process introspection:** macOS `libproc` (cgo or a small helper).

---

## 10. Verifiability strategy (`/verify` on every step)

The "verify contract": every component is **launchable headlessly + observable + fast**.

- **Fake bazel** (`testdata/fake-bazel.sh`): a `BAZEL_REAL` stub that emits a couple of BEP
  events, honors `--build_event_json_file`, traps SIGINT (to assert graceful cancel), and
  sleeps a configurable duration. Turns kill/admission verification into ~1-second checks
  instead of 5-minute iOS compiles.
- **Observable daemon:** `GET /healthz`, `brokerctl ls --json`, structured logs to a known path.
- **Synthetic workspace + 2–3 throwaway worktrees** of a trivial target for occasional
  real end-to-end verification (real bazel, real BEP/profile ingest).
- **Discoverable run recipes:** `Makefile` targets + a `CLAUDE.md` "How to run & verify"
  section per component, so `/verify` and `/run` don't guess.
- **GUI exception:** verify the menu-bar app via the `xcodebuildmcp-cli` skill
  (build/launch/screenshot/UI-automate); keep its logic in the daemon so there's little to verify.

---

## 11. Build order / milestones

1. **M0 — Cache sharing + profiling (mostly config).** `.bazelrc`: shared `--disk_cache` +
   relocatability flags (`-ffile-prefix-map`, `--incompatible_strict_action_env`,
   `--experimental_output_paths=strip`), `--profile`, per-worktree `--build_event_json_file`.
   Biggest single win (§8).
2. **M1 — Observable broker skeleton.** Go daemon: registry + `/healthz` + `ls --json`,
   launchd plist + `fake-bazel.sh`. First `/verify` target.
3. **M2 — Passive discovery + kill.** Enumerate bazel client PIDs → worktree; `brokerctl kill`.
   (Delivers trace + kill with **no wrapper**.)
4. **M3 — BEP ingest + metrics + cache insight.** Tail per-invocation BEP json → SQLite;
   cache% / durations / failures via the API + low-hit alerts (§8.4); disk-cache size report
   + GC. Perfetto link for deep dives.
5. **M4 — Admission control.** Global semaphore (max N concurrent) → CPU/RAM token bucket;
   stagger duplicate concurrent actions (anti–thundering-herd, §8.4); keep servers warm.
   Introduce the `tools/bazel` wrapper for block-before-build.
6. **M5 — Front-ends.** localhost web page, then the SwiftUI menu-bar app.

M0–M3 already deliver tracing, cache optimization, kill, and metrics — all self-contained.
M4 adds the only thing that truly needs interception.

---

## 12. Open questions / decisions to lock

- **D1. Observability depth:** is the broker's own SQLite metrics + Perfetto enough, or do we
  later want a richer prebuilt web UI (e.g. open-source **Buildbarn bb-portal**, which ingests
  BEP locally)? Default: roll our own, keep it self-contained.
- **D2. Broker transport:** HTTP+WS (recommended) vs gRPC.
- **D3. Admission model:** block-before-build (wrapper) vs kill-based throttling (passive only)
  vs hybrid. Driven by how painful CPU oversubscription actually is.
- **D4. Kill mechanism:** own-the-PID SIGINT (simple) vs command-server `Cancel` (out-of-band).
- **D5. Wrapper language:** bash vs tiny Go binary.

---

## 13. References (from prior research, all verified)

- Bazel JSON trace profile — https://bazel.build/advanced/performance/json-trace-profile
- Bazel build performance metrics / BEP — https://bazel.build/advanced/performance/build-performance-metrics
- Bazel BEP — https://bazel.build/remote/bep
- Perfetto — https://perfetto.dev/
- Buildbarn bb-portal (optional self-hosted BEP UI) — https://github.com/buildbarn/bb-portal
