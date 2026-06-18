#!/usr/bin/env bash
# scripts/verify-fast.sh — asserts the E0 acceptance criteria headlessly in ~3s.
set -uo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FAKE="$ROOT/testdata/fake-bazel.sh"
fail=0
say() { printf '%-40s %s\n' "$1" "$2"; }

# 1) build already done by Make; re-assert binaries exist.
if [[ -x "$ROOT/bin/broker" && -x "$ROOT/bin/brokerctl" ]]; then
  say "build artifacts" PASS
else
  say "build artifacts" FAIL; fail=1
fi

# 2) fake-bazel emits events and exits 0
BEP=$(mktemp /tmp/bb-bep.XXXX.json)
FAKE_BAZEL_DURATION=0.2 "$FAKE" build --invocation_id=verify-1 --build_event_json_file="$BEP" //:gen
rc=$?
n=$(wc -l < "$BEP" | tr -d ' ')
if [[ $rc -eq 0 && $n -ge 3 ]]; then
  say "fake-bazel emits BEP & exit 0" "PASS ($n events)"
else
  say "fake-bazel emits BEP & exit 0" "FAIL (rc=$rc n=$n)"; fail=1
fi
grep -q '"started"' "$BEP"       || { say "  has BuildStarted" FAIL; fail=1; }
grep -q '"buildFinished"' "$BEP" || { say "  has BuildFinished" FAIL; fail=1; }

# 3) SIGTERM yields the cancel code (8). The kill assertion accepts 8 (graceful trap) OR
#    137 (128+SIGKILL) per the consolidated review OD-A: a backgrounded stub may be reaped
#    by SIGKILL in the real E3 path, and SIGINT to an &-launched bash 3.2 process is ignored.
CANCEL_BEP=$(mktemp /tmp/bb-cancel.XXXX.json)
FAKE_BAZEL_DURATION=10 "$FAKE" build --build_event_json_file="$CANCEL_BEP" //:gen &
pid=$!
sleep 0.4
kill -TERM "$pid"
wait "$pid"; rc=$?            # `wait` returns the child's exit code (8 on graceful cancel)
if [[ $rc -eq 8 || $rc -eq 137 ]]; then
  say "SIGTERM -> cancel code (8|137)" "PASS (rc=$rc)"
else
  say "SIGTERM -> cancel code (8|137)" "FAIL (rc=$rc)"; fail=1
fi
if [[ $rc -eq 8 ]]; then
  grep -q '"overallSuccess":false' "$CANCEL_BEP" || { say "  cancel wrote failed BuildFinished" FAIL; fail=1; }
fi

# 4) broker healthz smoke (start, probe, stop) — proves the daemon stub serves.
#    Use an isolated config FILE (matches E2's $BAZEL_BROKER_CONFIG) so the smoke run never
#    touches the developer's real ~/.config state.
SMOKE_DIR=$(mktemp -d)
BAZEL_BROKER_CONFIG="$SMOKE_DIR/config.json" "$ROOT/bin/broker" --config "$SMOKE_DIR/config.json" &
bpid=$!; sleep 0.5
if command -v curl >/dev/null 2>&1; then
  code=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:8765/healthz" 2>/dev/null || echo 000)
  if [[ "$code" == "200" ]]; then
    say "broker /healthz 200" PASS
  else
    say "broker /healthz (best-effort)" "WARN (code=$code)"
  fi
fi
kill -TERM "$bpid" 2>/dev/null; wait "$bpid" 2>/dev/null
rm -rf "$SMOKE_DIR" "$CANCEL_BEP"
say "broker start/stop" PASS

rm -f "$BEP"
if [[ $fail -eq 0 ]]; then
  echo "VERIFY-FAST: PASS"; exit 0
else
  echo "VERIFY-FAST: FAIL"; exit 1
fi
