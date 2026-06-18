# E5 — Admission control & `tools/bazel` wrapper

> ⚠️ **Superseded where it conflicts with [`00-consolidated-review.md`](00-consolidated-review.md) + E2 §4 (frozen contract).** Conform before coding:
> - `POST /admission` returns **status-code + one-word body** (`200 ALLOW`/`202 QUEUE`/`403 DENY`; any other/unreachable → **fail open**) — matches E2 §4.2. Policy routes are `/admission/{pause,resume,drain}` + `/admission/release` + `GET /admission/status`.
> - Keep the Round-2 fixes: run bazel as a **child** (not `exec`) so the slot-release `trap` fires; cap-1 buffered verdict channel (no engine deadlock).
> - User decisions: **D3** (block vs kill vs hybrid), **fail-open vs fail-closed** default.

> Executable implementation plan for ONE epic of the Bazel Broker project.
> Block/queue builds **before** they oversubscribe the machine.

Status: **Draft v1** · Owner: Antonis · Last updated: 2026-06-17
Maps to architecture §3 (driver 5), §5 (C1), §8.4 · Milestone **M4** · Epic **E5** in `02-epics.md`
Depends on: **E2** (broker core + HTTP/WS API + registry) and **E3** (process discovery + kill + PID→worktree).
Target: macOS arm64 · Go 1.26 · Bazel 9.x · bash 3.2 (system `/bin/bash` — do **not** assume bash 4+).

---

## 1. Goal & scope recap

**Goal.** Add the only capability that genuinely requires a pre-exec interception point:
**block-before-build admission**. When too many worktrees try to build at once, the
machine thrashes (each iOS build spawns dozens of `clang`/`swiftc` processes) and *total*
throughput drops. E5 gates builds at the front door so the machine stays inside its
CPU/RAM budget, and queues the overflow instead of letting it pile on.

**In scope (from `02-epics.md` EPIC 5):**

1. `tools/bazel` wrapper (bash), committed to the iOS repo so it auto-propagates to every
   worktree (architecture driver 5). It: mints an `--invocation_id` (uuidgen), POSTs
   `/admission` and **blocks** until `ALLOW`/`QUEUE`-then-`ALLOW`, injects the E1 flag set,
   then `exec "$BAZEL_REAL" "$@"`. Provides `BROKER_BYPASS=1` escape hatch + CI guard.
   **Must `exec "$BAZEL_REAL"`, never `bazel`** (driver 5: else infinite recursion).
2. Broker **admission engine** (Go): start with a global semaphore (max N concurrent),
   then layer a CPU/RAM **token bucket** (load read via gopsutil, with a cgo
   `host_statistics64`/`host_processor_info` fallback documented).
3. **Pause / Resume / Drain** admission-policy endpoints (added to the E2 API).
4. **Anti–thundering-herd STAGGER** (§8.4 item 2): detect duplicate/overlapping target sets
   across worktrees and delay the second admission so the first populates `--disk_cache` →
   the second hits instead of recomputing.
5. **Keep-servers-warm** (§8.4 item 4): tune `--max_idle_secs`, avoid needless `shutdown`,
   so each worktree's in-memory analysis cache stays hot between builds.
6. Guards: skip admission in CI, optional bypass for Xcode-triggered builds.

**Out of scope (owned by other epics — do not re-implement):**
- BEP ingest / cache% / Perfetto (E4). The wrapper *injects* the flags E4 consumes but does
  not parse BEP.
- Process discovery, PID→worktree mapping, kill (E3). Admission *reuses* E3's snapshot.
- Registry / SQLite / HTTP server / launchd / auth token (E2). E5 *adds handlers* to it.
- Cache config / relocatability flags / `setup.sh` (E1). E5 *re-uses the same flag set* and
  must stay in sync with E1's canonical definition (see §4).
- `brokerctl` subcommands (E6). E5 exposes endpoints; `brokerctl drain/pause/resume` is E6's
  job, though we add a thin `curl`-based recipe for `/verify` here.

**Surfaced decision (not resolved here):** **D3** — admission model. This plan implements
the **block-before-build** arm and is structured so it can run as a **hybrid** alongside
E3's kill-based throttling, but the choice of default posture is left open (§6).

---

## 2. Design & implementation details

### 2.0 Architecture at a glance

```
  bazel/bazelisk launcher ─exec─▶ tools/bazel (bash, committed to iOS repo)
   (launcher finds <workspace>/tools/bazel, sets BAZEL_REAL=<real bazel>, re-execs it)
                                 │  mint invocation_id (uuidgen)
                                 │  POST /admission {worktree,targets,pid,invocation_id}
                                 │      └─ blocks: server holds the request open until ALLOW
                                 │         (long-poll); or returns QUEUE + Retry-After to re-poll
                                 ▼
                    ┌──────────────────────────────────────┐
                    │  BROKER  (Go, E2 daemon — we add to)   │
                    │  ┌────────────────────────────────┐    │
                    │  │ AdmissionEngine                 │    │
                    │  │   gate: Semaphore(maxN)         │    │
                    │  │   gate: TokenBucket(cpu,ram)    │◀── loadProbe (gopsutil / cgo)
                    │  │   gate: Stagger(target-overlap) │    │
                    │  │   policy: Pause/Resume/Drain     │    │
                    │  │   FIFO admission queue + waiters │    │
                    │  └────────────────────────────────┘    │
                    │   reads E3 registry snapshot (active   │
                    │   builds, their target sets) for stagger│
                    └──────────────────────────────────────┘
                                 │ ALLOW (HTTP 200, body: "ALLOW")
                                 ▼
  tools/bazel: inject E1 flags ──▶ run "$BAZEL_REAL" "$@" as child  (real build runs)
                                 │  (gated path RUNs+waits, not exec, so the trap can fire)
                                 │  on bazel exit (trap/EXIT): POST /admission/release {invocation_id}
                                 ▼
                    broker frees the semaphore/token slot, wakes the next waiter
```

**Why the response stays trivial (a status code + single word, no JSON):** the wrapper is
bash 3.2 with no `jq` guarantee. The broker answers `/admission` with an HTTP status code
**and** a one-word body (`ALLOW` / `QUEUE` / `DENY`), so the wrapper branches on either
without parsing. Any structured detail (queue position, reason) goes in **response headers**
(`X-Broker-*`), which bash reads cheaply if it wants to, and ignores otherwise.

**The `tools/bazel` contract (architecture §3 driver 5), stated precisely.** The thing that
re-execs us is the **Bazel launcher** — the native `bazel`/`bazelisk` client binary, *not* a
shell. On startup the launcher locates the workspace root, checks for an executable
`<workspace_root>/tools/bazel`, and if present **re-execs that script with `BAZEL_REAL` set to
the absolute path of the real bazel binary** it would otherwise have run, passing the original
argv through. (bazelisk implements the same hook; for bazelisk `BAZEL_REAL` points at the
bazelisk-resolved bazel.) Therefore in the *normal* flow `BAZEL_REAL` is **always set by the
launcher** and the wrapper's only job is to `exec "$BAZEL_REAL" "$@"`. The recursion hazard is
real but specific: the wrapper must **never** `exec bazel`/`exec bazelisk` (a bare name), because
that re-enters the launcher, which re-finds `tools/bazel`, which re-execs us → unbounded fork.
The `BAZEL_REAL`-unset fallback (someone ran `tools/bazel` directly, outside the launcher) must
break this loop explicitly — see §2.1 for the corrected guard (it both skips the script *and*
sets a `BB_WRAPPER_REENTRY` tripwire so even a mis-resolved `bazel` cannot recurse).

**Blocking model — long-poll, not busy-wait.** The default `/admission` call **blocks
server-side** (the Go handler holds the connection open on a `context`-aware wait until a
slot frees or the request times out) and then returns `200 ALLOW`. This means the *common*
case is a single request that returns the instant admission is granted — no polling loop in
bash. A `QUEUE` (HTTP 202) is only returned when the server-side wait hits its
per-request cap (`ADMISSION_POLL_SECONDS`, default 25s, kept under typical 30s proxy/idle
timeouts); the wrapper then re-POSTs (carrying its `invocation_id` so it keeps its queue
position). This makes the steady state cheap and the slow state robust.

> **Slot-vs-connection invariant (the load-bearing rule for §2.3).** A slot is *only*
> considered held once the client has been told `ALLOW` **and that ALLOW was actually
> delivered**. A waiter that timed out to `QUEUE`, or whose connection dropped, holds **no**
> slot — the engine must never acquire a gate for a waiter it can no longer hand `ALLOW` to.
> The release of a slot is driven by the wrapper's `/admission/release` (or the PID reaper),
> *not* by the long-poll connection closing. These two facts are what make the design correct
> under re-poll and client-disconnect; §2.3 implements them.

---

### 2.1 The full `tools/bazel` wrapper (bash)

Committed to the **iOS repo** at `tools/bazel` (mode `0755`). A reference copy lives in this
repo at `tools/bazel` for testing against `fake-bazel.sh`. It is dependency-free: only
`curl`, `uuidgen`, `mktemp`, and coreutils that ship with macOS.

```bash
#!/bin/bash
# tools/bazel — Bazel Broker admission wrapper.
# Committed to the iOS repo → auto-present in every git worktree (architecture §3 driver 5).
# Invoked by bazelisk/bazel with $BAZEL_REAL pointing at the real bazel binary.
# CONTRACT: must exec "$BAZEL_REAL" (NEVER "bazel") or it recurses forever.

set -u

# ---------------------------------------------------------------------------
# 0. Resolve the real bazel.
#
#    NORMAL PATH: the Bazel/bazelisk LAUNCHER (a native binary, not a shell)
#    finds <workspace>/tools/bazel, sets BAZEL_REAL to the absolute path of the
#    real bazel, and re-execs us. So BAZEL_REAL is almost always already set and
#    we just use it. We must NEVER `exec bazel`/`exec bazelisk` (a bare name) —
#    that re-enters the launcher, which re-finds tools/bazel → infinite recursion.
#
#    FALLBACK PATH: someone ran tools/bazel directly (BAZEL_REAL unset). We then
#    look up a real bazel/bazelisk on PATH that is NOT this script. Because that
#    candidate is itself a launcher that will re-find tools/bazel, we ALSO set a
#    re-entry tripwire (BB_WRAPPER_REENTRY) and check it on entry, so even a
#    mis-resolved candidate cannot recurse: a second entry with the tripwire set
#    aborts instead of looping.
# ---------------------------------------------------------------------------
if [ -n "${BB_WRAPPER_REENTRY:-}" ] && [ -z "${BAZEL_REAL:-}" ]; then
  echo "tools/bazel: re-entered without BAZEL_REAL (recursion guard tripped) — aborting" >&2
  exit 127
fi

self="$(cd "$(dirname "$0")" && pwd -P)/$(basename "$0")"
if [ -z "${BAZEL_REAL:-}" ]; then
  # Find a bazel/bazelisk on PATH that is NOT this script (compare resolved paths).
  for cand in bazelisk bazel; do
    while IFS= read -r p; do
      [ -x "$p" ] || continue
      rp="$(cd "$(dirname "$p")" 2>/dev/null && pwd -P)/$(basename "$p")"
      [ "$rp" = "$self" ] && continue           # never pick ourselves
      BAZEL_REAL="$p"; break 2
    done <<EOF
$(command -v -a "$cand" 2>/dev/null)
EOF
  done
  # Arm the tripwire for the (PATH-resolved) launcher we are about to invoke, so
  # if it re-execs this wrapper without BAZEL_REAL, the guard above aborts.
  export BB_WRAPPER_REENTRY=1
fi
if [ -z "${BAZEL_REAL:-}" ]; then
  echo "tools/bazel: cannot locate real bazel (BAZEL_REAL unset)" >&2
  exit 127
fi

# ---------------------------------------------------------------------------
# 1. Escape hatches & guards — bypass admission entirely (still exec real bazel).
#    - BROKER_BYPASS=1            : explicit developer opt-out.
#    - CI                        : standard CI env var (GitHub Actions, etc.).
#    - non-build commands        : `bazel version|info|help|shutdown|clean` etc.
#                                  must NOT block (used by tooling/Xcode/IDE probes).
# ---------------------------------------------------------------------------
BROKER_URL="${BROKER_URL:-http://127.0.0.1:8765}"
BROKER_TOKEN="${BROKER_TOKEN:-}"          # bearer token from E2 config (optional locally)

# Find the command verb: first non-flag arg. NOTE: a few startup flags take a SPACE-separated
# value (e.g. `--bazelrc PATH`, `--host_jvm_args X`); a value could be mistaken for the verb.
# That is SAFE here: a mistaken non-allowlisted token → needs_admission=0 → we bypass (exec real
# bazel) rather than wrongly gate. We never *gate* on a false positive; worst case we skip the
# gate, which is the fail-open direction we want. (The launcher passes startup flags `=`-joined in
# practice, so this is rare regardless.)
bazel_cmd=""
for a in "$@"; do
  case "$a" in
    --*) continue ;;                       # skip startup/leading flags
    *)   bazel_cmd="$a"; break ;;
  esac
done

needs_admission=0
case "$bazel_cmd" in
  build|test|run|coverage|mobile-install|aquery|cquery) needs_admission=1 ;;
  *) needs_admission=0 ;;                  # version/info/help/shutdown/clean/query → skip
esac

if [ "${BROKER_BYPASS:-0}" = "1" ] || [ -n "${CI:-}" ] || [ "$needs_admission" -eq 0 ]; then
  exec "$BAZEL_REAL" "$@"
fi

# ---------------------------------------------------------------------------
# 2. Mint a stable invocation id and identify the worktree.
#    We mint our own so we can (a) tag the BEP/profile files per-invocation and
#    (b) keep our queue slot across re-polls. Bazel also auto-generates one, but
#    --invocation_id pins it to ours so the broker can correlate.
# ---------------------------------------------------------------------------
INVOCATION_ID="$(uuidgen | tr 'A-Z' 'a-z')"
WORKTREE="$(cd "$(dirname "$0")/.." && pwd -P)"   # tools/bazel lives in <worktree>/tools/
TARGETS=""                                          # space-joined positional targets
for a in "$@"; do
  case "$a" in
    -*) ;;                                          # flag
    build|test|run|coverage|mobile-install|aquery|cquery|query|info|version|help|clean|shutdown) ;;
    *) TARGETS="${TARGETS:+$TARGETS }$a" ;;
  esac
done

# ---------------------------------------------------------------------------
# 3. Request admission. Long-poll: the broker holds the request open until a
#    slot frees, then returns "ALLOW". A 202 "QUEUE" means "re-poll" (server hit
#    its per-request cap); any error / unreachable broker → FAIL-OPEN (build anyway).
#    The single-word body + status code means NO json parsing in bash.
# ---------------------------------------------------------------------------
admission_body=""
admission_code=""
post_admission() {
  # Writes HTTP status to admission_code and body (first line) to admission_body.
  local out
  out="$(curl -sS \
      --max-time "${ADMISSION_CURL_TIMEOUT:-40}" \
      -o - -w '\n%{http_code}' \
      -H "Content-Type: application/json" \
      ${BROKER_TOKEN:+-H "Authorization: Bearer $BROKER_TOKEN"} \
      -X POST "$BROKER_URL/admission" \
      --data "{\"invocation_id\":\"$INVOCATION_ID\",\"worktree\":\"$WORKTREE\",\"pid\":$$,\"targets\":\"$TARGETS\"}" \
      2>/dev/null)" || { admission_code="000"; return 1; }
  admission_code="${out##*$'\n'}"           # last line = http_code
  admission_body="${out%$'\n'*}"            # everything before = body
  admission_body="$(printf '%s' "$admission_body" | head -n1 | tr -d '[:space:]')"
  return 0
}

attempt=0
max_attempts="${ADMISSION_MAX_ATTEMPTS:-240}"   # 240 * ~25s ≈ 100 min ceiling
while :; do
  attempt=$((attempt + 1))
  if ! post_admission; then
    echo "tools/bazel: broker unreachable at $BROKER_URL — proceeding (fail-open)" >&2
    break                                    # FAIL-OPEN: never block a build on a down broker
  fi
  case "$admission_code" in
    200) break ;;                            # ALLOW
    202) ;;                                  # QUEUE → loop & re-poll (keeps our invocation_id slot)
    403) echo "tools/bazel: admission DENIED by broker (drain?) — not building" >&2
         exit 75 ;;                          # EX_TEMPFAIL: explicit, deliberate denial
    *)   echo "tools/bazel: broker returned $admission_code — proceeding (fail-open)" >&2
         break ;;
  esac
  if [ "$attempt" -ge "$max_attempts" ]; then
    echo "tools/bazel: admission timed out after $attempt polls — proceeding (fail-open)" >&2
    break
  fi
done

# ---------------------------------------------------------------------------
# 4. Release-on-exit. CRITICAL CORRECTNESS POINT: on the admitted path we must
#    NOT `exec` bazel — `exec` replaces this shell, so a `trap … EXIT` would
#    NEVER fire and /admission/release would never be sent (the slot would only
#    free via the 5s PID reaper). Instead we RUN bazel as a child, forward
#    signals, wait, then release and propagate bazel's exit code. (The bypass
#    paths in steps 0–1 still use a clean `exec`, since they hold no slot.)
# ---------------------------------------------------------------------------
release() {
  curl -sS --max-time 5 \
    ${BROKER_TOKEN:+-H "Authorization: Bearer $BROKER_TOKEN"} \
    -X POST "$BROKER_URL/admission/release" \
    --data "{\"invocation_id\":\"$INVOCATION_ID\"}" >/dev/null 2>&1 || true
}
trap release EXIT          # fires because we WAIT (do not exec) on the gated path
# Forward Ctrl-C / TERM to the bazel child so the build cancels gracefully, then
# the EXIT trap releases the slot. $bazel_pid is set just below.
forward_sig() { [ -n "${bazel_pid:-}" ] && kill -s "$1" "$bazel_pid" 2>/dev/null || true; }
trap 'forward_sig INT'  INT
trap 'forward_sig TERM' TERM

# ---------------------------------------------------------------------------
# 5. Inject the E1 flag set (cache sharing + relocatability + per-invocation
#    BEP/profile). These MUST match E1's canonical definition (§4). Paths are
#    per-invocation so concurrent builds never clobber each other (architecture C3 warning).
#    --max_idle_secs keeps the server warm (§8.4 item 4 / keep-servers-warm).
# ---------------------------------------------------------------------------
BROKER_CACHE_DIR="${BROKER_CACHE_DIR:-$HOME/.cache/bazel-broker/disk_cache}"
BROKER_REPO_CACHE="${BROKER_REPO_CACHE:-$HOME/.cache/bazel-broker/repo_cache}"
BROKER_EVENT_DIR="${BROKER_EVENT_DIR:-$HOME/.local/state/bazel-broker/bep}"
BROKER_PROFILE_DIR="${BROKER_PROFILE_DIR:-$HOME/.local/state/bazel-broker/profiles}"
mkdir -p "$BROKER_CACHE_DIR" "$BROKER_REPO_CACHE" "$BROKER_EVENT_DIR" "$BROKER_PROFILE_DIR" 2>/dev/null || true

BEP_FILE="$BROKER_EVENT_DIR/$INVOCATION_ID.bep.json"
PROFILE_FILE="$BROKER_PROFILE_DIR/$INVOCATION_ID.profile.gz"

# Startup flags (must precede the command) vs command flags (after it).
# We split args: everything up to & including the command verb is "head"
# (startup flags + the command), the rest is "tail" (command flags + targets).
#
# Subtleties handled:
#  - Only the FIRST bare word is the command verb. A later positional that happens
#    to equal a command name (e.g. a target literally named "test") must NOT be
#    re-detected — hence we stop scanning once seen_cmd=1.
#  - A literal "--" ends bazel's OWN flag parsing (everything after it is passed to
#    the built binary under `run`). We must place our INJECTED_BUILD flags BEFORE any
#    "--", never after it, or they'd be handed to the target program. We therefore
#    track the "--" boundary and inject build flags just before it.
head_args=()
mid_args=()      # command flags/targets BEFORE a "--"
post_dd=()       # everything from "--" onward (passed through untouched)
seen_cmd=0
seen_dd=0
for a in "$@"; do
  if [ "$seen_dd" -eq 1 ]; then
    post_dd+=("$a"); continue
  fi
  if [ "$a" = "--" ]; then
    seen_dd=1; post_dd+=("$a"); continue
  fi
  if [ "$seen_cmd" -eq 0 ]; then
    head_args+=("$a")
    case "$a" in
      build|test|run|coverage|mobile-install|aquery|cquery) seen_cmd=1 ;;
    esac
  else
    mid_args+=("$a")
  fi
done

INJECTED_STARTUP=( "--max_idle_secs=${BROKER_MAX_IDLE_SECS:-10800}" )   # keep server warm 3h
INJECTED_BUILD=(
  "--invocation_id=$INVOCATION_ID"
  "--build_event_json_file=$BEP_FILE"
  "--generate_json_trace_profile"        # enable the profile (E1 sets this too; --profile alone is not enough on all configs)
  "--profile=$PROFILE_FILE"
  "--disk_cache=$BROKER_CACHE_DIR"
  "--repository_cache=$BROKER_REPO_CACHE"
  # Relocatability — keep IN SYNC with E1 (§4). The config-independent flags are safe to
  # hardcode; if E1's .bazelrc already sets them, Bazel de-dupes (last identical value wins).
  "--incompatible_strict_action_env"
  "--experimental_output_paths=strip"
  # Per-WORKTREE prefix-maps. These are NOT config-independent (they embed $WORKTREE), so the
  # static INJECTED_BUILD in E1's *fragment* cannot hardcode them — but the WRAPPER can, because
  # it resolved $WORKTREE above. Injecting them here is what lets a --ignore_all_rc_files build
  # (which drops E1's .bazelrc) still get relocatable, cache-hitting actions. Must mirror E1 §2.5
  # exactly (same `=.` target, same copt/objccopt/swiftcopt spread) or cross-worktree hits regress.
  "--copt=-ffile-prefix-map=$WORKTREE=."
  "--copt=-fdebug-prefix-map=$WORKTREE=."
  "--objccopt=-ffile-prefix-map=$WORKTREE=."
  "--objccopt=-fdebug-prefix-map=$WORKTREE=."
  "--swiftcopt=-ffile-prefix-map=$WORKTREE=."
)

# Assemble argv:
#   [startup flags...] [user startup flags + COMMAND] [INJECTED_BUILD] [user cmd flags/targets] [-- passthrough]
#
# bash 3.2 + set -u gotcha: "${arr[@]}" on an EMPTY array expands to an unbound-variable
# error under `set -u`. Guard each possibly-empty array with ${arr[@]+"${arr[@]}"} so an
# empty array expands to nothing instead of erroring. (head_args is never empty — it holds
# at least the command — but mid_args/post_dd often are.)
#
# RUN (not exec) so the EXIT trap fires and we POST /admission/release. We background bazel,
# forward signals to it (traps above), wait, capture its exit code, and exit with it.
"$BAZEL_REAL" \
  "${INJECTED_STARTUP[@]}" \
  "${head_args[@]}" \
  "${INJECTED_BUILD[@]}" \
  ${mid_args[@]+"${mid_args[@]}"} \
  ${post_dd[@]+"${post_dd[@]}"} &
bazel_pid=$!

# `wait` returns when bazel exits OR when a forwarded signal interrupts it. Loop so a
# trapped signal (which interrupts `wait`) re-waits for bazel's actual termination, giving
# us bazel's true exit status (and its graceful-cancel code on Ctrl-C).
bazel_rc=0
while :; do
  if wait "$bazel_pid"; then bazel_rc=0; else bazel_rc=$?; fi
  # If bazel is gone, stop; if `wait` returned due to a signal while bazel still lives, re-wait.
  kill -0 "$bazel_pid" 2>/dev/null || break
done

release          # explicit (the EXIT trap also calls it; release is idempotent server-side)
trap - EXIT      # avoid a duplicate release on exit
exit "$bazel_rc"
```

> **Why not `exec` on the gated path:** `exec` replaces the shell image, so any `trap … EXIT`
> is discarded and `/admission/release` would never be sent — the slot would linger until the
> PID reaper notices (≤5s after the build ends). Running bazel as a child and `wait`ing keeps
> release prompt and exit-code-faithful. The cost is one extra live shell per gated build
> (negligible) and the need to forward `INT`/`TERM` (done above) so Ctrl-C still cancels the
> build gracefully and yields bazel's cancel exit code. The **bypass** paths (BROKER_BYPASS / CI
> / non-build commands) keep the clean single `exec` since they hold no slot and need no release.

**Notes / decisions baked into the script**

- **Recursion guard (driver 5):** in the normal flow the launcher always sets `BAZEL_REAL`, so
  the wrapper just `exec "$BAZEL_REAL"`. The danger is only the **fallback** (someone runs
  `tools/bazel` directly): the PATH-resolved `bazel`/`bazelisk` we find there is *itself a
  launcher* that will re-find `tools/bazel`. We defend with **two** independent guards: (a)
  resolved-path comparison so we never pick *this* script (`[ "$rp" = "$self" ]`, using `pwd -P`
  on both sides so symlinked worktrees compare equal), and (b) a `BB_WRAPPER_REENTRY` env
  tripwire — exported before invoking the fallback launcher and checked on entry — so even a
  mis-resolved candidate that re-execs us without `BAZEL_REAL` aborts on the second entry instead
  of fork-bombing. The recursion test (§5.2) asserts a stable process count, not just exit 127.
- **Non-build commands skip the gate.** `version/info/help/shutdown/clean/query` are used by
  Xcode/IDE/tooling probes; blocking them would hang editors. Only
  `build/test/run/coverage/mobile-install/aquery/cquery` request admission.
- **Fail-open everywhere a broker problem is detected** (unreachable, non-200/202/403,
  timeout). A down broker must **never** prevent a developer or agent from building. Only an
  explicit `403` (drain) deliberately stops the build (exit 75, `EX_TEMPFAIL`). This
  fail-open vs fail-closed posture is **open decision** — see §6.
- **`set -u`, no `set -e`.** We want our own explicit error handling, not abort-on-error
  (curl returning non-zero must fall through to fail-open, not kill the script).
- **Exec on bypass, run-and-wait on the gated path.** The bypass branches (`BROKER_BYPASS`/`CI`/
  non-build commands) `exec "$BAZEL_REAL"` for clean process semantics. The **gated** path must
  instead **run bazel as a child and `wait`**, because `exec` would discard the `EXIT` trap and
  skip `/admission/release` (slot would only free via the 5s reaper). The trade-off (one extra
  shell + explicit signal forwarding) is documented at the exec site.
- **bash 3.2 safe.** Uses `tr 'A-Z' 'a-z'` (not `${x,,}`), arrays (OK in 3.2),
  `<<EOF` heredoc (not `<<<`), and avoids `mapfile`.

---

### 2.2 `/admission` request/response protocol

Deliberately minimal so bash needs no JSON parser on the response.

**Request** — `POST /admission` (JSON body; the wrapper *writes* JSON, which is easy):

```json
{ "invocation_id": "f1e2…", "worktree": "/wt/feature-a", "pid": 40321, "targets": "//app:app //lib:lib" }
```

**Response — status code carries the verdict; body is one word; details in headers:**

| HTTP | Body    | Meaning | Wrapper action |
|------|---------|---------|----------------|
| 200  | `ALLOW` | admitted (slot held for this invocation_id) | proceed to exec |
| 202  | `QUEUE` | server-side wait cap hit; still queued | re-POST (same invocation_id) |
| 403  | `DENY`  | drain mode / hard policy denial | exit 75, do **not** build |
| 429  | `QUEUE` | (alias for 202 if a proxy strips 202 semantics) | re-POST |
| 5xx/000 | *(any)* | broker error/unreachable | **fail-open**: build anyway |

Optional response headers (read-cheap from bash, but never required):
`X-Broker-Queue-Position: 2`, `X-Broker-Reason: cpu-token`, `Retry-After: 5`,
`X-Broker-Wait-Ms: 25000`.

`POST /admission/release` — `{ "invocation_id": "f1e2…" }` → `204 No Content`. **Idempotent**
(releasing an unknown/already-released id is a `204` no-op). Releases the waiter's gates,
deletes it from `byID`, and calls `schedule()` to wake the next FIFO waiter. This is the
**primary** slot-release path; the PID-liveness reaper (T8) is the backstop for clients that die
before sending it.

**Long-poll semantics (server side), reconciled with the §2.3 state machine:**
- The handler `attach`es the request as a waiter (deduped by `invocation_id`) and selects on its
  cap-1 buffered verdict channel for up to `ADMISSION_POLL_SECONDS` (25s).
- **Admitted within the window** → `200 ALLOW`.
- **Window expires while still `wsQueued`** → `202 QUEUE`. The waiter **stays enqueued** in FIFO
  order; the wrapper's re-POST re-`attach`es by `invocation_id`. (A short GC grace tolerates the
  gap between the `202` response and the re-POST so a genuinely-polling client is never dropped
  from its FIFO slot; a client that never re-polls is GC'd.)
- **Admitted but the window expired before the handler drained `ALLOW`** (admit raced the
  timeout) → handler returns `202 QUEUE`, but the `ALLOW` is **buffered**; the re-POST re-attaches
  and immediately drains it → `200 ALLOW` with no slot lost.
- **Client disconnects** (`ctx.Done`) → handler returns; `detach` drops the waiter only if it was
  still `wsQueued` (no gates held). An already-admitted-but-orphaned waiter is left for the reaper.
- **Engine draining** → `403 DENY`.

---

### 2.3 Admission engine (Go) — semaphore → token bucket → stagger

Lives in `internal/admission/` in the broker module. Wired into the E2 HTTP mux.

#### 2.3.1 Core types & gate interface

```go
package admission

import (
	"context"
	"sync"
	"time"
)

// Verdict is the single-word outcome the HTTP layer maps to a status code.
type Verdict int

const (
	Allow Verdict = iota // 200 ALLOW
	Queue                // 202 QUEUE (wait window expired, still waiting)
	Deny                 // 403 DENY (drain / hard policy)
)

// Request is what the wrapper POSTs.
type Request struct {
	InvocationID string   `json:"invocation_id"`
	Worktree     string   `json:"worktree"`
	PID          int      `json:"pid"`
	Targets      string   `json:"targets"` // space-joined; engine splits to a set
}

// Gate is one admission stage. Try is non-blocking: it returns whether this
// request may pass *right now*, and if not, a hint for how long to wait before
// re-evaluating (0 = wake me on the next release/event).
type Gate interface {
	Name() string
	// Try reports admit=true if the request may pass this gate now.
	// retryAfter is a soft hint (used for stagger / token refill backoff).
	Try(ctx context.Context, req Request, snap RegistrySnapshot) (admit bool, retryAfter time.Duration)
	// Acquire / Release bookkeep occupancy. Called only when ALL gates admit.
	Acquire(req Request)
	Release(invocationID string)
}
```

#### 2.3.2 Engine: FIFO queue + policy + gate chain

```go
type Policy struct {
	MaxConcurrent  int           // global semaphore size
	CPUHighWater   float64       // e.g. 0.85 — admit only if CPU busy% is below this
	RAMPressureMax int           // macOS memory-pressure level to allow up to: 1=normal,2=warn,4=critical
	                             //   admit while level <= this (default: allow up to 1=normal only,
	                             //   i.e. hold on warn+). NOT a RAM fraction — see §2.3.4.
	StaggerWindow  time.Duration // e.g. 8s — delay 2nd overlapping target set
	PollSeconds    time.Duration // server-side long-poll cap (25s)
	Draining       bool
	Paused         bool
}

type Engine struct {
	mu       sync.Mutex
	policy   Policy
	gates    []Gate            // ordered: semaphore, tokenBucket, stagger
	queue    []*waiter         // FIFO; head is next eligible
	byID     map[string]*waiter
	registry RegistryReader    // E3 snapshot source
	load     LoadProbe         // gopsutil / cgo
	notify   chan struct{}     // coalesced wake on release/policy/load tick
}

type waiterState int

const (
	wsQueued   waiterState = iota // in FIFO, no verdict yet
	wsAdmitted                    // gates acquired, ALLOW pending delivery / delivered
	wsDone                        // terminal verdict delivered or abandoned
)

type waiter struct {
	req      Request
	enqueued time.Time
	state    waiterState
	// connected: a live long-poll handler is currently selecting on this waiter.
	// Set on (re-)attach, cleared when the handler returns (timeout/disconnect).
	connected bool
	// ch is BUFFERED (cap 1) and single-shot. schedule() never blocks on it:
	// it does a non-blocking send. If no handler is connected, the buffered
	// verdict is picked up by the next re-poll that re-attaches. This is the fix
	// for the "schedule() deadlocks holding e.mu sending to a gone handler" bug.
	ch chan Verdict
}

// Admit is called by the HTTP handler. It blocks up to PollSeconds, then returns
// Queue so the wrapper re-polls. CRITICAL invariants:
//   - Acquiring gates and marking a waiter Admitted happens ONLY inside schedule()
//     under e.mu, and the resulting ALLOW is buffered (cap-1 chan) so delivery never
//     depends on a handler being connected at that instant.
//   - A timed-out/disconnected handler does NOT release the waiter's gates. If the
//     waiter was already Admitted, its ALLOW sits buffered; the re-poll re-attaches
//     and drains it. If it was only Queued, ctx-cancel drops it (no gates held).
func (e *Engine) Admit(ctx context.Context, req Request) Verdict {
	w := e.attach(req) // dedupe by invocation_id; (re-)attaches handler, marks connected.
	defer e.detach(w)  // clears connected; drops the waiter ONLY if still wsQueued (no gates held).

	// Fast path: a verdict may already be buffered from a prior poll cycle
	// (admitted between this client's polls). Non-blocking peek first.
	select {
	case v := <-w.ch:
		return v
	default:
	}

	timer := time.NewTimer(e.policy.PollSeconds)
	defer timer.Stop()
	select {
	case v := <-w.ch: // Allow or Deny, delivered by schedule()
		return v
	case <-timer.C:
		return Queue // tell wrapper to re-poll; waiter stays enqueued (or stays Admitted with ALLOW buffered)
	case <-ctx.Done():
		return Queue // client hung up; detach() drops it if unadmitted, reaper handles admitted-but-orphaned
	}
}

// detach is the slot-release-safety hinge. It NEVER releases gates for an admitted
// waiter (that is the wrapper's /admission/release job, backed by the PID reaper).
// It only garbage-collects a waiter that is still queued and whose client is gone.
func (e *Engine) detach(w *waiter) {
	e.mu.Lock()
	defer e.mu.Unlock()
	w.connected = false
	if w.state == wsQueued {
		// No gates held. If the client's context is truly done (not just a poll
		// timeout that will re-poll), drop it from the FIFO so it can't wedge the head.
		// A poll-timeout keeps it enqueued (connected will be re-set on re-poll); a real
		// disconnect is distinguished by ctx.Err()==Canceled, checked by the caller path.
		// Implementation: detach removes the waiter only when it has NOT re-attached
		// within a short grace (a lazy GC tick), which both re-poll and disconnect tolerate.
		e.scheduleQueuedGC(w) // marks lastDetached; a GC pass removes if not re-attached in grace.
	}
	// If wsAdmitted: leave it. ALLOW is buffered; gates are held until release/reaper.
}

// schedule() runs whenever something changes (release, policy flip, load tick, enqueue).
// It walks the FIFO head and admits as many as the gate chain allows. It NEVER blocks
// on a waiter channel (cap-1 buffered + non-blocking send), so it can hold e.mu safely.
func (e *Engine) schedule() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.policy.Paused {
		return // hold everyone; no DENY, they just wait
	}
	snap := e.registry.Snapshot()
	for len(e.queue) > 0 {
		w := e.queue[0]
		if w.state != wsQueued { // already admitted (ALLOW buffered, awaiting re-poll); pop it.
			e.queue = e.queue[1:]
			continue
		}
		if e.policy.Draining {
			e.deliver(w, Deny) // non-blocking buffered send
			w.state = wsDone
			e.queue = e.queue[1:]
			delete(e.byID, w.req.InvocationID)
			continue
		}
		admit, retry := e.tryAllGates(w.req, snap)
		if !admit {
			// Head can't pass yet (e.g. stagger window, no token). Do NOT skip it
			// (FIFO fairness). Arm a timer for the soft retry hint so a stagger/token
			// wait re-runs schedule() even with no external event.
			e.armRetryTimer(w, retry)
			return
		}
		e.acquireAllGates(w.req) // gates now held for w.req.InvocationID
		w.state = wsAdmitted
		e.deliver(w, Allow) // buffered; picked up by the connected handler or the next re-poll
		e.queue = e.queue[1:]
		// Keep w in byID until release so a re-poll re-attaches to the SAME admitted waiter
		// and drains its buffered ALLOW instead of enqueuing a duplicate.
		snap = e.registry.SnapshotWith(w.req) // reflect the just-admitted build for stagger
	}
}

// deliver does a non-blocking send on the cap-1 buffered channel. Because ch is
// drained at most once per verdict and refilled never, a full buffer means a verdict
// is already pending — we drop the duplicate. This is what lets schedule() hold e.mu.
func (e *Engine) deliver(w *waiter, v Verdict) {
	select {
	case w.ch <- v:
	default: // verdict already buffered; nothing to do
	}
}
```

**Waiter lifecycle & every slot-release path (the part that has to be airtight).**

| Event | Waiter state | Gates | What frees the slot |
|---|---|---|---|
| enqueue (first POST) | `wsQueued` | none | — (nothing to free) |
| poll timeout → `202 QUEUE` | stays `wsQueued` | none | re-POST re-attaches; GC drops it only if no re-poll within grace |
| client disconnect while queued | `wsQueued` → GC'd | none | `detach` GC removes from FIFO; no slot was ever held |
| `schedule()` admits | `wsAdmitted` | **held** | buffered `ALLOW`; **slot freed by `/admission/release` or PID reaper** |
| client gets `ALLOW`, build runs, exits | `wsAdmitted` | held | wrapper `trap … EXIT` POSTs `/admission/release` |
| client gets `ALLOW` buffered but disconnects before draining it (rare) | `wsAdmitted` | held | **PID reaper** (T8): pid dead → release (≤5s) |
| `/drain` | `wsQueued` → `wsDone` (`DENY`) | none | — |

The single most important property: **the long-poll connection closing never releases a
slot.** Slot release is exclusively `/admission/release` (happy path) or the PID-liveness
reaper (crash path). This decouples HTTP transport lifetime from admission accounting, which is
what makes re-poll and disconnect safe. Conversely, **gates are acquired exactly once**, inside
`schedule()` under `e.mu`, and the admitted waiter is kept in `byID` (not just the FIFO) until
release — so a re-poll with the same `invocation_id` re-attaches to the *same admitted waiter*
and drains its buffered `ALLOW` rather than enqueuing a duplicate or re-acquiring a second slot.
The semaphore's `Acquire`/`Try` are additionally idempotent on `invocation_id` (already counted
→ no-op) as a belt-and-suspenders against any double-acquire.

**Why a cap-1 buffered, single-shot channel (not an unbuffered one).** With an unbuffered
channel, `schedule()`'s `w.ch <- Allow` would block while holding `e.mu` whenever the handler
has already returned `Queue`/disconnected — deadlocking the whole engine. Buffering decouples
the admit decision from delivery: the verdict is *produced* once and *consumed* by whichever
poll re-attaches, with `detach`/re-attach toggling `connected` purely for GC bookkeeping.

#### 2.3.3 Gate 1 — global semaphore (ship this first)

```go
type Semaphore struct {
	mu   sync.Mutex
	max  int
	held map[string]struct{} // invocation_id → slot
}

func (s *Semaphore) Try(_ context.Context, req Request, _ RegistrySnapshot) (bool, time.Duration) {
	s.mu.Lock(); defer s.mu.Unlock()
	if _, ok := s.held[req.InvocationID]; ok { return true, 0 } // already counted
	return len(s.held) < s.max, 0
}
func (s *Semaphore) Acquire(req Request)        { s.mu.Lock(); s.held[req.InvocationID] = struct{}{}; s.mu.Unlock() }
func (s *Semaphore) Release(id string)          { s.mu.Lock(); delete(s.held, id); s.mu.Unlock() }
func (s *Semaphore) Name() string               { return "semaphore" }
```

This alone satisfies the epic's "Done when" (3 builds, max-concurrency=2 → third queues then
admits). Ship it, verify it, then layer the token bucket.

#### 2.3.4 Gate 2 — CPU/RAM token bucket (load-aware)

The token bucket prevents admitting even *within* `MaxConcurrent` if the machine is already
hot (e.g. a non-Bazel process is pegging the CPU). It reads live load and only admits when
headroom remains.

```go
type LoadProbe interface {
	// Sample returns cpuBusy in [0,1] (1 = fully busy) and the macOS memory-pressure
	// level (1=normal, 2=warn, 4=critical). On non-darwin the pressure level is synthesized
	// from a free-page ratio behind the same interface.
	Sample() (cpuBusy float64, ramPressure int, err error)
}

type TokenBucket struct {
	probe   LoadProbe
	cpuHi   float64 // CPUHighWater
	ramPmax int     // RAMPressureMax (admit while level <= this)
	mu      sync.Mutex
	cpu     float64 // cached last sample (refreshed by a 1s ticker; first sample primed+discarded)
	ramP    int
}

func (t *TokenBucket) Try(_ context.Context, _ Request, snap RegistrySnapshot) (bool, time.Duration) {
	t.mu.Lock(); cpu, ramP := t.cpu, t.ramP; t.mu.Unlock()
	// Anti-stampede debit: load is sampled at ~1s cadence, but `schedule()` can admit several
	// builds within one sample window before their clang/swiftc spawn shows up in load. Debit a
	// small synthetic load per just-admitted-but-not-yet-reflected build so we don't admit a herd
	// in one tick. `snap.ActiveBuilds` counts builds admitted since the last load sample.
	const rampPerBuild = 0.10 // ~10% CPU reserved per in-ramp build; tune empirically (was 0.0)
	effCPU := cpu + float64(snap.RecentlyAdmitted)*rampPerBuild
	if effCPU >= t.cpuHi   { return false, 2 * time.Second }
	if ramP   >  t.ramPmax { return false, 2 * time.Second }
	return true, 0
}
// Acquire/Release: no-op (occupancy is the semaphore's job); Name() "tokenbucket".
```

**Reading load on macOS arm64 — two options (D-stack, surfaced):**

- **Option A (recommended, default): `gopsutil`** (`github.com/shirou/gopsutil/v4`).
  - CPU: `cpu.Percent(0, false)` returns busy% **since the previous call** (the `0` interval is
    non-blocking and diffs against gopsutil's last sample). The **first** call after startup
    returns a meaningless `0` (no prior sample), so the sampling ticker must **prime** with one
    discarded call, then read every 1s. Do **not** use `cpu.Percent(interval>0, …)` on the hot
    path — it sleeps for `interval`, which would stall the gate.
  - RAM: this is where naive gopsutil is **actively misleading on macOS**. `vm.UsedPercent` /
    `vm.Used` count wired+active+inactive+compressed, and macOS keeps inactive/file-backed pages
    resident aggressively, so `UsedPercent` sits near 80–90% on a healthy idle Mac — gating on it
    would refuse admission constantly. The honest signals, in order of preference:
    1. **Memory-pressure level** — `sysctl kern.memorystatus_vm_pressure_level`
       (1=normal, 2=warn, 4=critical). This is what the OS itself uses to decide when to compress
       /swap; gate on `>= warn`. gopsutil does **not** expose this, so even in "Option A" we read
       it via a tiny `unix.SysctlUint32` call (no cgo). **This is the recommended RAM signal.**
    2. **Swap activity** — `mem.SwapMemory()` rising `Sin`/`Sout` indicates real pressure.
    3. Only as a last resort, a free-page ratio (`vm.Available/vm.Total`), treating
       inactive+purgeable as available — not raw `UsedPercent`.
  - **Net:** CPU via gopsutil is fine; **RAM should gate on `memorystatus_vm_pressure_level`
    (sysctl), not gopsutil's `UsedPercent`.** The `RAMHighWater` policy knob is reinterpreted as a
    pressure-level threshold, not a fraction, on macOS (documented in the policy comment).
- **Option B (fallback / no-cgo-dep concern): cgo `host_statistics64` + `host_processor_info`.**
  - CPU: `host_processor_info(HOST_PROCESSOR_LOAD_INFO)` → per-core `cpu_ticks[USER|SYSTEM|
    NICE|IDLE]`; busy = (user+sys+nice)/total, diffed across two samples.
  - RAM: `host_statistics64(HOST_VM_INFO64, vm_statistics64)` → `active + wired + compressed`
    pages × page size, over `sysctl hw.memsize`.
  - Pressure: `sysctl kern.memorystatus_vm_pressure_level` (1=normal,2=warn,4=critical).

  Sketch:
  ```go
  // +build darwin
  // #include <mach/mach.h>
  // #include <mach/processor_info.h>
  // #include <mach/host_info.h>
  import "C"
  // host_statistics64(mach_host_self(), HOST_VM_INFO64, &vmstat, &count)
  // host_processor_info(mach_host_self(), PROCESSOR_CPU_LOAD_INFO, &n, &info, &cnt)
  ```

  **Open D-stack decision:** gopsutil (simpler, pure-ish Go, already a likely dep) vs cgo
  host_statistics (no extra module, exact same primitives, but cgo cross-compile friction).
  Default: **gopsutil**, with the cgo probe behind the same `LoadProbe` interface so we can
  swap without touching the engine.

#### 2.3.5 Gate 3 — STAGGER (anti–thundering-herd, §8.4 item 2)

When two worktrees request admission for **overlapping target sets** at nearly the same time,
both would miss the shared `--disk_cache` (neither has written results yet) and both recompute
the identical actions. Stagger admits the first immediately, then **holds the overlapping
second** for `StaggerWindow` so the first can populate the disk cache → the second's actions
become hits.

**Precise definition of "overlap" and the stagger rule (v1).** Let `T(req)` be the set of
**raw target-pattern strings** the request carries (as the wrapper joined them, e.g.
`//app:app`, `//lib/...`). Two requests *overlap* iff `T(a) ∩ T(b) ≠ ∅` under **normalized
string equality** — we normalize each pattern by (a) trimming whitespace, (b) stripping a
redundant leading `//` vs `@//` repo prefix to a canonical form, and (c) lower-casing nothing
(labels are case-sensitive). We deliberately do **not** expand `...` wildcards or resolve
package globs in v1 (that needs the action graph); we treat patterns as opaque strings. The
stagger rule: a request is **held** iff it overlaps an *active or recently-admitted* request
whose admit time is within `StaggerWindow`, and it is held for exactly the **remaining** window
of the *earliest* such overlap (so a third arrival doesn't reset the clock).

```go
type Stagger struct {
	window time.Duration
	clock  func() time.Time // injectable for tests
	mu     sync.Mutex
	// recently-admitted target sets: invocation_id → (targets set, admittedAt).
	// GC'd on a ticker and opportunistically in Try (entries older than window are dead).
	recent map[string]admitRecord
}
type admitRecord struct {
	targets    map[string]struct{}
	admittedAt time.Time
}

func (s *Stagger) Try(_ context.Context, req Request, snap RegistrySnapshot) (bool, time.Duration) {
	want := toSet(req.Targets) // normalize() applied per the rule above
	s.mu.Lock(); defer s.mu.Unlock()
	now := s.clock()
	var hold time.Duration // 0 = admit; >0 = remaining window of the EARLIEST overlap

	consider := func(targets map[string]struct{}, admittedAt time.Time) {
		age := now.Sub(admittedAt)
		if age >= s.window || age < 0 {
			return // window elapsed (or clock skew) → no longer blocks
		}
		if overlaps(want, targets) {
			remaining := s.window - age
			if remaining > hold {
				hold = remaining // we must wait for the LONGEST remaining among earliest overlaps
			}
		}
	}

	// 1. Our own recently-admitted records (opportunistically GC dead ones).
	for id, rec := range s.recent {
		if now.Sub(rec.admittedAt) >= s.window {
			delete(s.recent, id) // opportunistic GC
			continue
		}
		consider(rec.targets, rec.admittedAt)
	}
	// 2. E3 snapshot — covers passively-discovered (un-wrapped) builds and builds that
	//    survived a broker restart, which our `recent` map would otherwise miss.
	for _, b := range snap.Builds {
		consider(toSet(b.Targets), b.StartedAt)
	}
	if hold > 0 {
		return false, hold
	}
	return true, 0
}
func (s *Stagger) Acquire(req Request) {
	s.mu.Lock(); s.recent[req.InvocationID] = admitRecord{toSet(req.Targets), s.clock()}; s.mu.Unlock()
}
// Release keeps the record so the cache-warm window still applies after the (possibly very
// short) first build finishes; the GC ticker reclaims it once it ages past the window.
func (s *Stagger) Release(id string) { /* GC by ticker; see runGC */ }

// runGC drops records older than the window. Started alongside the load ticker so `recent`
// can never grow without bound (the bug a "keep until window GC with no GC" comment would hide).
func (s *Stagger) runGC(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every); defer t.Stop()
	for {
		select {
		case <-ctx.Done(): return
		case <-t.C:
			s.mu.Lock()
			now := s.clock()
			for id, rec := range s.recent {
				if now.Sub(rec.admittedAt) >= s.window { delete(s.recent, id) }
			}
			s.mu.Unlock()
		}
	}
}

// overlaps: any shared normalized target ⇒ likely shared actions. Coarse but effective.
func overlaps(a, b map[string]struct{}) bool {
	// iterate the smaller set for cheapness
	if len(b) < len(a) { a, b = b, a }
	for k := range a { if _, ok := b[k]; ok { return true } }
	return false
}
```

**Tuning / caveats:**
- `StaggerWindow` is a *delay*, not a *block*: after the window the second is admitted even if
  the first hasn't finished (we only need the first to have *executed and written* the early
  shared actions, not to have completed the whole build). Default 8s; expose as policy.
- **Earliest-overlap clock, not last:** holding for the *remaining* window of the matching
  record (computed from `admittedAt`, never reset) means a steady stream of overlapping requests
  does **not** indefinitely starve later ones — each waits at most one window past the build it
  overlaps, not one window past the most-recent arrival.
- **`recent` is GC'd** by `runGC` (ticker) *and* opportunistically inside `Try`. The earlier
  draft's "keep record until window GC; harmless" had no GC and would leak — fixed.
- **A request with empty targets** (e.g. `bazel build` with all targets in a `.bazelrc`
  `build:` config, or a `--target_pattern_file`) has `T(req)=∅`, so `overlaps` is always false →
  it is **never staggered**. Documented as a known false-negative (we cannot see those targets
  from the command line); the shared disk cache still helps it, just without the head start.
- Stagger is **subordinate to fairness**: it returns `retryAfter`, not a hard deny, so the
  FIFO head can still be the staggered request — `schedule()` arms a timer for `retry` and
  re-evaluates. To avoid head-of-line blocking the *non-overlapping* builds behind a staggered
  one, v1.1 may allow `schedule()` to admit a later non-overlapping waiter past a staggered head
  (bounded look-ahead) — **flagged as a follow-up, not in v1** to keep FIFO simple.
- Overlap is computed on **target patterns as opaque strings**, the cheap signal. Architecturally
  honest: §8.4 says "the *same* action"; pattern overlap is a strong proxy without action-graph
  analysis.

#### 2.3.6 Keep-servers-warm (§8.4 item 4)

Two parts, both implemented in E5:

1. **Wrapper-injected `--max_idle_secs`** (see §2.1 step 5): default 10800s (3h) so a
   worktree's Bazel server — and its in-memory analysis (Skyframe) cache — survives idle gaps
   between an agent's edit/build cycles instead of shutting down and cold-starting.
2. **Broker policy: never auto-`shutdown`.** The admission engine must *not* issue
   `bazel shutdown` as a resource-reclaim tactic (that would throw away the warm analysis
   cache — the one cache we cannot share, §8.1). Resource pressure is handled by *queuing new
   admissions* (token bucket), not by killing warm servers. (Killing a *runaway* build is E3's
   explicit `Kill`, a user action — different from idle reclaim.) This is encoded as an
   invariant + a test that asserts the engine issues no `shutdown` under load.

---

### 2.4 HTTP wiring (added to the E2 mux)

```go
func RegisterAdmissionRoutes(mux *http.ServeMux, e *Engine, auth Middleware) {
	mux.Handle("POST /admission",         auth(http.HandlerFunc(e.handleAdmit)))
	mux.Handle("POST /admission/release", auth(http.HandlerFunc(e.handleRelease)))
	mux.Handle("POST /pause",             auth(http.HandlerFunc(e.handlePause)))
	mux.Handle("POST /resume",            auth(http.HandlerFunc(e.handleResume)))
	mux.Handle("POST /drain",             auth(http.HandlerFunc(e.handleDrain)))
	mux.Handle("GET  /admission/status",  auth(http.HandlerFunc(e.handleStatus)))
}

func (e *Engine) handleAdmit(w http.ResponseWriter, r *http.Request) {
	var req Request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil { http.Error(w, "BAD", 400); return }
	switch e.Admit(r.Context(), req) {     // blocks up to PollSeconds
	case Allow: w.WriteHeader(200); io.WriteString(w, "ALLOW")
	case Queue: w.Header().Set("Retry-After", "1"); w.WriteHeader(202); io.WriteString(w, "QUEUE")
	case Deny:  w.WriteHeader(403); io.WriteString(w, "DENY")
	}
}
```

`/pause`, `/resume`, `/drain` flip `Policy.Paused` / `Policy.Draining` and call `schedule()`.
`/drain` immediately `DENY`s all currently-queued waiters and rejects new ones; in-flight
builds finish naturally (drain ≠ kill). `/admission/status` returns JSON
`{maxConcurrent, held, queued, paused, draining, cpu, ram}` for `brokerctl` / `/healthz`.

> ⚠️ **Interface reconciliation with E2 (must be resolved before T2 — see §4.1, flagged to
> escalate).** E2 §4.2 only **reserved** three of these routes (`POST /admission`, `POST /drain`,
> `POST /resume`) as `501` stubs. The other three this epic needs — **`POST /pause`**,
> **`POST /admission/release`**, **`GET /admission/status`** — were **not reserved in the E2
> frozen table**. E2 froze its API and said later epics "only swap the handler body — the
> path/method/auth contract is fixed", so **adding** routes is a contract change that needs an
> E2 amendment, not just an E5 handler. Two further concrete mismatches:
> 1. **`/admission` response body.** E2's reserved note says `/admission` "will return
>    `{"decision":"allow|queue|deny"}`" — a **JSON** body. E5 deliberately returns a **single
>    word + status code** (no JSON) because the bash wrapper has no `jq`. These are incompatible
>    wire contracts. **Recommendation:** amend E2 to the status-code-+-one-word form (E5's), since
>    the only consumer of `/admission` is the bash wrapper; keep the structured detail in
>    `X-Broker-*` headers. Escalate to the E2 owner.
> 2. **`/healthz` shape.** E2 already froze `HealthResponse` with a `queued` field (it's `0`
>    until E5). E5 must **populate** that existing field, not add a new one — earlier draft text
>    said "extend `/healthz` to add `queued`", which is wrong; corrected in §4.1.

---

## 3. Sequencing (ordered, checkpointed)

Each task is independently verifiable. Depends on E2 (broker + mux + auth + registry reader)
and E3 (registry snapshot with active builds' target sets + PID liveness).

- [ ] **T0 — E2 API amendment (cross-epic, blocks T2/T4).** Coordinate with the E2 owner to
  reserve `POST /admission/release`, `POST /pause`, `GET /admission/status` as `501` stubs and
  amend the `/admission` reserved-response note to the single-word body form (§4.1, §6). This is an
  E2-owned change; E5 cannot land its handlers against a frozen API without it. **Verify:** the
  three new routes return `501` (not `404`) from the E2 daemon before E5 fills them.

- [ ] **T1 — Engine skeleton + Gate interface + FIFO queue + slot-release state machine.**
  `internal/admission/`: `Engine`, `Gate`, `waiter` (with `wsQueued/wsAdmitted/wsDone` +
  `connected`), cap-1 buffered single-shot verdict channel, `Admit` (long-poll, fast-path peek,
  `attach`/`detach`), `schedule` (non-blocking `deliver`), `runGC` for queued waiters. Stub gate
  that always admits. **Verify (the load-bearing concurrency tests, `go test -race`):**
  (a) N concurrent `Admit` calls all return `Allow`;
  (b) **schedule() never deadlocks** when a handler has already returned `Queue` — admit a waiter
      whose handler timed out, assert the `ALLOW` is buffered and the *next* `Admit` for the same
      id drains it (returns `Allow`) without re-acquiring;
  (c) `ctx` cancel of a *queued* waiter drops it from the FIFO (no leak);
  (d) `ctx` cancel of an *admitted* waiter does **not** release its gate (slot stays held until
      explicit release).

- [ ] **T2 — Semaphore gate + HTTP `/admission` + `/admission/release`.** Wire into E2 mux (after
  T0). **Verify:** `curl` 3 admits with `MaxConcurrent=2` → two `200 ALLOW`, one `202 QUEUE` then
  re-polls; `POST /admission/release` one of the two → the queued third re-poll returns `200`.
  Assert releasing is idempotent (`204` on a second/unknown release). (`/verify` recipe §5.)

- [ ] **T3 — `tools/bazel` wrapper (bash) against `fake-bazel.sh`.** Implement §2.1; reference
  copy at repo `tools/bazel`. **Verify:** `BAZEL_REAL=testdata/fake-bazel.sh tools/bazel build //x`
  mints an id, POSTs, execs fake-bazel with injected flags; `BROKER_BYPASS=1` and `CI=1` skip
  the POST (assert via broker access log / a request counter). Recursion guard: run with
  `BAZEL_REAL` unset in a temp PATH containing only the wrapper → exits 127, not a fork bomb.

- [ ] **T4 — Pause / Resume / Drain endpoints.** **Verify:** `drain` → queued waiters get
  `403 DENY` and the wrapper exits 75 without execing; `resume` after `pause` releases held
  waiters in FIFO order.

- [ ] **T5 — LoadProbe (gopsutil) + TokenBucket gate.** 1s sampling ticker; CPU/RAM
  high-water gating. **Verify:** inject a fake `LoadProbe` returning `cpu=0.99` → admits are
  held even with semaphore slots free; drop to `0.10` → they admit. (Unit test with fake probe;
  manual smoke with real gopsutil printing samples.)

- [ ] **T6 — Stagger gate.** Overlap on target labels + E3 snapshot consultation.
  **Verify:** two admits for `//app:app` within the window → first `200` immediately, second
  delayed ~`StaggerWindow` then `200`; disjoint targets are not delayed. (Stagger demo §5.)

- [ ] **T7 — Keep-servers-warm.** Wrapper injects `--max_idle_secs`; engine invariant "no
  auto-shutdown". **Verify:** assert injected flag present in the exec'd argv (fake-bazel logs
  its args); engine test asserts no `shutdown` command is ever issued under simulated load.

- [ ] **T8 — Stale-slot reaper + `/admission/status` + `/healthz` extension.** Reaper frees
  slots whose `pid` is dead (reuses E3 PID liveness) on a 5s tick; `/healthz` now reports
  `{builds, queued}`. **Verify:** admit a build, `kill -9` the fake pid → within ~5s the slot
  frees and a waiter is admitted.

- [ ] **T9 — End-to-end with real bazel + 2 worktrees** (uses E0 synthetic workspace).
  **Verify:** copy wrapper into the workspace's `tools/bazel`, run the same target in two
  worktrees; observe stagger + a high cache-hit second build (cache% comes from E4 if present,
  else inspect Bazel's own summary). Confirms auto-propagation via the committed wrapper.

- [ ] **T10 — Docs + Make targets + CLAUDE.md recipe.** `make admission-verify` runs the
  fake-build matrix; CLAUDE.md "how to run & verify" for E5.

---

## 4. Interfaces & contracts

### 4.1 New broker API surface (extends the E2 table in architecture §6)

| Method | Body | Response | E2 status | Notes |
|---|---|---|---|---|
| `POST /admission` | `{invocation_id,worktree,pid,targets}` | `200 ALLOW` / `202 QUEUE` / `403 DENY` / 5xx | **reserved** (501) — but E2 spec'd a JSON body; **must amend** to single-word | long-poll (≤25s); single-word body, detail in `X-Broker-*` headers |
| `POST /admission/release` | `{invocation_id}` | `204` | **NOT reserved by E2 — new route, needs E2 amendment** | idempotent; primary slot-release path |
| `POST /pause` | — | `200` | **NOT reserved by E2 — new route, needs E2 amendment** | hold all admissions (no deny) |
| `POST /resume` | — | `200` | **reserved** (501) | release held waiters, FIFO |
| `POST /drain` | — | `200` | **reserved** (501) | deny queued + new; in-flight finish |
| `GET /admission/status` | — | `200` JSON `{maxConcurrent,held,queued,paused,draining,cpu,ram}` | **NOT reserved by E2 — new route, needs E2 amendment** | for brokerctl/UI |
| `GET /healthz` (E2-owned) | — | E2's `HealthResponse` | **owned by E2** | E5 **populates the existing `queued` field** (frozen in E2 §4.1); does **not** add a field |

All endpoints reuse E2's auth middleware (loopback + bearer token, D-stack-2). **Three of these
routes (`/admission/release`, `/pause`, `/admission/status`) are not in E2's frozen §4.2 table
and the `/admission` body shape conflicts with E2's reserved note — this requires an E2 contract
amendment, flagged to escalate in §6 and called out in §2.4.** The fix is small (E2 reserves
three more `501` stubs and changes the `/admission` reserved-response note), but it must be a
deliberate, owner-approved E2 change because E2 declared its API frozen.

> Note on `/pause` vs `/drain`/`/resume` semantics asymmetry: E2 reserved `/drain` and `/resume`
> but the natural pair for `/resume` is `/pause`, while the natural pair for `/drain` is an
> (un-drain). E5 maps `/resume` to clear **both** `Paused` and `Draining` for ergonomics; if the
> E2 amendment is undesirable, an alternative is to fold pause into a single
> `POST /admission/policy {paused,draining}` route (one new route instead of three) — surfaced
> as an option for the E2 owner.

### 4.2 Wrapper environment contract

| Env var | Default | Meaning |
|---|---|---|
| `BAZEL_REAL` | *(set by the bazel/bazelisk launcher)* | absolute path to real bazel; wrapper invokes **this**, never the bare name `bazel`/`bazelisk` (recursion). `exec` it on bypass paths; **run-and-wait** it on the gated path so the release trap fires (§2.1 step 4) |
| `BROKER_BYPASS` | `0` | `1` → skip admission, still inject flags? **No** — full bypass, straight exec |
| `BROKER_URL` | `http://127.0.0.1:8765` | broker base URL |
| `BROKER_TOKEN` | *(empty)* | bearer token (from E2 config) |
| `CI` | *(empty)* | if set → bypass (CI guard) |
| `BROKER_MAX_IDLE_SECS` | `10800` | keep-servers-warm idle timeout |
| `BROKER_CACHE_DIR` / `BROKER_REPO_CACHE` | `~/.cache/bazel-broker/…` | shared caches (E1) |
| `BROKER_EVENT_DIR` / `BROKER_PROFILE_DIR` | `~/.local/state/bazel-broker/…` | per-invocation BEP/profile (E4 consumes) |
| `ADMISSION_POLL_SECONDS` (server) / `ADMISSION_MAX_ATTEMPTS` (wrapper) | `25` / `240` | long-poll + ceiling |

**Note on `BROKER_BYPASS` and flag injection:** v1 treats bypass as a *full* passthrough
(`exec "$BAZEL_REAL" "$@"` with no injected flags) so it is a true escape hatch when the
broker/flag-injection itself is suspected. **Open question (§6):** should bypass still inject
the harmless E1 cache/profile flags (so cache sharing survives a bypass)? Surfaced, not
resolved.

### 4.3 Shared injected flag set (must stay in sync with **E1**)

E1 owns the canonical relocatability + cache flag definition (`.bazelrc` fragment + `setup.sh`).
The wrapper injects an **overlapping** set so that admission-gated builds get them even if a
worktree's `.bazelrc` is stale or dropped (`--ignore_all_rc_files`). Source of truth for the
*relocatability/cache* flags = **E1**; the wrapper's `INJECTED_BUILD` is a documented mirror.

**Precedence is NOT "harmless de-dupe" for every flag — be precise (this was hand-waved before):**

- **Idempotent (identical value) flags** — `--disk_cache`, `--repository_cache`,
  `--incompatible_strict_action_env`, `--experimental_output_paths=strip`, the prefix-maps: if E1
  and the wrapper set the *same* value, Bazel's last-wins is a genuine no-op. The wrapper and E1
  **must therefore agree byte-for-byte** (same cache dir, same `=.` prefix-map spelling). The plan
  ties this with a test (§5.2) that diffs the wrapper's injected relocatability flags against E1's
  `setup.sh` output. **If they diverge, cross-worktree cache hits silently regress** — this is the
  real risk, not a crash.
- **Path flags that DIFFER by design** — `--build_event_json_file` and `--profile`. E1 writes
  **relative, one-per-worktree** paths (`.bazel-broker/bep.json`, collision-safe via the
  per-output-base lock, E1 §2.4). The wrapper writes **absolute, per-invocation** paths
  (`$BROKER_EVENT_DIR/$INVOCATION_ID.bep.json`). These are **different values**, so last-wins means
  **the wrapper's per-invocation path overrides E1's** whenever the wrapper runs. That is the
  intended outcome (E4 prefers per-invocation history, E1 OD-1), but it is an **override, not a
  de-dupe**, and it means a build's BEP lands in a *different* place depending on whether it went
  through the wrapper. **Flag to E4:** E4's tailer must look in **both** locations (or the broker
  must tell it which, via the `/admission` record's `invocation_id`→path mapping). Surfaced, not
  resolved here.

Per-invocation `--build_event_json_file` / `--profile` paths are **owned here** (wrapper mints the
invocation id) and **consumed by E4**. Path-collision safety (architecture C3) is guaranteed for
the wrapper's paths because every path embeds the unique id.

### 4.4 How E5 uses E3

- **Registry snapshot** (`RegistryReader.Snapshot()`): the Stagger gate and TokenBucket read
  the set of currently-active builds and their target sets — including **passively discovered**
  (un-wrapped) builds — so stagger/headroom account for builds that never hit `/admission`.
- **PID liveness**: the stale-slot reaper asks E3 whether a slot's `pid` is still alive,
  freeing slots orphaned by a wrapper that died before `release` (crash, `kill -9`).
- E5 does **not** call E3's `Kill` — admission is block-based; killing is a separate user
  action (relevant to D3, §6).

---

## 5. Testing & verification

### 5.1 The headline `/verify` (epic "Done when")

Uses E0's `testdata/fake-bazel.sh` (sleeps `FAKE_BAZEL_DURATION`, honors flags, traps SIGINT).

```bash
# make admission-verify  (pseudocode of the recipe)
broker --config testdata/broker.test.toml &        # MaxConcurrent=2, PollSeconds=3 for speed
export BROKER_URL=http://127.0.0.1:8765 BAZEL_REAL="$PWD/testdata/fake-bazel.sh"
FAKE_BAZEL_DURATION=5 tools/bazel build //a &      # admitted (1/2)
FAKE_BAZEL_DURATION=5 tools/bazel build //b &      # admitted (2/2)
FAKE_BAZEL_DURATION=1 tools/bazel build //c &      # QUEUED
sleep 1
curl -s $BROKER_URL/admission/status | grep '"queued":1'   # assert c is queued
# wait for a or b to finish → c admits:
wait %4 || true                                            # c eventually exits 0
curl -s $BROKER_URL/admission/status | grep '"queued":0'
```

**Acceptance (from `02-epics.md` "Done when"):**
1. ✅ Start 3 fake builds, `max-concurrency=2` → third shows `queued`, then admits when one
   finishes. (Asserted via `/admission/status` `queued` count transitions 0→1→0 and `//c` exits 0.)
2. ✅ `BROKER_BYPASS=1 tools/bazel build //x` skips the gate — broker request counter unchanged,
   build runs immediately even when at capacity.
3. ✅ `curl -X POST $BROKER_URL/drain` → subsequent `tools/bazel build` exits 75 without
   execing fake-bazel (assert via fake-bazel's "started" marker file *not* appearing).

### 5.2 Targeted checks

- **CI guard:** `CI=1 tools/bazel build //x` → no `/admission` POST (request counter unchanged).
- **Recursion guard:** PATH containing only the wrapper, `BAZEL_REAL` unset → exits 127 in
  <1s, no runaway fork (assert process count stable).
- **Fail-open:** stop the broker, `tools/bazel build //x` → prints "broker unreachable …
  proceeding", execs fake-bazel, exit 0. **This is the critical safety test.**
- **Token bucket:** fake `LoadProbe` `cpu=0.99` with free semaphore slots → admit held;
  `cpu=0.10` → admit proceeds.
- **Stagger demo:** two `tools/bazel build //app:app` within `StaggerWindow` → first's
  fake-bazel "started" marker appears immediately, second's appears ~`StaggerWindow` later;
  disjoint targets (`//app` vs `//lib`) show no delay.
- **Keep-warm:** grep the fake-bazel arg log for `--max_idle_secs=10800`; engine test asserts
  zero `shutdown` invocations under simulated high load.
- **E1 flag-sync (prevents silent cache-hit regression):** in a temp worktree, run E1's
  `setup.sh`, then diff the wrapper's emitted relocatability flags (`--incompatible_strict_action_env`,
  `--experimental_output_paths=strip`, the five prefix-maps with `$WORKTREE`) against the managed
  block E1 wrote. Assert they match value-for-value (same `=.` target, same copt/objccopt/swiftcopt
  spread). A mismatch fails the test — this is the guard that keeps the "belt-and-suspenders"
  claim honest.
- **Release-on-exit (the bug this revision fixes):** gated `tools/bazel build //x` that runs to
  completion → assert `/admission/release` was POSTed *promptly* (within ~200ms of fake-bazel
  exit, i.e. before the 5s reaper would fire) via the broker access log. Repeat with Ctrl-C
  mid-build → assert the build received SIGINT (fake-bazel's cancel marker) **and** release was
  still POSTed. This proves the run-and-wait pattern (not `exec`) actually frees slots.
- **schedule() non-deadlock under timed-out poll:** unit test — admit a waiter, force its handler
  to return `Queue` (poll timeout) *before* `schedule()` admits it, then trigger `schedule()`;
  assert the engine does not block (no goroutine stuck sending on `w.ch`) and the buffered `ALLOW`
  is drained by a re-poll. `go test -race` with a watchdog timeout.
- **Admitted-but-disconnected reaping:** admit a build, drop the connection without releasing,
  `kill -9` the pid → reaper frees the slot within ~5s; a waiter behind it then admits.
- **Race:** `go test -race ./internal/admission/...` clean.

### 5.3 `/verify` recipe (CLAUDE.md, for `/verify` + `/run`)

```
make build                 # broker + reference tools/bazel present
make admission-verify      # runs 5.1 + 5.2 fail-open + bypass + drain, asserts, exits non-zero on fail
go test -race ./internal/admission/...
```

---

## 6. Risks, edge cases, open decisions

**Open decisions (surfaced, NOT resolved — per instructions):**

- **D3 — Admission model (block vs kill-based throttling vs hybrid).** This plan builds the
  **block-before-build** arm (the wrapper + engine). It is intentionally compatible with E3's
  kill-based throttling for a **hybrid**: block new builds *and* let the operator kill a
  runaway. The default posture (pure-block, pure-kill, or hybrid-on-by-default) is **left
  open**; the engine + reaper make hybrid cheap to enable. Recommendation to decide later
  based on observed oversubscription pain (architecture §12 D3).
- **Fail-open vs fail-closed when the broker is down.** This plan **defaults to fail-open**
  (a down/unreachable broker never blocks a build; only an explicit `403`/drain stops one).
  Rationale: the broker is a *throughput optimizer*, not a *correctness gate*; an agent or
  developer must always be able to build. The alternative (fail-closed: refuse to build if the
  gate is unreachable) would make the broker a single point of failure for all dev work.
  **Surfaced as the key open decision** — flip a `BROKER_FAIL_CLOSED=1` env/policy if a future
  use case demands hard admission. Default stays fail-open.
- **`BROKER_BYPASS` flag-injection behavior** (§4.2): does bypass still inject E1 cache/profile
  flags? v1 says no (full passthrough); open.
- **gopsutil vs cgo `host_statistics64`** for load (§2.3.4, D-stack): default gopsutil for CPU,
  **`sysctl kern.memorystatus_vm_pressure_level` for RAM** (gopsutil's `UsedPercent` is misleading
  on macOS), all swappable behind `LoadProbe`.

**Cross-epic contract change to escalate (NOT an E5 decision — needs E2 owner sign-off):**

- **E2 API amendment required.** E2 §4.2 froze its route table and reserved only `POST /admission`,
  `POST /drain`, `POST /resume`. E5 needs **three additional routes** (`POST /admission/release`,
  `POST /pause`, `GET /admission/status`) and a **change to the `/admission` reserved-response
  shape** (E2 spec'd JSON `{"decision":...}`; E5 requires a single-word body for the `jq`-free bash
  wrapper). E5 cannot unilaterally add frozen-API routes. **Action:** the E2 owner must either (a)
  reserve the three extra `501` stubs + amend the `/admission` note to the single-word form, or
  (b) collapse pause/resume/drain into one `POST /admission/policy` route (one new route).
  Recommended: (a). Blocks T2/T4. (See §2.4 and §4.1.)

**Edge cases & mitigations:**

- **Wrapper bypass via direct real-bazel path.** A user/agent who runs `$BAZEL_REAL` (or an
  absolute `/opt/homebrew/bin/bazel`) directly **skips the gate entirely**. Mitigation: this is
  *expected* (escape hatch by construction) and the broker still *sees* the build via E3 passive
  discovery (so stagger/headroom/trace still account for it) — admission just can't *block* it.
  Documented as a known limitation of any wrapper-based approach (architecture §7: only the
  pre-exec hook can block).
- **`--ignore_all_rc_files`.** If a caller passes this, the iOS `.bazelrc` (E1) flags vanish —
  but the **wrapper still injects** the cache/profile/relocatability flags on the command line,
  so they survive. This is a *reason* the wrapper mirrors E1's flags rather than relying solely
  on `.bazelrc`. (Caveat: if the caller *also* passes their own conflicting values, last-wins
  applies.)
- **Xcode-triggered builds.** Xcode/IDE integrations invoke bazel for `info`/`query`/`version`
  probes constantly; blocking these would hang the IDE. The wrapper's command allowlist (§2.1
  step 1) only gates `build/test/run/coverage/mobile-install/aquery/cquery`. An explicit
  `BROKER_BYPASS=1` in the IDE's bazel env is the documented escape if even those must not block.
- **Deadlock / broker down.** Covered by fail-open (above) + the wrapper's `ADMISSION_MAX_ATTEMPTS`
  ceiling: even a misbehaving broker that returns endless `202` eventually fails open after the
  ceiling. No path leaves a build blocked forever.
- **Orphaned slots (two sub-cases, both covered).** (1) A wrapper killed (`kill -9`) *after*
  `ALLOW` but before `release` leaks a held semaphore slot → the **stale-slot reaper** (T8) frees
  slots whose `pid` is dead (via E3 liveness) on a 5s tick. (2) A waiter that was **admitted while
  its long-poll connection was already gone** (admit raced a disconnect) holds gates with no client
  coming back to drain the buffered `ALLOW` — the *same* reaper catches it: its `pid` was POSTed at
  enqueue, so liveness reaps it. The slot-release design (§2.3) ensures connection loss alone never
  frees a slot and admit never blocks the engine, so these are the *only* leak paths and both fall
  to the reaper. **Verify (T8):** admit a build, `kill -9` its pid → slot frees within ~5s.
- **CI detection.** Relies on the standard `CI` env var (GitHub Actions, GitLab, CircleCI all set
  it). Documented; teams on exotic CI can also set `BROKER_BYPASS=1`. (Open: enumerate more CI
  vars — `GITHUB_ACTIONS`, `BUILDKITE` — if `CI` proves insufficient.)
- **Thundering-herd false negatives.** Stagger keys on exact target-label overlap; two builds of
  *different* labels that share many underlying actions won't be staggered (no overlap detected).
  Acceptable for v1 (the shared disk cache still helps the second, just without the head start);
  finer action-level overlap is a follow-up.
- **Long-poll connection limits.** Many simultaneously-blocked wrappers hold open connections;
  the broker's HTTP server must allow enough concurrent handlers (Go's `net/http` spawns a
  goroutine per connection with no built-in cap, so low-tens of held connections is a non-issue at
  single-developer scale). Each held connection costs ~1 goroutine + the cap-1 waiter channel;
  there is **no per-connection slot cost** (slots are accounted independently of connections per
  §2.3), so a flood of polling wrappers cannot exhaust admission slots — only goroutines, bounded
  by `ADMISSION_MAX_ATTEMPTS` re-polls × wrappers. Documented, not a hard limit at this scale.
- **bash 3.2 / macefault shell.** Wrapper avoids bash-4 features (tested under `/bin/bash`).

---

## 7. Effort & internal ordering

**Critical path (ship value early):** T1 → T2 → T3 → **(working block-before-build with a
global semaphore — this satisfies the epic's headline "Done when")** → T4 (drain) →
then T5/T6/T7 layer in parallel-ish.

| Task | Est. | Notes / parallelism |
|---|---|---|
| T0 E2 API amendment (cross-epic) | 0.25 d | coordinate w/ E2 owner; 3 stubs + `/admission` body note |
| T1 Engine skeleton + queue + long-poll + slot state machine | 1.5 d | pure Go + `-race`; the deadlock/release tests are the load-bearing ones |
| T2 Semaphore + `/admission` + `/release` | 0.5 d | hits the epic's core "Done when" |
| T3 `tools/bazel` wrapper + fake-bazel | 1.0 d | bash 3.2 care; recursion + bypass tests |
| T4 Pause/Resume/Drain | 0.5 d | small once engine exists |
| T5 LoadProbe (gopsutil) + TokenBucket | 1.0 d | + 0.5 d if cgo fallback also built |
| T6 Stagger gate | 1.0 d | overlap + E3 snapshot + FIFO interaction |
| T7 Keep-servers-warm | 0.25 d | flag inject + no-shutdown invariant + test |
| T8 Reaper + status + healthz | 0.5 d | reuses E3 liveness |
| T9 Real-bazel 2-worktree e2e | 0.5 d | needs E0 workspace + ideally E4 for cache% |
| T10 Docs + Make + CLAUDE.md | 0.5 d | |

**Rough total:** ~7–7.5 days. **Minimum shippable (epic "Done when"):** T1+T2+T3+T4 ≈ 3 days
(semaphore-based block + drain + wrapper). Token bucket (T5) and stagger (T6) are the
higher-sophistication layers that deliver the §8.4 cache + machine-health wins and can land
incrementally behind the already-working semaphore.

**Dependency reminder:** E5 is last in the core chain (E0→E2→E3→E5). **T0 (E2 API amendment)
gates T2/T4.** T1–T4 need E2's mux/auth and a stub registry; T6/T8 need E3's real snapshot + PID
liveness; T9 needs E0's synthetic workspace and is richer with E4's cache metrics.

---

## Staff Engineer Review

*Reviewer pass on Draft v1. Scope: wrapper correctness, admission long-poll + slot-release
semantics, stagger algorithm, macOS load probe, and E2/E1 interface alignment. D3 (block vs kill
vs hybrid) and fail-open-vs-closed were intentionally left unresolved per review constraints —
framing sharpened, not decided.*

### (a) Verdict

**Approve with required changes — now incorporated.** The epic's instincts were right (long-poll
over busy-wait, single-word body for `jq`-free bash, semaphore-first shipping order, stagger as a
soft delay), but Draft v1 shipped **two latent correctness bugs that would have failed in
production** and several interface claims that were too loose. Both bugs are now fixed in-place.
With the revisions, the plan is buildable and the "Done when" is reachable via T1–T2 (+T0). The
single hard blocker is the cross-epic E2 API amendment (T0), which is small but must be owner-approved.

### (b) Top findings

1. **[Critical] `exec` on the gated path silently skipped `/admission/release`.** The wrapper
   ended with `exec "$BAZEL_REAL"`, which replaces the shell image — so the `trap release EXIT`
   could never fire. Every gated build would have held its slot until the 5s PID reaper, serializing
   throughput far below `MaxConcurrent`. **Fixed:** the gated path now *runs* bazel as a child,
   forwards `INT`/`TERM`, `wait`s, releases explicitly + via the EXIT trap, and propagates bazel's
   exit code. Bypass paths keep the clean `exec`.

2. **[Critical] `schedule()` could deadlock the engine holding `e.mu`.** It did `w.ch <- Allow` on
   an unbuffered channel; once a handler returned `Queue`/disconnected and stopped selecting, that
   send blocks forever **while holding the engine mutex** → total admission stall. **Fixed:** waiters
   now carry a cap-1 buffered single-shot channel and explicit `wsQueued/wsAdmitted/wsDone` state;
   `schedule()` does a non-blocking `deliver`; the verdict is drained by whichever re-poll re-attaches.

3. **[Critical] Slot-vs-connection coupling was undefined.** It was unclear what freed a slot when a
   client timed out or disconnected mid-admission. **Fixed:** stated and enforced the invariant that
   *connection close never frees a slot* — release is exclusively `/admission/release` or the PID
   reaper — and that gates are acquired exactly once, with the admitted waiter retained in `byID` so
   re-polls re-attach instead of double-acquiring. Added a full waiter-lifecycle table covering every
   release path including admitted-but-orphaned.

4. **[High] Recursion guard was insufficient.** The `BAZEL_REAL`-unset fallback could resolve a
   `bazel` that is itself a launcher and re-exec `tools/bazel` → fork bomb; the `$p = $self`
   comparison also missed symlinked-worktree path aliasing. **Fixed:** added a `BB_WRAPPER_REENTRY`
   env tripwire checked on entry, `pwd -P` on both sides of the self-comparison, and a recursion
   test that asserts a stable process count. Also corrected the `tools/bazel`/`BAZEL_REAL` contract
   prose (it's the *launcher*, not a shell, that re-execs us).

5. **[High] E2 interface mismatch.** E5 needs three routes E2 never reserved
   (`/admission/release`, `/pause`, `/admission/status`) and a `/admission` body shape (single word)
   that contradicts E2's reserved JSON-`{decision}` note. E2 declared its API frozen, so this needs
   an amendment, not a silent handler swap. **Fixed:** flagged throughout (§2.4, §4.1, §6) and made
   T0 a gating task; offered a one-route `/admission/policy` alternative for the E2 owner.

6. **[Med] macOS RAM probe was misleading.** gopsutil `mem.UsedPercent` reads ~80–90% on a healthy
   idle Mac (inactive/compressed pages), which would refuse admission constantly. **Fixed:** RAM now
   gates on `sysctl kern.memorystatus_vm_pressure_level` (the OS's own pressure signal, no cgo);
   `RAMHighWater` became `RAMPressureMax`. CPU via gopsutil retained, with the first-sample priming
   gotcha called out.

7. **[Med] Stagger under-specified + memory leak.** "overlap" wasn't defined, the `recent` map had
   no GC ("keep until window GC" with no GC), and a stream of overlapping builds could starve later
   ones. **Fixed:** precise normalized-string overlap definition, earliest-overlap (non-resetting)
   hold clock, a real `runGC` ticker + opportunistic GC, and an explicit empty-targets false-negative note.

8. **[Med] E1 flag-sync was hand-waved as "harmless de-dupe".** The per-worktree prefix-maps were
   missing from the wrapper (it *can* compute them from `$WORKTREE`), `--generate_json_trace_profile`
   was absent, and BEP/profile path *override* (not de-dupe) vs E1 was unacknowledged. **Fixed:**
   added the prefix-maps + profile-enable flag, separated idempotent flags from design-divergent path
   flags, and flagged the E4 "look in both BEP locations" consequence.

### (c) What I changed (all in-place, 7-section structure preserved)

- **§2.0:** corrected the `tools/bazel`/`BAZEL_REAL` launcher contract; added the slot-vs-connection
  invariant box; reconciled the long-poll/release prose with the new state machine.
- **§2.1 (wrapper):** `BB_WRAPPER_REENTRY` tripwire + `pwd -P` self-compare; `--` end-of-flags
  handling in the arg split; bash-3.2 `set -u` empty-array guards; **run-and-wait instead of `exec`
  on the gated path** with signal forwarding; added prefix-maps + `--generate_json_trace_profile`;
  safe-direction note on command-verb misdetection.
- **§2.2:** rewrote `/admission/release` and long-poll semantics around the state machine; added the
  admit-raced-timeout case.
- **§2.3:** new `waiter` state machine (cap-1 buffered single-shot channel), non-blocking `deliver`,
  `attach`/`detach` with queued-only GC, corrected `schedule()`; added the waiter-lifecycle/
  release-path table and the "why buffered" rationale; macOS pressure-based RAM probe; anti-stampede
  CPU debit; **rewrote Stagger** (overlap definition, earliest-overlap clock, `runGC`).
- **§2.4:** flagged the E2 route/body mismatch with a recommended fix.
- **§3:** added **T0** (E2 amendment) as a gating task; expanded T1 verify to the deadlock/release/
  ctx-cancel matrix; tightened T2.
- **§4.1/§4.3:** marked each route reserved-vs-new; corrected the `/healthz` "populate not add"
  point; replaced the "de-dupe" claim with precise idempotent-vs-override flag analysis + the E4
  BEP-location consequence.
- **§5:** added release-on-exit, schedule-non-deadlock, admitted-but-disconnected reaping, and E1
  flag-sync diff tests.
- **§6:** added the E2-amendment escalation; refined orphaned-slot (two sub-cases), long-poll
  connection-limit, and load-probe open decisions.
- **§7:** added T0 and re-sized T1 to reflect the concurrency work.

### (d) Decisions / risks to escalate

- **[Escalate — owner: E2] API amendment (T0, blocking).** Reserve `/admission/release`, `/pause`,
  `/admission/status`; change `/admission` reserved response to single-word body. Or adopt the
  single `/admission/policy` route alternative. Nothing in E5 can land cleanly until this is decided.
- **[Escalate — D3, intentionally unresolved] block vs kill vs hybrid.** Framing improved; the
  engine + reaper keep hybrid cheap. Decision deferred to observed oversubscription pain (arch §12).
- **[Escalate — intentionally unresolved] fail-open vs fail-closed.** Default stays fail-open with a
  `BROKER_FAIL_CLOSED` switch; surfaced as the key safety decision, not decided here.
- **[Escalate — owner: E4] BEP/profile path divergence.** Wrapper's per-invocation absolute paths
  *override* E1's relative per-worktree paths. E4's tailer must handle both locations (or consume the
  broker's `invocation_id`→path mapping). Decide where E4 looks.
- **[Risk — owner: E1/E5 jointly] relocatability flag drift.** Wrapper-injected prefix-maps must stay
  byte-identical to E1's `setup.sh` output or cross-worktree cache hits silently regress. Guarded by
  the §5.2 flag-sync test; keep that test green as E1 evolves.
- **[Risk — verify empirically] stagger `StaggerWindow` default (8s) and token-bucket `rampPerBuild`
  (0.10) and the macOS pressure threshold** are guesses; tune against real iOS builds in T5/T6/T9.
- **[Open — D-stack] gopsutil(CPU)+sysctl(RAM) vs full cgo `host_statistics64`.** Default keeps the
  no-cgo path; cgo fallback stays behind the `LoadProbe` interface.
