#!/usr/bin/env bash
# One command to run the whole thing: build, start the daemon, open the dashboard
# pre-authenticated. Ctrl-C stops the daemon. No manual token copy/paste.
#
# Env overrides: BAZEL_BROKER_CONFIG (config file path), BROKER_NO_OPEN=1 (don't
# open a browser — just print the URL).
set -euo pipefail
cd "$(dirname "$0")/.."

echo "==> building"
make build >/dev/null

CONFIG="${BAZEL_BROKER_CONFIG:-$HOME/.config/bazel-broker/config.json}"

echo "==> starting broker"
bin/broker ${BAZEL_BROKER_CONFIG:+--config "$BAZEL_BROKER_CONFIG"} &
BROKER_PID=$!
trap 'echo; echo "==> stopping broker"; kill "$BROKER_PID" 2>/dev/null || true; wait "$BROKER_PID" 2>/dev/null || true' INT TERM EXIT

# Wait for the daemon to write its config + answer /healthz.
PORT=8765
for _ in $(seq 1 50); do
  if [ -f "$CONFIG" ]; then
    PORT=$(/usr/bin/python3 -c "import json;print(json.load(open('$CONFIG')).get('port',8765))" 2>/dev/null || echo 8765)
    curl -fsS "127.0.0.1:$PORT/healthz" >/dev/null 2>&1 && break
  fi
  sleep 0.2
done

TOKEN=$(/usr/bin/python3 -c "import json;print(json.load(open('$CONFIG'))['token'])" 2>/dev/null || true)
URL="http://127.0.0.1:$PORT/"
[ -n "$TOKEN" ] && URL="http://127.0.0.1:$PORT/#token=$TOKEN"

echo "==> broker ready on 127.0.0.1:$PORT"
echo "    dashboard: $URL"
echo "    cli:       bin/brokerctl ls   (watch | kill <id> | drain | profile <id>)"
if [ -z "${BROKER_NO_OPEN:-}" ] && command -v open >/dev/null 2>&1; then
  open "$URL"
fi
echo "==> Ctrl-C to stop"
wait "$BROKER_PID"
