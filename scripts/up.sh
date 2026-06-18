#!/usr/bin/env bash
# Run the broker daemon from source (dev convenience). Builds, starts the daemon,
# waits for /healthz, then blocks; Ctrl-C stops it. The menu-bar app is the main
# UX; this is for running the daemon directly during development.
#
# Env override: BAZEL_BROKER_CONFIG (config file path).
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

echo "==> broker ready on 127.0.0.1:$PORT"
echo "    cli:      bin/brokerctl ls   (watch | kill <id> | drain | profile <id>)"
echo "    menu-bar: open apps/MenuBar (the main UX) — it talks to this daemon"
echo "==> Ctrl-C to stop"
wait "$BROKER_PID"
