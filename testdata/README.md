# testdata — verify fixtures

Fixtures the broker is exercised against. Two kinds: a fast pure-bash stub for headless
`/verify`, and a real synthetic Bazel workspace for occasional true e2e.

## `fake-bazel.sh` — the BAZEL_REAL stub (fast path)

A pure-bash fake `bazel` that emits newline-delimited BEP JSON and is discoverable/killable.
Used by every later epic to turn kill/admission/ingest into 1-second checks.

```sh
# emit events, exit 0
FAKE_BAZEL_DURATION=2 testdata/fake-bazel.sh --build_event_json_file=/tmp/x.json
jq . < /tmp/x.json        # every line is valid JSON

# cancel: SIGTERM -> exit 8 (the graceful trap)
FAKE_BAZEL_DURATION=10 testdata/fake-bazel.sh --build_event_json_file=/tmp/c.json & pid=$!
sleep 0.4; kill -TERM "$pid"; wait "$pid"; echo "rc=$?"   # rc=8
```

Knobs: `FAKE_BAZEL_DURATION` (s, fractional ok), `FAKE_BAZEL_CACHE_HITS`/`FAKE_BAZEL_CACHE_MISSES`,
`FAKE_BAZEL_EXIT`. Flags honored: `--build_event_json_file=PATH`, `--invocation_id=ID`.

**Cancel contract.** Send **SIGTERM** for a reliable graceful cancel (exit **8**) in every
configuration. SIGINT is also trapped (and re-armed) for the interactive/real-tty bazel-client
case, but bash 3.2 sets SIGINT to `SIG_IGN` for `&`-launched processes, so tests assert the
cancel via SIGTERM. E3's real kill path must therefore be SIGTERM-first (or SIGINT to the process
*group*), then SIGKILL — a bare `kill -INT <pid>` to a backgrounded client is a no-op.

Used as a `BAZEL_REAL` target by `tools/bazel`:

```sh
BAZEL_REAL=testdata/fake-bazel.sh tools/bazel build //:gen --build_event_json_file=/tmp/w.json
```

## `workspace/` — synthetic real Bazel workspace (e2e path)

A bzlmod workspace with `//:hello` (sh_binary via `rules_shell`) and `//:gen` (a cacheable
`genrule` action). `make verify-e2e` runs a real `bazel build //:gen //:hello` here, writing a
genuine `--build_event_json_file` + `--profile=*.gz`. Skips cleanly when no bazel/bazelisk is on
PATH. The first run on a clean machine fetches `rules_shell` from the BCR (not network-hermetic);
subsequent runs hit the repository cache. Never on the fast `/verify` path.

## `ios-app/` — real iOS rules_apple fixture (end-to-end target)

Out of E0 scope; do not modify. The genuine end-to-end target the broker is built to observe.
