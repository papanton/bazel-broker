# Bazel Broker — Agent Guide

Single-Mac, self-contained control + observability for Bazel across git worktrees.
Architecture: `plans/01-architecture.md` · Epics: `plans/02-epics.md` ·
Binding cross-epic reconciliation (read first): `plans/epics/00-consolidated-review.md`.

## Conventions (DO NOT BREAK — other epics depend on these; E2 is authoritative for the API)
- **Module path: `github.com/antoniospapantoniou/bazel-broker`** — appears in every import.
- Go 1.26, macOS arm64. cgo only inside `internal/discovery` (E3); the default build stays pure-Go.
- Logging: slog **JSON** handler via `internal/logging`. Always include the key `invocation_id`
  on build-scoped log lines. Level via `$BAZEL_BROKER_LOG_LEVEL` (default info).
- Config file: `~/.config/bazel-broker/config.json` (override the *file* via `$BAZEL_BROKER_CONFIG`;
  `$XDG_CONFIG_HOME` is honored for the dir).
  State dir (db, log): `~/.local/state/bazel-broker/` (`broker.db`, `broker.log`).
- API transport: loopback TCP `127.0.0.1:8765` (default) + `Authorization: Bearer <token>`.
  [OD-5: ephemeral port + `broker.json` discovery is an open alternative — not yet adopted.]
- **Shared types: `internal/build.Build` (domain) + `internal/api.Build` (JSON DTO).** `internal/api`
  is the importable wire contract (NOT `internal/wire` — retired by the consolidated review C1).
  States `{queued,running,finished,failed,killed,unknown}` (state is **`running`**, not `building`);
  DTO fields include `invocation_id`, `start_time`, `cache_hit_ratio` (E2 §4.1 freezes tags).
- fake-bazel cancel code: **8**. CANCEL SIGNAL = **SIGTERM** (SIGINT is also trapped but only fires
  with a tty/process-group; an `&`-launched process has SIGINT=SIG_IGN on bash 3.2 — verified).
  Kill tests accept exit **8 OR 137** (137 = SIGKILL reap in the real E3 path).

## Build & run
- `make build`       — builds `bin/broker` + `bin/brokerctl` (with version ldflags)
- `make run-broker`  — runs the daemon in the foreground (Ctrl-C to stop)
- `bin/broker -version` · `bin/brokerctl version` · `bin/brokerctl ls`

## Verify
- `make verify-fast`  — ~3s headless: build + `go test ./...` + fake-bazel + cancel-code + daemon smoke.
  Prints `VERIFY-FAST: PASS`. This is what the `/verify` skill runs.
- `make verify-e2e`   — real `bazel build //:gen //:hello` in `testdata/workspace/` (SKIPs cleanly
  if no bazel/bazelisk on PATH). Writes a real BEP json + `.profile.gz`.
- `make fmt vet test` — gofmt / go vet / go test.

## fake-bazel (`testdata/fake-bazel.sh`)
Stub `BAZEL_REAL` for fast verify. Honors `--build_event_json_file`, `--invocation_id`.
Knobs: `FAKE_BAZEL_DURATION` (s, fractional), `FAKE_BAZEL_CACHE_HITS`/`MISSES`, `FAKE_BAZEL_EXIT`.
Cancel: send **SIGTERM** -> exit **8** (not SIGINT for `&`-launched; see Conventions). Example:
```sh
FAKE_BAZEL_DURATION=2 testdata/fake-bazel.sh build --build_event_json_file=/tmp/x.json //:gen
```
As a wrapper target (proves the BAZEL_REAL re-exec contract for E5):
```sh
BAZEL_REAL=testdata/fake-bazel.sh tools/bazel build //:gen --build_event_json_file=/tmp/w.json
```

## Layout
```
cmd/broker, cmd/brokerctl         entrypoints (E0 stubs)
internal/version                  ldflags-injected build metadata
internal/build                    domain Build + State/Source enums
internal/api                      importable JSON DTOs (api.Build) — the wire contract
internal/config                   config path/defaults (E2 fills IO)
internal/logging                  slog JSON convention
internal/httpapi                  HTTP server (E0: /healthz only)
internal/registry, store, apiclient   named compiling placeholders (E2/E6 fill)
internal/discovery, bep           doc.go placeholders (E3/E4; cgo boundary in discovery)
tools/bazel                       BAZEL_REAL passthrough placeholder (E5 implements)
testdata/fake-bazel.sh            fast verify stub
testdata/workspace               synthetic real Bazel workspace (e2e)
testdata/ios-app                  real rules_apple iOS fixture (end-to-end target; do not modify)
scripts/verify-fast.sh            the fast verify orchestrator
```

## Per-component recipes (filled in as epics land)
- **broker daemon (E2):** `make run-broker` (foreground); `make smoke` (automated
  register->ls->deregister flow on an isolated port/config — asserts states);
  `make install` / `make uninstall` (launchd LaunchAgent via `deploy/install.sh`).
  Manual:
  `TOKEN=$(jq -r .token ~/.config/bazel-broker/config.json)`;
  `curl 127.0.0.1:8765/healthz`  (`/healthz` is auth-exempt) ->
  `{"status":"ok","builds":0,"queued":0,"total":0,...}`;
  `curl -H "Authorization: Bearer $TOKEN" 127.0.0.1:8765/builds` (== `ls --json`);
  register: `curl -H "Authorization: Bearer $TOKEN" -X POST 127.0.0.1:8765/register
  -d '{"invocation_id":"x","worktree":"/wt","targets":["//app:App"],"pid":1}'`;
  live events: `websocat -H "Authorization: Bearer $TOKEN" ws://127.0.0.1:8765/events`
  (first frame `snapshot`, then `build` upsert events).
  Wire contract is `internal/api` (FROZEN, §4); golden fixtures in `testdata/api/*.json`
  (regen with `go test ./internal/api -update`). Reserved routes (E3 kill / E4 metrics+profile /
  E5 admission) return **501** at the exact path the consumer calls; inject a real handler via
  `httpapi.WithKiller/WithMetrics/WithAdmitter` (no main.go edits).
- **brokerctl (E6):** terminal front-end. Build: `make build`. Unit tests: `make verify-brokerctl`
  (scoped, no broker). Subcommands: `ls [--json]`, `kill <id>`, `drain` (+ `drain pause`/`drain resume`),
  `watch [--once] [--json]`, `profile <id> [--print]`. Root flags: `--json --config --port --token --timeout`.
  Reads `port`/`token`/`host` from `config.json` (same resolution as E2: `--config` > `$BAZEL_BROKER_CONFIG`
  > `$XDG_CONFIG_HOME` > `~/.config`); `--port`/`--token` override. Calls FROZEN routes:
  `GET /builds`, `GET /healthz`, `WS /events`, `POST /builds/{id}/kill` (E3),
  `POST /admission/{drain,pause,resume}` (E5), `GET /builds/{id}/profile` (E4). Reserved routes return
  **501** → CLI degrades to exit **6** (`not available yet`), NOT a crash. Exit codes:
  0 ok · 1 usage · 2 config · 3 unreachable · 4 auth(401) · 5 broker(4xx/5xx incl 404) · 6 not-impl(501) · 7 open.
  `watch` reconnects with backoff across broker restarts (lossless re-snapshot); Ctrl-C exits 0;
  `--json` emits NDJSON (one event/line). Verify recipe:
  ```sh
  make build && ./bin/broker &                       # daemon writes config.json + token
  TOKEN=$(jq -r .token ~/.config/bazel-broker/config.json); PORT=$(jq -r .port ~/.config/bazel-broker/config.json)
  curl -sX POST 127.0.0.1:$PORT/register -H "Authorization: Bearer $TOKEN" \
    -d '{"invocation_id":"v1","worktree":"/wt/a","targets":["//app:App"]}'
  bin/brokerctl ls --json | jq -e '.builds|length'   # parseable; mirrors /healthz .total
  bin/brokerctl kill v1; echo $?                      # 6 until E3 lands, then 0
  bin/brokerctl watch --json --once                  # snapshot + live build events as NDJSON
  ```
- **discovery/kill (E3):** launch fake-bazel long; `brokerctl ls`; `brokerctl kill <id>`.
  Broker's kill path must use SIGTERM / process-group SIGINT, not a bare `kill -INT <pid>`.
- **BEP/metrics (E4):** point ingest at a BEP json; `brokerctl metrics <id>`.
- **admission (E5):** `make admission-verify` (race; engine + `tools/bazel` wrapper, no daemon).
  Headline DoD: start 3 fake builds via `tools/bazel` with `MaxConcurrent=2` → the 3rd QUEUEs,
  then admits when one finishes (slot freed **server-side**, no wrapper trap). `POST /admission`
  returns a **status code + one-word body** (`200 ALLOW`/`202 QUEUE`/`403 DENY`), never JSON (C5);
  the wrapper **fails open** on any non-200/202/403 or unreachable broker. `BROKER_BYPASS=1`/`CI`/
  non-build verbs skip the gate. Engine: `internal/admission` (FIFO + Semaphore→TokenBucket→Stagger,
  cap-1 buffered verdict, PID reaper). **main.go wiring** (the orchestrator applies this — E5 does
  NOT edit main.go/httpapi):
  ```go
  // after reg/hub/disco are built, before httpapi.New:
  adAdapter := admission.NewRegistryAdapter(reg, hub)
  engine := admission.NewEngine(admission.DefaultPolicy(), adAdapter) // policy from cfg if desired
  admitter := admission.NewAdmitter(engine)
  // ...then add the option to httpapi.New(...):
  //   httpapi.WithAdmitter(admitter),
  go engine.Run(ctx, admission.NewLoadProbe(), adAdapter) // load ticker + stagger/queued GC + PID reaper + server-side release
  ```
- **menubar (E8):** the main UX (`apps/MenuBar/`); see its CLAUDE.md. (The E7 web
  dashboard was removed — the menu-bar app is the sole GUI; `brokerctl` is the CLI.)

## Pitfalls baked in
- macOS bash is **3.2** — fake-bazel avoids bash-4 features (no `${var,,}`, no assoc arrays).
- `--build_event_json_file` is **newline-delimited JSON**, not a JSON array.
- Do not write a literal `~` into a generated `.bazelrc` (it is not expanded — an E1 concern).
