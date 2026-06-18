#!/usr/bin/env bash
# deploy/install.sh — install or uninstall the bazel-broker LaunchAgent (per-user).
#
#   deploy/install.sh install [binary_path]   # default: $(go env GOPATH)/bin/broker or ./bin/broker
#   deploy/install.sh uninstall
#
# Uses modern launchctl bootstrap/bootout (gui/$(id -u)). The agent runs as the
# developer's user so it can read their worktrees.
set -euo pipefail

LABEL="com.bazelbroker.broker"
HOME_DIR="$HOME"
CONFIG="$HOME_DIR/.config/bazel-broker/config.json"
PLIST_SRC="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/com.bazelbroker.broker.plist"
PLIST_DST="$HOME_DIR/Library/LaunchAgents/$LABEL.plist"
STATE_DIR="$HOME_DIR/.local/state/bazel-broker"

cmd="${1:-install}"

case "$cmd" in
  install)
    binary="${2:-}"
    if [[ -z "$binary" ]]; then
      if [[ -x "./bin/broker" ]]; then
        binary="$(cd "$(dirname ./bin/broker)" && pwd)/broker"
      elif [[ -x "$(go env GOPATH 2>/dev/null)/bin/broker" ]]; then
        binary="$(go env GOPATH)/bin/broker"
      else
        echo "error: broker binary not found; pass it: install.sh install /path/to/broker" >&2
        exit 1
      fi
    fi
    mkdir -p "$STATE_DIR" "$(dirname "$PLIST_DST")" "$(dirname "$CONFIG")"

    # Substitute the template placeholders.
    sed -e "s#__BINARY__#$binary#g" \
        -e "s#__CONFIG__#$CONFIG#g" \
        -e "s#__HOME__#$HOME_DIR#g" \
        "$PLIST_SRC" > "$PLIST_DST"

    # Re-bootstrap idempotently.
    launchctl bootout "gui/$(id -u)/$LABEL" 2>/dev/null || true
    launchctl bootstrap "gui/$(id -u)" "$PLIST_DST"
    echo "installed $LABEL -> $binary"
    echo "  config: $CONFIG"
    echo "  check:  launchctl print gui/$(id -u)/$LABEL | grep -i state"
    ;;

  uninstall)
    launchctl bootout "gui/$(id -u)/$LABEL" 2>/dev/null || true
    rm -f "$PLIST_DST"
    echo "uninstalled $LABEL"
    ;;

  *)
    echo "usage: install.sh [install [binary]|uninstall]" >&2
    exit 2
    ;;
esac
