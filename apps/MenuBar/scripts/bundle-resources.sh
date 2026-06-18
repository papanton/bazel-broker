#!/usr/bin/env bash
# apps/MenuBar/scripts/bundle-resources.sh
#
# Xcode pre-build "run script" phase: build the broker daemon at the repo root and
# copy it together with the deploy/cache-config helper scripts into the app bundle's
# Resources/ directory, so the produced .app is a self-contained single entry point.
#
# These copies are BUILD ARTIFACTS — they are NOT committed (the originals live at the
# repo root). Resolve the repo root relative to $SRCROOT (apps/MenuBar -> ../..).
#
# Run script phases run with the build env: $SRCROOT (apps/MenuBar) and, when copying
# into the bundle, $TARGET_BUILD_DIR/$UNLOCALIZED_RESOURCES_FOLDER_PATH. When invoked
# manually (no $TARGET_BUILD_DIR), it falls back to staging under build/Resources for
# inspection.
set -euo pipefail

SRCROOT="${SRCROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
REPO_ROOT="$(cd "$SRCROOT/../.." && pwd)"

if [[ -n "${TARGET_BUILD_DIR:-}" && -n "${UNLOCALIZED_RESOURCES_FOLDER_PATH:-}" ]]; then
  RES_DIR="$TARGET_BUILD_DIR/$UNLOCALIZED_RESOURCES_FOLDER_PATH"
else
  RES_DIR="$SRCROOT/build/Resources"
fi

echo "bundle-resources: repo=$REPO_ROOT -> $RES_DIR"
mkdir -p "$RES_DIR"

# 1. Provide the broker daemon at $RES_DIR/broker.
# Xcode's PhaseScriptExecution runs with a minimal PATH (no Homebrew), so a bare
# `go` is not found when the build is launched from Finder/Xcode/make. Prefer an
# already-built bin/broker (from `make build`); otherwise build it, extending PATH
# to the usual Go install locations.
BROKER_OUT="$RES_DIR/broker"
PREBUILT="$REPO_ROOT/bin/broker"
if [[ -x "$PREBUILT" ]]; then
  echo "bundle-resources: using prebuilt broker -> $PREBUILT"
  install -m 0755 "$PREBUILT" "$BROKER_OUT"
else
  export PATH="/opt/homebrew/bin:/usr/local/bin:$HOME/go/bin:$PATH"
  GO_BIN="$(command -v go || true)"
  if [[ -z "$GO_BIN" ]]; then
    echo "bundle-resources: error: no $REPO_ROOT/bin/broker and 'go' not found on PATH." >&2
    echo "  Run 'make build' first, or install Go." >&2
    exit 1
  fi
  echo "bundle-resources: building broker with $GO_BIN -> $BROKER_OUT"
  ( cd "$REPO_ROOT" && "$GO_BIN" build -o "$BROKER_OUT" ./cmd/broker )
fi
chmod +x "$BROKER_OUT"

# 2. Copy the deploy + cache-config helpers verbatim.
copy() {  # src dst
  install -m "$1" "$REPO_ROOT/$2" "$RES_DIR/$(basename "$2")"
  echo "bundle-resources: + $2"
}
copy 0755 deploy/install.sh
copy 0644 deploy/com.bazelbroker.broker.plist
copy 0755 cache-config/setup.sh
copy 0644 cache-config/bazelrc.fragment
copy 0755 tools/bazel

echo "bundle-resources: done"
