#!/usr/bin/env bash
# scripts/smoke.sh — the E2 "Done when" manual flow, automated and isolated.
# Starts the broker on an isolated config FILE + a free port, then asserts:
#   /healthz (no auth) -> {builds:0,...}
#   register -> /builds shows it running
#   /builds without token -> 401
#   deregister exit 0 -> finished
# Never touches the developer's real ~/.config or ~/.local/state.
set -uo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BROKER="$ROOT/bin/broker"
fail=0
say() { printf '%-46s %s\n' "$1" "$2"; }

TMP="$(mktemp -d)"
CFG="$TMP/config.json"
PORT=18765
# Pre-seed config so we control port + token deterministically.
TOKEN="smoketoken$$"
cat > "$CFG" <<EOF
{"host":"127.0.0.1","port":$PORT,"token":"$TOKEN","db_path":"$TMP/broker.db","log_path":"$TMP/broker.log"}
EOF
chmod 600 "$CFG"

BAZEL_BROKER_CONFIG="$CFG" "$BROKER" --config "$CFG" >/dev/null 2>&1 &
bpid=$!
trap 'kill -TERM "$bpid" 2>/dev/null; wait "$bpid" 2>/dev/null; rm -rf "$TMP"' EXIT

# wait for listen
for _ in $(seq 1 30); do
  curl -s -o /dev/null "http://127.0.0.1:$PORT/healthz" && break
  sleep 0.1
done

base="http://127.0.0.1:$PORT"
auth=(-H "Authorization: Bearer $TOKEN")

# 1. healthz no-auth
h=$(curl -s "$base/healthz")
if echo "$h" | grep -q '"builds":0' && echo "$h" | grep -q '"queued":0'; then
  say "healthz {builds:0,queued:0}" PASS
else
  say "healthz {builds:0,queued:0}" "FAIL ($h)"; fail=1
fi

# 2. unauth /builds -> 401
code=$(curl -s -o /dev/null -w '%{http_code}' "$base/builds")
[[ "$code" == "401" ]] && say "unauth /builds -> 401" PASS || { say "unauth /builds -> 401" "FAIL ($code)"; fail=1; }

# 3. register
curl -s "${auth[@]}" -X POST "$base/register" \
  -d '{"invocation_id":"smoke1","worktree":"/wt/a","targets":["//app:App"],"pid":4242}' >/dev/null

# 4. /builds shows running
st=$(curl -s "${auth[@]}" "$base/builds")
if echo "$st" | grep -q '"invocation_id":"smoke1"' && echo "$st" | grep -q '"state":"running"'; then
  say "register -> /builds running" PASS
else
  say "register -> /builds running" "FAIL ($st)"; fail=1
fi

# 5. deregister exit 0 -> finished
dr=$(curl -s "${auth[@]}" -X POST "$base/deregister" -d '{"invocation_id":"smoke1","exit_code":0}')
echo "$dr" | grep -q '"state":"finished"' && say "deregister exit0 -> finished" PASS || { say "deregister exit0 -> finished" "FAIL ($dr)"; fail=1; }

if [[ $fail -eq 0 ]]; then echo "SMOKE: PASS"; exit 0; else echo "SMOKE: FAIL"; exit 1; fi
