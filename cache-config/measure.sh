#!/usr/bin/env bash
# bazel-broker E1 — measure.sh: two-worktree (or two-location) cache-hit experiment.
#
# Usage: ./measure.sh <WORKTREE_A> <WORKTREE_B> <TARGET>
#
# Builds <TARGET> cold in A (populating the shared --disk_cache), then builds the
# SAME target in B and reports B's cross-worktree cache-hit ratio.
#
# CACHE-HIT DEFINITION (binding, consolidated-review C8 / P4):
#   The hit ratio is derived from Bazel's ActionSummary.runner_count[] — the
#   STRUCTURED form of the end-of-build "N processes: X disk cache hit, Y internal,
#   Z linux-sandbox" summary line. It is the SAME number E4 computes from the BEP.
#
#     disk_hits        = runner_count[name=="disk cache hit"].count
#     executed_runners = sum(runner_count[i].count) over names that locally EXECUTED
#                        an action — i.e. EXCLUDING the synthetic "total" entry,
#                        EXCLUDING the non-executing "internal" entry (symlink/
#                        workspace-status/bookkeeping actions that never touch the
#                        disk cache), AND EXCLUDING "disk cache hit" itself
#                        (e.g. "worker", "darwin-sandbox", "local", "remote cache hit").
#     hit_ratio        = disk_hits / (disk_hits + executed_runners)
#
#   Equivalently: of every action that produced an output (hits + locally executed),
#   what fraction was served from the shared disk cache. The "N processes:" summary
#   line is the same data: "35 disk cache hit, 69 internal, 4 worker" → 35/(35+4).
#
#   NOTE: `actions_created - actions_executed` is WRONG — a disk-cache hit is counted
#   INSIDE actions_executed, so that formula understates/inverts the real hit rate.
#   We do NOT use it.
set -euo pipefail

if [[ $# -ne 3 ]]; then
  echo "usage: $0 <WORKTREE_A> <WORKTREE_B> <TARGET>" >&2
  exit 2
fi

A="$(cd "$1" && pwd -P)"; B="$(cd "$2" && pwd -P)"; TARGET="$3"
BEP_REL=".bazel-broker/bep.json"

# Print the runner_count-based stats for a worktree's bep.json as:
#   "<disk_hits> <executed_runners> <total_runners>"
# Casing is protojson lowerCamelCase (Bazel 8.x/9.x default for --build_event_json_file).
runner_stats() {  # $1=worktree dir
  local f="$1/$BEP_REL"
  jq -rs '
    (map(select(.buildMetrics)) | last // {}).buildMetrics.actionSummary.runnerCount // []
    | (map(select(.name=="disk cache hit") | .count) | add // 0) as $hits
    | (map(select(.name!="total" and .name!="internal" and .name!="disk cache hit") | .count) | add // 0) as $exec
    | (map(select(.name=="total") | .count) | add // 0) as $total
    | "\($hits) \($exec) \($total)"
  ' "$f"
}

echo "== cold build in A: $A =="
( cd "$A" && bazel clean && bazel build "$TARGET" 2>&1 | tee /tmp/bb_a.log )

echo
echo "== warm build in B (should hit A's shared disk cache): $B =="
( cd "$B" && bazel clean && bazel build "$TARGET" 2>&1 | tee /tmp/bb_b.log )

echo
echo "================ RESULTS (B = warm build) ================"
read -r hits_b exec_b total_b < <(runner_stats "$B")
: "${hits_b:=0}" "${exec_b:=0}" "${total_b:=0}"
echo "B runner_count: disk_cache_hit=$hits_b  executed(non-internal)=$exec_b  total=$total_b"

denom=$(( hits_b + exec_b ))
if [[ "$denom" -gt 0 ]]; then
  awk -v h="$hits_b" -v d="$denom" \
    'BEGIN{printf "B cross-worktree disk-cache HIT RATIO = %.1f%% (%d hits / %d cacheable actions)\n", (h/d)*100, h, d}'
else
  echo "B cross-worktree disk-cache HIT RATIO = n/a (no cacheable actions in runner_count)"
fi

echo
echo "Bazel summary line (B) — authoritative cross-check, casing-independent:"
grep -E "Process(es)?:|disk cache hit|processes:" /tmp/bb_b.log || \
  echo "  (no 'N processes:' summary line — target may have been a full disk-cache hit with 0 local processes)"

echo
echo "Bazel summary line (A, cold) for reference:"
grep -E "Process(es)?:|disk cache hit|processes:" /tmp/bb_a.log || echo "  (none)"
