#!/usr/bin/env bash
# loadtest.sh — fire N concurrent, deterministically-long Bazel builds across git
# worktrees so the broker (menu-bar / dashboard / brokerctl) can be exercised:
# live trace, kill, admission queueing, oversubscription.
#
# Uses the //:slow genrule in testdata/workspace (a controllable `sleep`), so the
# duration is exact and there's no compile/cache flakiness.
#
#   scripts/loadtest.sh                 # 4 worktrees, 120s each
#   scripts/loadtest.sh -n 6 -s 300     # 6 worktrees, 5 min each
#   scripts/loadtest.sh --down          # tear everything down
#
# Worktrees + a fresh standalone git repo are created under ~/Developer/bb-loadtest
# (so each worktree's git root is its own bazel workspace -> correct broker names).
set -u
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SRC="$REPO/testdata/workspace"
BASE="$HOME/Developer/bb-loadtest"
BZL="$(command -v bazelisk || command -v bazel)"
N=4
SECS=120

while [ $# -gt 0 ]; do
  case "$1" in
    -n) N="$2"; shift 2 ;;
    -s) SECS="$2"; shift 2 ;;
    --down)
      echo "==> tearing down"
      pkill -f 'bazelisk build //:slow' 2>/dev/null
      for d in "$BASE"/wt-*/; do [ -d "$d" ] && ( cd "$d" && "$BZL" shutdown >/dev/null 2>&1 ); done
      [ -d "$BASE/ws" ] && for d in "$BASE"/wt-*/; do git -C "$BASE/ws" worktree remove --force "$d" 2>/dev/null; done
      rm -rf "$BASE" /tmp/bb-loadtest-*.log
      echo "==> done (broker still running)"
      exit 0 ;;
    *) echo "usage: loadtest.sh [-n worktrees] [-s seconds] | --down" >&2; exit 2 ;;
  esac
done

echo "==> standalone workspace repo: $BASE/ws"
rm -rf "$BASE"; mkdir -p "$BASE"
rsync -a --exclude 'bazel-*' --exclude '.git' "$SRC/" "$BASE/ws/"
( cd "$BASE/ws" && git init -q && git config user.email l@l && git config user.name loadtest && git add -A && git commit -qm base )

echo "==> firing $N concurrent //:slow builds at ${SECS}s each"
CACHE="$HOME/.cache/bazel-broker/loadtest-cache"
for i in $(seq 1 "$N"); do
  wt="wt-$i"; path="$BASE/$wt"
  git -C "$BASE/ws" worktree add --detach "$path" HEAD >/dev/null 2>&1
  # Apply E1 cache config so the build emits BEP at the path the broker tails —
  # that's what lets the broker show the command + targets + cache% (not just a
  # discovered process). Without it you'd only get worktree + elapsed.
  BAZEL_BROKER_CACHE="$CACHE" BROKER_DISK_CACHE="$CACHE" \
    bash "$REPO/cache-config/setup.sh" "$path" >/dev/null 2>&1
  ( cd "$path" && nohup "$BZL" build //:slow --action_env=SLOW_SECS="$SECS" \
      >"/tmp/bb-loadtest-$wt.log" 2>&1 & )
  echo "    $wt -> sleeping ${SECS}s"
  sleep 1
done

echo
echo "==> watch: open the menu-bar app, http://127.0.0.1:8765/, or 'bin/brokerctl watch'"
echo "==> stop : scripts/loadtest.sh --down"
