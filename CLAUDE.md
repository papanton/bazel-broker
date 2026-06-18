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
- **broker daemon (E2):** `make run-broker`;
  `TOKEN=$(jq -r .token ~/.config/bazel-broker/config.json)`;
  `curl 127.0.0.1:8765/healthz`  (`/healthz` is auth-exempt).
- **discovery/kill (E3):** launch fake-bazel long; `brokerctl ls`; `brokerctl kill <id>`.
  Broker's kill path must use SIGTERM / process-group SIGINT, not a bare `kill -INT <pid>`.
- **BEP/metrics (E4):** point ingest at a BEP json; `brokerctl metrics <id>`.
- **admission (E5):** start 3 fake builds with `max_concurrency=2`; the 3rd queues.
- **web (E7) / menubar (E8):** see their epic plans.

## Pitfalls baked in
- macOS bash is **3.2** — fake-bazel avoids bash-4 features (no `${var,,}`, no assoc arrays).
- `--build_event_json_file` is **newline-delimited JSON**, not a JSON array.
- Do not write a literal `~` into a generated `.bazelrc` (it is not expanded — an E1 concern).
