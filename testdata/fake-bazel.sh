#!/usr/bin/env bash
# testdata/fake-bazel.sh — a fake `bazel` (BAZEL_REAL stub) for fast, headless /verify.
#
# Emulates just enough of Bazel for the broker's verify paths:
#   * parses --build_event_json_file=PATH and --invocation_id=ID
#   * emits newline-delimited BEP JSON events (BuildStarted, Progress, BuildMetrics, BuildFinished)
#   * stays alive for FAKE_BAZEL_DURATION seconds (default 1), so it can be discovered & killed
#   * traps SIGTERM *and* SIGINT, writes a failed BuildFinished, and exits with cancel code 8
#
# CANCEL SIGNAL: both SIGTERM and SIGINT trigger a graceful cancel and exit 8, in every config
# INCLUDING when the script is `&`-launched. bash 3.2 normally sets SIGINT to SIG_IGN for
# backgrounded processes (so a `trap ... INT` silently never installs); we work around that by
# re-execing once through perl to reset SIGINT to its default disposition (see below). Tests may
# assert the cancel via SIGTERM (always reliable) or SIGINT (now equally reliable). See E0 §2.6.
#
# Env knobs:
#   FAKE_BAZEL_DURATION   build wall time in seconds (fractional ok). Default: 1
#   FAKE_BAZEL_CACHE_HITS / FAKE_BAZEL_CACHE_MISSES   numbers reported in BuildMetrics. Default 7 / 3
#   FAKE_BAZEL_EXIT       success exit code override. Default: 0
#
# Exit codes:  0 success · 8 interrupted (SIGTERM/SIGINT) · 2 usage error
set -u

# ---- robustly un-ignore SIGINT --------------------------------------------------------------
# bash 3.2 sets SIGINT (and SIGQUIT) to SIG_IGN for `&`-launched processes, and an inherited
# SIG_IGN CANNOT be un-ignored from within the same process via `trap` (verified: `trap -p INT`
# stays empty and `kill -INT` is a no-op). To make the SIGINT cancel contract robust even when
# backgrounded, re-exec ourselves once through perl, which resets SIGINT to its default
# disposition first; the fresh bash process can then install the INT trap normally. Guarded by an
# env flag so we re-exec at most once, and gated on perl being present (always true on macOS base);
# if perl is missing we fall through and SIGTERM still works in every config.
if [[ -z "${FAKE_BAZEL_SIGRESET:-}" ]] && command -v perl >/dev/null 2>&1; then
  export FAKE_BAZEL_SIGRESET=1
  exec perl -e '$SIG{INT}="DEFAULT"; exec @ARGV or die "exec: $!"' "$0" "$@"
fi

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
    build|test|run|query|info|clean|shutdown) : ;;       # ignore the command verb
    -*)                        : ;;                      # ignore other flags
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

# Render the targets as a plain space-separated list (no quotes), safe to embed inside a JSON
# string value. Real Bazel's progress stderr is free text, so this stays faithful and valid.
# "${TARGETS[*]:-}" joins on the first IFS char (a space) and is empty-array-safe under set -u.
targets_plain() { printf '%s' "${TARGETS[*]:-}"; }

# ---- SIGTERM/SIGINT handler: graceful cancel ------------------------------------------------
on_signal() {
  emit "{\"id\":{\"buildFinished\":{}},\"buildFinished\":{\"overallSuccess\":false,\"exitCode\":{\"name\":\"INTERRUPTED\",\"code\":${CANCEL_CODE}},\"finishTime\":\"$(now_ts)\"}}"
  exit "$CANCEL_CODE"
}
# Trap both. SIGINT was reset to its default disposition by the perl re-exec at the top, so this
# INT trap installs and fires even for a backgrounded process (verified via `trap -p INT`).
trap on_signal TERM
trap on_signal INT

# ---- prepare BEP file -----------------------------------------------------------------------
if [[ -n "$BEP_FILE" ]]; then
  mkdir -p "$(dirname "$BEP_FILE")" 2>/dev/null || true
  : > "$BEP_FILE"   # truncate/create
fi

# ---- emit started + progress ----------------------------------------------------------------
emit "{\"id\":{\"started\":{}},\"started\":{\"uuid\":\"${INVOCATION_ID}\",\"startTimeMillis\":\"$(date +%s)000\",\"buildToolVersion\":\"fake-8.0.0\",\"command\":\"build\"}}"
emit "{\"id\":{\"progress\":{}},\"progress\":{\"stderr\":\"INFO: Analyzed targets: $(targets_plain).\\n\"}}"

# ---- stay alive (killable) for DURATION, in small sleep slices so traps fire promptly --------
# Sleep in <=0.2s slices via `sleep & wait $!` so a pending TERM/INT trap runs near-immediately
# (each `wait` returns when the backgrounded sleep is signalled). Keeps the <1s kill budget E3 needs.
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
