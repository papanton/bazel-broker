#!/usr/bin/env bash
# bazel-broker E1 — setup.sh
# Writes the absolute-path .bazelrc lines for cross-worktree cache sharing
# + profiling into the target iOS project. .bazelrc cannot expand ~ / $HOME,
# so we resolve them here and append a managed block.
#
# Usage:
#   ./setup.sh [PROJECT_DIR] [--cache-dir DIR] [--rc FILE] [--check]
#
# Defaults:
#   PROJECT_DIR : current directory (must be a Bazel workspace / worktree root)
#   --cache-dir : $HOME/.cache/bazel-broker   (shared across ALL worktrees)
#   --rc        : <PROJECT_DIR>/.bazelrc
#
# Re-run setup.sh in EACH worktree: the disk/repo cache is shared, but the
# per-worktree prefix-map root is resolved to THIS worktree's absolute path.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
FRAGMENT="$SCRIPT_DIR/bazelrc.fragment"

PROJECT_DIR="."
CACHE_DIR="${BAZEL_BROKER_CACHE:-$HOME/.cache/bazel-broker}"
RC_FILE=""
CHECK_ONLY=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --cache-dir) CACHE_DIR="$2"; shift 2 ;;
    --rc)        RC_FILE="$2";   shift 2 ;;
    --check)     CHECK_ONLY=1;   shift ;;
    -*)          echo "unknown flag: $1" >&2; exit 2 ;;
    *)           PROJECT_DIR="$1"; shift ;;
  esac
done

if [[ ! -f "$FRAGMENT" ]]; then
  echo "error: fragment not found: $FRAGMENT" >&2
  exit 1
fi

# Resolve to absolute, real paths (handles symlinked worktrees / /var->/private/var
# aliasing on macOS) so the prefix-map root exactly matches what Bazel/clang sees.
PROJECT_DIR="$(cd "$PROJECT_DIR" && pwd -P)"
RC_FILE="${RC_FILE:-$PROJECT_DIR/.bazelrc}"

# Sanity: this must be a Bazel workspace root.
if [[ ! -f "$PROJECT_DIR/MODULE.bazel" && ! -f "$PROJECT_DIR/WORKSPACE" \
   && ! -f "$PROJECT_DIR/WORKSPACE.bazel" ]]; then
  echo "error: $PROJECT_DIR is not a Bazel workspace (no MODULE.bazel/WORKSPACE)" >&2
  exit 1
fi

# Resolve the cache dir to an absolute path too (it may be given relative or with ~).
CACHE_DIR="${CACHE_DIR/#\~/$HOME}"
mkdir -p "$CACHE_DIR"
CACHE_DIR="$(cd "$CACHE_DIR" && pwd -P)"
DISK_CACHE="$CACHE_DIR/disk"
REPO_CACHE="$CACHE_DIR/repo"

BEGIN="# >>> bazel-broker E1 (managed) >>>"
END="# <<< bazel-broker E1 (managed) <<<"

# Build the managed block: static fragment (minus its NOTE comment) + abs lines.
build_block() {
  echo "$BEGIN"
  # Static, path-independent flags from the committed fragment, up to (and not
  # including) the structural sentinel the fragment owns. Everything below the
  # sentinel in the fragment is documentation, not config.
  sed '/^# >>> setup.sh appends absolute-path lines below this marker <<</,$d' "$FRAGMENT"
  cat <<EOF

# --- Absolute paths (resolved by setup.sh; .bazelrc cannot expand ~ / \$HOME) ---
build --disk_cache=$DISK_CACHE
build --repository_cache=$REPO_CACHE

# --- Per-worktree relocatability prefix maps, C/C++/ObjC clang path ONLY ---
#     (Swift relocatability comes from the rules_swift swift.file_prefix_map
#      feature in the static fragment; swiftc rejects the clang spelling.)
build --copt=-ffile-prefix-map=$PROJECT_DIR=.
build --copt=-fdebug-prefix-map=$PROJECT_DIR=.
build --objccopt=-ffile-prefix-map=$PROJECT_DIR=.
build --objccopt=-fdebug-prefix-map=$PROJECT_DIR=.
$END
EOF
}

NEW_BLOCK="$(build_block)"

# Extract the current managed block (between BEGIN/END inclusive) from a file.
extract_block() {  # $1=file
  awk -v b="$BEGIN" -v e="$END" '
    $0==b {f=1} f {print} $0==e {f=0}
  ' "$1"
}

# Print a file with any managed block (BEGIN..END inclusive) removed — the
# complement of extract_block, sharing the same one-line awk state machine.
strip_block() {  # $1=file
  awk -v b="$BEGIN" -v e="$END" '
    $0==b {f=1} !f {print} $0==e {f=0}
  ' "$1"
}

if [[ "$CHECK_ONLY" == 1 ]]; then
  if [[ -f "$RC_FILE" ]] && \
     diff <(extract_block "$RC_FILE") <(printf '%s\n' "$NEW_BLOCK") >/dev/null 2>&1; then
    echo "ok: $RC_FILE managed block is up to date"; exit 0
  fi
  echo "drift: $RC_FILE managed block missing or stale (run setup.sh to fix)"; exit 1
fi

mkdir -p "$DISK_CACHE" "$REPO_CACHE"

# Bazel does NOT create the parent dir for --profile / --build_event_json_file;
# it errors and omits the file if .bazel-broker/ is missing. Create it per worktree.
mkdir -p "$PROJECT_DIR/.bazel-broker"

# Strip any existing managed block, then append the freshly-built one. Strip +
# append is simpler than an in-place splice and reuses strip_block's awk idiom.
touch "$RC_FILE"
tmp="$(mktemp)"
# $(strip_block ...) drops the old block AND any trailing blank lines (command
# substitution strips them), so re-runs don't accumulate whitespace.
printf '%s\n\n%s\n' "$(strip_block "$RC_FILE")" "$NEW_BLOCK" > "$tmp"
mv "$tmp" "$RC_FILE"

# Ensure broker artifacts are ignored by git in this worktree.
GI="$PROJECT_DIR/.gitignore"
if ! { [[ -f "$GI" ]] && grep -qxF ".bazel-broker/" "$GI"; }; then
  echo ".bazel-broker/" >> "$GI"
fi

echo "wrote managed block to $RC_FILE"
echo "  disk_cache       = $DISK_CACHE   (shared across worktrees)"
echo "  repository_cache = $REPO_CACHE"
echo "  prefix-map root  = $PROJECT_DIR  (this worktree)"
echo "NOTE: re-run setup.sh in EACH worktree so its prefix-map root is correct."
