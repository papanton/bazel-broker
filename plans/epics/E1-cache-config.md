# E1 — Cache sharing & profiling config

> Cross-worktree Bazel cache hits + per-build profiling for an iOS (ObjC/Swift) app,
> with **zero application code**. Pure config + a setup script + a doc.

Status: **Draft v2 (staff-reviewed)** · Owner: Antonis · Maps to architecture M0 / §8 · Last updated: 2026-06-17

Parent docs: [`01-architecture.md`](../01-architecture.md) §8 (cache strategy) · [`02-epics.md`](../02-epics.md) EPIC 1.

---

## 1. Goal & scope recap

**Goal:** make Bazel builds run from *different git worktrees* share a single on-disk cache so
that work done in worktree A becomes a cache **hit** in worktree B, and make every build emit a
Perfetto-openable timing profile — all without writing any broker code.

**Why this epic is independent.** Per the epic dependency graph (`02-epics.md`), E1 has
**no upstream dependency** — it is config that lives in the *iOS project's* `.bazelrc`, not in the
bazel-broker repo. It is the only epic that delivers a concrete win (cross-worktree cache hits)
before any daemon exists. The one *downstream* consumer is **E4 (BEP ingest, metrics & cache
insight)**, which depends on E1 because it tails the `--build_event_json_file` that E1 configures
and reads the `BuildMetrics` cache stats that E1 makes meaningful. E1 must therefore **pin the
file-path conventions** E4 will rely on (see §4).

**In scope (deliverables):**
1. A `.bazelrc` fragment for the **user's iOS project** (not this repo) containing:
   - shared `--disk_cache` (absolute path),
   - `--profile` per build,
   - per-worktree **relative** `--build_event_json_file`,
   - relocatability **primarily via the rules_apple/rules_swift/apple_support relocatability
     *features*** (`oso_prefix_is_pwd`, `relative_ast_path`, `remap_xcode_path`,
     `swift.cacheable_swiftmodules`, `swift.file_prefix_map`, `swift.coverage_prefix_map`,
     `swift.use_global_module_cache`) — these are the idiomatic, toolchain-aware mechanism and are
     default-on in recent versions; we assert/pin them with `--features=…`. Raw
     `-ffile-prefix-map`/`-fdebug-prefix-map` via `--copt`/`--objccopt` is a *supplement* for the
     plain C/C++/ObjC compile path only (Swift uses a different spelling — see §2.5). Plus
     `--incompatible_strict_action_env` and `--experimental_output_paths=strip`.

   > **Correction from v1:** v1 specified `--swiftcopt=-ffile-prefix-map=<WORKTREE>=.`. That is
   > **wrong** — `swiftc` does not accept the clang `-ffile-prefix-map` spelling; it uses
   > single-dash `-file-prefix-map`/`-debug-prefix-map`/`-coverage-prefix-map`. The correct path
   > is to let the rules_swift `file_prefix_map`/`coverage_prefix_map` features emit them. See §2.5.
2. A `setup.sh` (lives in **this** repo, run against the iOS project) that writes the
   absolute-path lines, since `.bazelrc` cannot expand `~` / `$HOME`.
3. A cache doc explaining what each flag does and the **"never share the output base"** rule.
4. A documented **two-worktree measurement method** using BEP `BuildMetrics` to prove the hit-rate
   improvement.

**Explicitly out of scope (belongs to other epics, do not build here):**
- Broker-side hit-ratio visualization, low-hit alerts, disk-cache GC/size cap, admission
  stagger (anti–thundering-herd), keep-servers-warm tuning → **E4 / E5**. E1 only *enables* the
  metrics; it does not ingest or act on them.
- The `tools/bazel` wrapper that *injects* these flags at runtime → **E5**. E1 ships the
  static `.bazelrc` path; the wrapper is an alternative/duplicate injection mechanism added later.
- `bazel-diff` (build-less) → architecture §8.5, not an E1 deliverable.

**Target environment:** macOS arm64 (Apple Silicon), Bazel **9.x** via Bazelisk, iOS app built
with `rules_apple` + `rules_swift` (ObjC + Swift + some C/C++), `apple_support` toolchain.

> **Version assumption that gates the whole relocatability story:** apple_support ≥ ~1.5 and
> rules_swift ≥ ~1.5 enable the relocatability features above *by default*. T0 (new) **records the
> resolved versions** (`bazel mod graph | grep -E 'apple_support|rules_swift'`); if they are older,
> E1 must enable the features explicitly and may need the manual maps as a real (not belt-and-suspenders)
> fallback. Do not author the manual maps as the primary lever — verify what the rules already do first.

---

## 2. Design & implementation details

### 2.1 Repository layout (this repo)

E1 adds a self-contained `cache-config/` directory to the bazel-broker repo. Nothing here is
compiled; it is shipped to the user and applied to *their* iOS project.

```
bazel-broker/
  cache-config/
    setup.sh                 # writes absolute-path lines into the iOS project
    bazelrc.fragment         # the static, path-independent flags (template)
    README.md                # the cache doc (flag explanations + rules)
    measure.sh               # two-worktree cache-hit experiment (uses jq)
  plans/epics/E1-cache-config.md   # this file
```

> Naming note: keep the fragment filename `bazelrc.fragment` (not `.bazelrc`) so it is never
> picked up accidentally as a real rc file inside *this* repo.

### 2.2 The three cache layers (recap from §8.1, drives every decision)

| Layer | Scope | Action in E1 |
|---|---|---|
| Analysis cache (Skyframe, in-memory) | per server / output base | **cannot** share — leave alone (warm-server tuning is E5) |
| Local action cache | per output base | superseded by disk cache; do nothing |
| **Disk cache** (`--disk_cache`) | a directory | **share** — the main lever |
| Repository cache (`--repository_cache`) | per-user | **already shared by default**; set explicitly as belt-and-suspenders |

**The cardinal rule, stated once and loudly in the doc:** *share the `--disk_cache`, NEVER share
the output base.* Sharing the output base would force every worktree onto the same Bazel server and
its single exclusive lock (architecture §3.2), serializing all builds — the exact opposite of the
goal. The output base is keyed by `output_user_root / md5(workspace_path)`, so distinct worktree
paths already get distinct output bases for free; E1 must **not** override `--output_base` /
`--output_user_root`.

### 2.3 The static fragment — `cache-config/bazelrc.fragment`

These lines are path-independent and can be committed verbatim. The two absolute-path lines
(`--disk_cache`, `--repository_cache`) and the prefix-map lines (which reference `$HOME`/worktree
roots) are **omitted here** and written by `setup.sh` (§2.5), because `.bazelrc` does not expand
`~` or `$HOME`.

```bazelrc
# ============================================================================
#  bazel-broker E1 — cache sharing & profiling (static fragment)
#  Managed block. Absolute-path lines are appended by bazel-broker setup.sh.
#  Do not edit between the BEGIN/END markers; re-run setup.sh instead.
# ============================================================================

# --- Relocatability (idiomatic): assert the rules_apple/rules_swift/apple_support
#     relocatability features. These are default-ON in recent versions; we pin them so
#     the cache stays relocatable even if a user/CI flips defaults. They are the PRIMARY
#     lever for cross-worktree (and cross-machine) hits on an iOS toolchain. (§2.5)
#     - apple_support: oso_prefix_is_pwd, relative_ast_path, remap_xcode_path
#     - rules_swift:   swift.cacheable_swiftmodules, swift.file_prefix_map,
#                      swift.coverage_prefix_map, swift.use_global_module_cache
common --features=oso_prefix_is_pwd
common --features=relative_ast_path
common --features=remap_xcode_path
common --features=swift.cacheable_swiftmodules
common --features=swift.file_prefix_map
common --features=swift.coverage_prefix_map
common --features=swift.use_global_module_cache

# --- Relocatability: strip config-specific output path segments so that
#     bazel-{out}/<cfg>/... is the same string regardless of worktree/config.
#     Bazel 7+; still flagged `experimental` in 9.x — confirm spelling per OD-5. (§8.3)
common --experimental_output_paths=strip

# --- Pin the action environment so it does not vary with each shell's PATH/env,
#     which would otherwise bust the action key across worktrees/agents.
#     NOTE (iOS): DEVELOPER_DIR/SDKROOT are set by the apple_support toolchain, NOT
#     inherited from the client env, so strict mode does NOT strip them. The real risk
#     is non-hermetic *tools on PATH*. See §2.5 / R2 / OD-6 before assuming this is safe.
build --incompatible_strict_action_env

# --- Profiling: every build writes a Chrome-trace profile. The path is RELATIVE
#     to the workspace root, so it lands inside each worktree (collision-safe,
#     see §2.4). Openable directly in ui.perfetto.dev.
build --generate_json_trace_profile
build --profile=.bazel-broker/command.profile.gz

# --- Live build event stream for the broker (E4) to tail. RELATIVE path →
#     one file per worktree, never shared, never clobbered (see §2.4).
#     ⚠ CONTRACT-OPEN: E4 *recommends a per-invocation* filename; this is a per-WORKTREE
#       static path. See §4 and escalation EC-1. Do not treat as final.
build --build_event_json_file=.bazel-broker/bep.json

# NOTE: the following are written by setup.sh with ABSOLUTE paths:
#   build --disk_cache=<ABS>/bazel-broker-cache/disk
#   build --repository_cache=<ABS>/bazel-broker-cache/repo
#   build --copt=-ffile-prefix-map=<WORKTREE_ABS>=.          # C/C++/ObjC clang path only
#   build --copt=-fdebug-prefix-map=<WORKTREE_ABS>=.
#   build --objccopt=-ffile-prefix-map=<WORKTREE_ABS>=.
#   build --objccopt=-fdebug-prefix-map=<WORKTREE_ABS>=.
#   # NO --swiftcopt=-ffile-prefix-map: swiftc rejects the clang spelling; the
#   #   rules_swift `swift.file_prefix_map` feature (above) emits the correct
#   #   single-dash `-file-prefix-map` itself. (§2.5)
```

Notes on the static lines:
- `common` (vs `build`) for `--experimental_output_paths=strip` and the `--features` lines so they
  apply to every command that does analysis (build/test/run), keeping output paths identical across
  phases.
- The `--features` lines are **idempotent assertions** of defaults, not new behavior on a current
  toolchain. They matter as a guard and as documentation of *which* features deliver relocatability.
  **OD-2** — verify against the project's actual versions (T0) and via `aquery` (T8) that they fire;
  if already default-on, keep them anyway (cheap insurance) but note it.
- `--generate_json_trace_profile` is the explicit on-switch for the profile; `--profile=` sets the
  path. In Bazel 9.x the profile is `auto` (on for `build`), but we set it explicitly so the path is
  deterministic and per-worktree.
- **Dropped from v1:** `--build_event_json_file_path_conversion=no`. v1's rationale ("compact the
  stream / keep file stable for tailing / no buffering surprises") was **incorrect** — this flag
  does *not* affect buffering, compaction, or the BEP file's own path. It controls whether
  **paths embedded inside BEP content** (e.g. the `--profile` URI E4 reads to locate the trace) are
  rewritten to globally-valid `file://` URIs (default `true`). It has **zero** effect on the tailer.
  Whether E4 prefers plain local paths vs `file://` *inside* the events is **E4's** call (it must
  strip `file://` regardless for the Perfetto deep-link, §2.9 of E4) → moved to **OD-5/EC-1**, not set here.

### 2.4 Why a **relative** `--build_event_json_file` is collision-safe

This is the subtle correctness argument the epic must nail.

- A **relative** path is resolved against the **workspace root** (the worktree directory). So
  `--build_event_json_file=.bazel-broker/bep.json` becomes
  `/worktrees/feature-a/.bazel-broker/bep.json` in worktree A and
  `/worktrees/feature-b/.bazel-broker/bep.json` in worktree B. **Different worktrees → different
  absolute files.** No cross-worktree clobber.
- *Within* a single worktree, could two concurrent builds clobber the same `bep.json`? **No** —
  because of the **exclusive per-output-base lock** (architecture §3.2). All builds from one
  worktree share one output base and therefore one server, and that server runs **at most one
  command at a time**; a second `bazel build` in the same worktree blocks on the lock (or fails
  fast with `--block_for_lock=false`). So there is never more than one writer to a given worktree's
  `bep.json` at a time. The relative path is collision-safe precisely *because* the per-output-base
  lock serializes intra-worktree builds.
  - **Scope caveat (added v2):** this holds only because E1 does **not** set `--output_base` /
    `--output_user_root` (§2.2 rule). Two `bazel` invocations in the *same* worktree but with
    *different* explicit output bases would NOT share the lock and could write `bep.json`
    concurrently. E1's invariant (output base untouched) is therefore load-bearing for this claim;
    it is listed in the §4 contract table and must not be relaxed by E5's wrapper.
- Contrast with an **absolute static** path (e.g. `--build_event_json_file=/tmp/bep.json` in a
  shared rc): that **would** clobber across concurrent worktrees (different output bases → genuinely
  parallel builds → simultaneous writers to one file). This is the failure mode called out in
  architecture §C3. Relative paths sidestep it without the broker minting per-invocation paths.
- Same reasoning applies to `--profile=.bazel-broker/command.profile.gz`: relative → per-worktree,
  and the per-output-base lock prevents intra-worktree overwrite while a build is in flight.
- **Trade-off / open decision (OD-1) — and a DIRECT CONFLICT WITH E4 (escalate, EC-1):** a relative
  per-worktree BEP path gives at most one *current* file per worktree; the next build in the same
  worktree **overwrites** (in-place truncates) it. E1 argues this is fine for "live tail + most-recent
  metrics." **But E4's plan (§2.2, §4, D-E4-2) explicitly *recommends a per-invocation* filename**
  (`.bazel-broker/<invocation_id>.bep.json`) precisely because in-place truncation of a reused path
  is "the dangerous case: the tailer's read offset is now past EOF and it silently stalls," forcing
  E4 to build an inode/size **truncation supervisor** as a workaround. So this is not merely an E1
  history trade-off — **the two epics currently disagree on the contract.** Options:
  - **(A)** E1 keeps per-worktree path; E4 owns the truncation supervisor (E4's fallback path). Cheaper
    rc, more tailer complexity + residual stall risk.
  - **(B)** E1 emits per-invocation paths. Eliminates truncation entirely, gives free on-disk history,
    but `.bazelrc` alone cannot interpolate the invocation id → needs the E5 `tools/bazel` wrapper or
    a `--bep_…` convention, which **reintroduces an E5 dependency E1 was designed to avoid** (E1 is
    "zero code, static rc"). 
  - **Recommendation to escalate:** keep E1 static/per-worktree for the no-wrapper milestone (M0/M3),
    and have E4 implement the truncation supervisor it already specced; switch to per-invocation only
    once E5's wrapper exists. **Do not silently pick — this is the headline E1↔E4 contract decision.**

`.bazel-broker/` should be added to the iOS project's `.gitignore` (setup.sh does this, §2.6).

### 2.5 Relocatability — making the shared cache actually hit

The disk cache is a content-addressed action cache: worktree B hits worktree A's result only when
the **action key** matches. The action key is a hash over the command line, input file paths,
environment, and output paths. Three things bust it across worktrees even for byte-identical source:

1. **Absolute worktree paths leaking into command lines / debug info.** `clang`/`swiftc` embed the
   source file's absolute path in object files, DWARF debug info, `.swiftmodule`, OSO stabs entries,
   and AST sections, and pass absolute `-I`/`-fmodule-map` paths.
   `/worktrees/feature-a/Foo.m` ≠ `/worktrees/feature-b/Foo.m` → different key → miss.
2. **Environment variation.** The action env (PATH, locale, agent-specific vars) differs per shell
   and per Claude instance → busts the env component of the key.
3. **Config-specific output path segments.** `bazel-out/<hash-or-cfg>/bin/...` segments can differ.

**The correct fix on an iOS toolchain is FEATURES first, raw maps second.** This is the single most
important revision in v2. The Bazel Apple ecosystem already solves relocatability with
toolchain-aware *features* that emit the right per-compiler flags (including the **OSO/AST/Xcode
path** cases that raw `--copt` maps do not cover) — this is how BuildBuddy/EngFlow shops get
cross-machine cache hits. Use them; do not reinvent them with raw prefix-maps:

| Feature | Module | What it fixes |
|---|---|---|
| `oso_prefix_is_pwd` | apple_support | Rewrites the OSO (debug-map) absolute prefix in linked binaries to `.` |
| `relative_ast_path` | apple_support | Makes embedded AST paths relative (debug info relocatable) |
| `remap_xcode_path` | apple_support | Replaces the absolute Xcode/DEVELOPER_DIR path with a fixed token so a different Xcode/worktree path still hits |
| `swift.file_prefix_map` | rules_swift | Emits swiftc's **single-dash `-file-prefix-map`** (maps `$PWD`→`.`) — the *correct* Swift spelling |
| `swift.coverage_prefix_map` | rules_swift | Same for `-coverage-prefix-map` (coverage builds) |
| `swift.cacheable_swiftmodules` | rules_swift | Strips absolute paths embedded in `.swiftmodule` |
| `swift.use_global_module_cache` | rules_swift | Fixed module-cache location so the implicit-module cache is reused, not path-keyed |

These are **default-on** in apple_support ≥ ~1.5 and rules_swift ≥ ~1.5. We pin them with `--features=`
(§2.3) as a guard + documentation. **Verify they fire** via `aquery` (T8) on the project toolchain.

**Why NOT `--swiftcopt=-ffile-prefix-map` (v1 bug):** `swiftc` does **not** accept clang's
`-ffile-prefix-map`/`-fdebug-prefix-map` spelling; the Swift frontend uses single-dash
`-file-prefix-map` / `-debug-prefix-map` / `-coverage-prefix-map` (Swift 5.7+). Passing the clang
spelling via `--swiftcopt` would error or be silently dropped — a cold cache that *looks* configured.
The rules_swift `swift.file_prefix_map` feature emits the correct flag with the right `$PWD`→`.`
mapping; do **not** hand-roll it through `--swiftcopt`.

**Raw clang maps as a *supplement* (C/C++/ObjC only).** For the plain C/C++/ObjC compile path that
is not driven by a relocatability feature, the raw maps are still useful belt-and-suspenders:
`-ffile-prefix-map=<WORKTREE_ABS>=.` (implies `-fmacro-prefix-map` + `-fdebug-prefix-map`) plus an
explicit `-fdebug-prefix-map=<WORKTREE_ABS>=.` for older clang, via `--copt` and `--objccopt` only.
**OD-2b** — confirm via `aquery` these don't *double-map* or conflict with what apple_support's
crosstool already injects (`oso_prefix_is_pwd` operates at link time, the maps at compile time, so
they should be orthogonal — verify).

> **Subtle: does `=$PROJECT_DIR=.` even match the path clang embeds?** The map only fires if the
> absolute string clang/swiftc writes is literally the worktree root. With sandboxed execution the
> compiler's cwd is the **sandbox/exec root**, and sources are reached through the execroot symlink
> forest — the embedded `$PWD` may be the *exec root path* (which contains the output base, itself
> path-derived) rather than the bare worktree path. This is exactly why the feature-based approach
> (`$PWD`→`.`, where `$PWD` is whatever the action's real cwd is) is more robust than a hardcoded
> `<WORKTREE_ABS>=.` map: the feature maps the *actual* cwd, the hardcoded map guesses it. T8's
> ablation + `aquery` must confirm the hardcoded map's LHS matches reality; if it doesn't, **rely on
> the features and drop the raw maps** rather than tuning the LHS per sandbox mode. (OD-2b.)

**Env (item 2):** `--incompatible_strict_action_env` pins a static action env. **iOS caveat:**
`DEVELOPER_DIR`/`SDKROOT` are injected by the apple_support **toolchain** (and resolved via the
`__BAZEL_XCODE_*__` placeholder mechanism + `remap_xcode_path`), *not* inherited from the client
shell — so strict mode does **not** strip them and does **not**, by itself, break the Xcode path.
The genuine breakage risk is a build action that depends on a *non-hermetic tool discovered on the
user's `PATH`* (homebrew tools, code-signing helpers, a Python on PATH — see bazelbuild/bazel#8536).
Mitigation in R2/OD-6: re-admit the *specific* missing var with `--action_env=NAME` rather than
abandoning strict mode. Under strict mode PATH defaults to `/bin:/usr/bin`.

**Output paths (item 3):** `--experimental_output_paths=strip` strips config-specific segments from
output paths in action command lines. Still `experimental`-prefixed on 9.x → OD-5 spelling check.

> Why the maps target `=.` and not `=` (empty): mapping to `.` keeps paths as valid relative paths
> (`./Foo.m`) that downstream tools and lldb can still resolve against the worktree, rather than bare
> filenames that break source lookup during debugging. (Note the features above use the same `$PWD`→`.`
> convention, so the two approaches are consistent.)

### 2.6 `setup.sh` — full script

`setup.sh` runs **against the iOS project** (passed as `$1` or `--project`, default `.`). It is
idempotent: it manages a marked block in the iOS project's `.bazelrc` and rewrites it on each run.
It derives the **per-worktree absolute path** for the prefix-maps at the worktree it is run in, and
warns that it must be re-run per worktree (each worktree needs its own prefix-map line; see OD-3).

```bash
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

# Resolve to absolute, real paths (handles symlinked worktrees on macOS).
PROJECT_DIR="$(cd "$PROJECT_DIR" && pwd -P)"
RC_FILE="${RC_FILE:-$PROJECT_DIR/.bazelrc}"

# Sanity: this must be a Bazel workspace root.
if [[ ! -f "$PROJECT_DIR/MODULE.bazel" && ! -f "$PROJECT_DIR/WORKSPACE" \
   && ! -f "$PROJECT_DIR/WORKSPACE.bazel" ]]; then
  echo "error: $PROJECT_DIR is not a Bazel workspace (no MODULE.bazel/WORKSPACE)" >&2
  exit 1
fi

DISK_CACHE="$CACHE_DIR/disk"
REPO_CACHE="$CACHE_DIR/repo"

BEGIN="# >>> bazel-broker E1 (managed) >>>"
END="# <<< bazel-broker E1 (managed) <<<"

# Build the managed block: static fragment (minus its NOTE comment) + abs lines.
build_block() {
  echo "$BEGIN"
  # Static, path-independent flags from the committed fragment, stripping the
  # trailing NOTE comment block (everything from the first 'NOTE:' line on).
  sed '/^# NOTE: the following/,$d' "$FRAGMENT"
  cat <<EOF

# --- Absolute paths (resolved by setup.sh; .bazelrc cannot expand ~ / \$HOME) ---
build --disk_cache=$DISK_CACHE
build --repository_cache=$REPO_CACHE

# --- Per-worktree relocatability prefix maps, C/C++/ObjC clang path ONLY ---
#     (Swift relocatability comes from the rules_swift swift.file_prefix_map
#      feature in the static fragment; swiftc rejects the clang spelling. §2.5)
build --copt=-ffile-prefix-map=$PROJECT_DIR=.
build --copt=-fdebug-prefix-map=$PROJECT_DIR=.
build --objccopt=-ffile-prefix-map=$PROJECT_DIR=.
build --objccopt=-fdebug-prefix-map=$PROJECT_DIR=.
$END
EOF
}

NEW_BLOCK="$(build_block)"

if [[ "$CHECK_ONLY" == 1 ]]; then
  if [[ -f "$RC_FILE" ]] && \
     diff <(awk "/^${BEGIN//\//\\/}\$/{f=1} f; /^${END//\//\\/}\$/{f=0}" "$RC_FILE") \
          <(printf '%s\n' "$NEW_BLOCK") >/dev/null 2>&1; then
    echo "ok: $RC_FILE managed block is up to date"; exit 0
  fi
  echo "drift: $RC_FILE managed block missing or stale (run setup.sh to fix)"; exit 1
fi

mkdir -p "$DISK_CACHE" "$REPO_CACHE"

# Replace any existing managed block in-place; otherwise append.
touch "$RC_FILE"
if grep -qF "$BEGIN" "$RC_FILE"; then
  tmp="$(mktemp)"
  awk -v begin="$BEGIN" -v end="$END" -v block="$NEW_BLOCK" '
    $0==begin {print block; skip=1; next}
    $0==end   {skip=0; next}
    !skip     {print}
  ' "$RC_FILE" > "$tmp"
  mv "$tmp" "$RC_FILE"
else
  { echo ""; printf '%s\n' "$NEW_BLOCK"; } >> "$RC_FILE"
fi

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
```

Key decisions baked into the script:
- **Shared cache default** `$HOME/.cache/bazel-broker/{disk,repo}` — a single location for *all*
  worktrees, overridable via `--cache-dir` or `$BAZEL_BROKER_CACHE`. This is the disk-cache
  location convention E4's GC will target (§4).
- **Idempotent managed block** with BEGIN/END markers → re-running rewrites, never duplicates.
- **`--check` mode** for CI / a Make target to assert the rc is current without mutating it.
- **Creates the cache dirs** so the first build does not race on directory creation.
- **`pwd -P`** resolves symlinks (macOS worktrees and `/var`→`/private/var` aliasing) so the
  prefix-map root exactly matches the path Bazel/clang will see — otherwise the map silently
  fails to match and the cache stays cold.

### 2.7 Measurement method — `cache-config/measure.sh`

Proves the "Done when" hit-rate claim from `BuildMetrics` in the BEP json (the same event E4
ingests). Pure shell + `jq`; no broker required.

Approach: clean both worktrees' output bases, build the target in worktree A (cold — populates the
shared disk cache), then build the *same* target in worktree B and read its cache stats from B's
`bep.json`. The relevant BEP fields (under `buildMetrics.actionSummary`):

- `actionsCreated` / `actionsExecuted` — total vs locally executed actions.
- `actionCacheStatistics` and per-`ActionData` `actionsExecuted` — actions that ran vs were served
  from cache.

The robust, version-stable hit metric is **`1 - (actionsExecuted / actionsCreated)`** for the
B build, cross-checked against Bazel's own end-of-build "processes" summary line
(`INFO: ... N processes: M disk cache hit, ...`). measure.sh prints both.

> **Metric caveat (must stay consistent with E4 D-E4-3).** `1 - executed/created` is a *proxy*, not
> a true disk-cache-hit ratio: `actionsCreated` includes non-cacheable **internal/worker/symlink**
> actions that never touch the disk cache, so the proxy *understates* the real hit rate, and A's vs
> B's `actionsCreated` can differ slightly (e.g. coverage/aspect actions). The **authoritative**
> number for the "Done when" is the Bazel summary line's explicit `M disk cache hit` count
> (`grep`-ed below) — that is what E4 stores as ground-truth (`summary_line`) and diffs against. E1
> and E4 must report the **same** headline figure: prefer the parsed `disk cache hit` count;
> show the BEP proxy only as a secondary, clearly-labelled cross-check. (Pin field casing per OD-4.)

```bash
#!/usr/bin/env bash
# bazel-broker E1 — measure.sh: two-worktree cache-hit experiment.
# Usage: ./measure.sh <WORKTREE_A> <WORKTREE_B> <TARGET>
set -euo pipefail
A="$(cd "$1" && pwd -P)"; B="$(cd "$2" && pwd -P)"; TARGET="$3"

bep_hits() {  # $1=worktree dir -> prints "executed created" from its bep.json
  local f="$1/.bazel-broker/bep.json"
  # BEP json is one JSON object per line; pick the BuildMetrics event.
  jq -rs '
    map(select(.buildMetrics)) | last
    | .buildMetrics.actionSummary
    | "\(.actionsExecuted // 0) \(.actionsCreated // 0)"' "$f"
}

echo "== cold build in A: $A =="
( cd "$A" && bazel clean && bazel build "$TARGET" 2>&1 | tee /tmp/bb_a.log )

echo "== warm build in B (should hit A's disk cache): $B =="
( cd "$B" && bazel clean && bazel build "$TARGET" 2>&1 | tee /tmp/bb_b.log )

read -r exec_b created_b < <(bep_hits "$B")
echo "B actionsExecuted=$exec_b actionsCreated=$created_b"
if [[ "${created_b:-0}" -gt 0 ]]; then
  awk -v e="$exec_b" -v c="$created_b" \
    'BEGIN{printf "B disk-cache hit ratio = %.1f%%\n", (1-e/c)*100}'
fi
echo "Bazel summary (B):"; grep -E "processes:|disk cache hit" /tmp/bb_b.log || true
```

> `bazel clean` (not `clean --expunge`) keeps repository cache warm and only drops the output
> base's local state, forcing B to rely on the **shared disk cache** for action results — which is
> exactly what we want to measure. **OD-4** — confirm the exact `actionSummary` field names on the
> project's Bazel 9.x BEP (`protojson` casing can be `actionsCreated` vs `actions_created`).
>
> **Correction (v2):** the prose claimed measure.sh "tolerates both via `jq //` fallbacks," but the
> `jq` above only handles a missing *value* (`// 0`), **not** a different *key casing* — `.buildMetrics`
> / `.actionSummary` are hardcoded. `protojson` BEP emits **lowerCamelCase** by default
> (`buildMetrics`, `actionSummary`, `actionsExecuted`), so the current selectors are the right bet for
> 9.x, but if a build uses `--build_event_json_file` with proto field names the keys become snake_case
> and the script silently prints `0 0`. Before relying on it, run once and confirm the keys; if needed,
> add real casing fallbacks (`(.buildMetrics // .build_metrics) | (.actionSummary // .action_summary)`).
> The headline number should come from the **`disk cache hit` summary-line grep** regardless (above),
> which is casing-independent — the BEP proxy is the secondary check.

---

## 3. Sequencing (ordered, checkpointed task list)

Each task is independently verifiable. T0 + Tasks 1–4 are repo-local (T0 needs the iOS project's
module graph only); 5–8 need a real iOS workspace with ≥2 worktrees.

- [ ] **T0. Pin toolchain versions (gates the relocatability design).** Record resolved
  `bazel`, `apple_support`, `rules_swift`, `rules_apple` versions
  (`bazel --version`; `bazel mod graph | grep -E 'apple_support|rules_swift|rules_apple'`).
  **Verify:** if apple_support ≥1.5 & rules_swift ≥1.5, the relocatability features are default-on
  and the `--features=` lines are assertions; if older, note that they must be enabled and the raw
  clang maps become a real fallback. Records the answer for OD-2/OD-5. *(repo-local-ish; ~15 min)*
- [ ] **T1. Scaffold `cache-config/`.** Create the directory + empty `setup.sh`, `bazelrc.fragment`,
  `measure.sh`, `README.md`. **Verify:** files exist; `bash -n setup.sh` parses.
- [ ] **T2. Write `bazelrc.fragment`** (the static lines, §2.3). **Verify:** the fragment contains
  only path-independent flags; no `~`/`$HOME`/absolute paths outside the NOTE comment.
- [ ] **T3. Implement `setup.sh`** (§2.6) incl. `--check`, managed-block rewrite, `.gitignore`
  edit. **Verify (no iOS project needed):** run against a throwaway dir containing an empty
  `MODULE.bazel`; assert `.bazelrc` gets a single managed block with absolute `--disk_cache`;
  run twice → still exactly one block (idempotent); `--check` exits 0 after, exits 1 after manual
  edit; cache dirs created.
- [ ] **T4. Write `README.md` cache doc** (§2.2 table, per-flag explanations §2.5, the
  "never share the output base" rule, the relative-path collision-safety argument §2.4).
  **Verify:** doc review; every flag in the fragment is explained.
- [ ] **T5. Apply to a real iOS project, one worktree.** Run `setup.sh` in worktree A; `bazel build
  //App` succeeds; confirm `.bazel-broker/bep.json` and `command.profile.gz` are produced.
  **Verify:** both files exist and are non-empty after a build.
- [ ] **T6. Profile opens in Perfetto.** Open A's `command.profile.gz` at `ui.perfetto.dev`;
  confirm a critical path renders. **Verify:** screenshot / manual confirm.
- [ ] **T7. Two-worktree cache-hit experiment.** Run `setup.sh` in worktree B (different path),
  then `measure.sh A B //App`. **Verify:** B's hit ratio is high (target ≥ ~80–90% of A's executed
  actions become hits; exact bar in §5). Record the *baseline-without-relocatability* number first
  (see T8) so the delta is attributable.
- [ ] **T8. Relocatability ablation + `aquery` proof (proves the flags matter AND fire).** Two parts:
  (a) `bazel aquery 'mnemonic("SwiftCompile|ObjcCompile|CppCompile", //App)'` and confirm the
  relocatability flags actually appear on the action command lines (single-dash `-file-prefix-map`
  on SwiftCompile from the feature; `-ffile-prefix-map` on Objc/Cpp; OSO/AST/xcode-remap at link).
  (b) Temporarily disable them (`--features=-swift.file_prefix_map,-oso_prefix_is_pwd,…` + comment the
  raw maps/strict-env/output-paths in B's rc), re-measure; expect a *markedly lower* hit ratio.
  Restore. **Verify:** flags present in (a); ratio drops without them and rises with them in (b) —
  this is the quantified evidence for §8.3 and settles OD-2/OD-2b empirically.
- [ ] **T9. Wire Make targets + CLAUDE recipe** (`make cache-setup`, `make cache-check`,
  `make cache-measure`). **Verify:** `make cache-check` is green in the repo's CI-style invocation.
  (Make target lives in this repo's root Makefile, created by E0; coordinate so E1 only *adds*
  targets.)

**Checkpoint gates:** T1–T4 gate "config is correct in isolation"; T5–T6 gate "single-worktree
profiling works"; T7–T8 gate the headline "cross-worktree cache hits with measured improvement."

---

## 4. Interfaces & contracts

These are the cross-epic contracts E1 **must hold stable** for E4 (and optionally E5).

| Contract | Value | Consumed by |
|---|---|---|
| **BEP json path** (per worktree, relative) | `<worktree>/.bazel-broker/bep.json` | **E4** tails this (`nxadm/tail`). ⚠ **CONTESTED** — E4 wants per-invocation (`<id>.bep.json`); see EC-1 / OD-1. One writer per worktree at a time (per-output-base lock, §2.4), but reuse → in-place truncation E4 must survive. |
| **Profile path** (per worktree, relative) | `<worktree>/.bazel-broker/command.profile.gz` | **E4** locates this to serve to Perfetto; **E6** `brokerctl profile <id>` opens it. Same per-invocation contention as the BEP path (EC-1). |
| **Disk cache location** | `$BAZEL_BROKER_CACHE/disk`, default `$HOME/.cache/bazel-broker/disk` | **E4** disk-cache size report + GC targets this dir; **E5** stagger reasons about it. Matches E4's "shared `--disk_cache` dir from config." ✅ consistent. |
| **Disk-cache GC flag** | E1 **may** also need to write `--experimental_disk_cache_gc_max_size=<N>G` if E4 chooses Bazel-native GC (E4 D-E4-4) | **E4** prefers driving Bazel's built-in GC, which requires this flag *in the rc* — i.e. E1's deliverable. **NEW contract, escalate EC-2:** confirm with E4 whether the GC flag belongs in E1's managed block (recommended: yes, gated behind a version check) or stays E4-side. |
| **Repository cache location** | `$BAZEL_BROKER_CACHE/repo`, default `$HOME/.cache/bazel-broker/repo` | informational; default-shared anyway. |
| **Relocatability features** | the `--features=…` set in §2.3 (pinned default-on) | **E4** low-hit alert (§2.7/D-E4-3) interprets a hit-ratio drop as path/env leakage — i.e. one of *these* features regressed. The feature list is the thing E4's "cache_busting_suspected" alert is implicitly about. |
| **Managed-block markers** | `# >>> bazel-broker E1 (managed) >>>` … `# <<< … <<<` | **E5** wrapper, if it injects the same flags at runtime, must detect/avoid double-injection vs this static block. |
| **Output base** | **untouched** — Bazel default (`output_user_root/md5(workspace)`) | invariant relied on by E2/E3 (per-worktree server discovery) and §2.2 rule; **also load-bearing for §2.4's clobber-safety argument** — E5's wrapper must not set `--output_base`. |
| **Env override** | `BAZEL_BROKER_CACHE` selects the cache root for setup.sh | E4/E5 read the same env to locate the cache. |

**Path-resolution contract:** all paths E1 writes are produced via `pwd -P` (symlinks resolved),
so the absolute paths E4 discovers from a process's cwd will string-match the BEP/profile file
locations. (Note: with the **feature-based** relocatability the embedded compiler path is `$PWD`→`.`,
not a `pwd -P` worktree literal, so `pwd -P` matters for *file discovery*, less so for the maps — §2.5.)

**E4 dependency note:** E4 (BEP ingest) is the *only* hard downstream dependency on E1. E1 finishes
first (M0 before M3). **Two contracts are currently in tension and must be locked with E4's owner
before E1 freezes:** the BEP/profile path lifecycle (EC-1 / OD-1) and the disk-cache GC flag
ownership (EC-2). Neither is resolved here.

---

## 5. Testing & verification

### 5.1 Repo-local (no iOS project, fast `/verify`)
- `bash -n setup.sh measure.sh` — syntax.
- `shellcheck setup.sh measure.sh` — lint (add to Make target).
- **setup.sh idempotency test:** temp dir + empty `MODULE.bazel`; run twice; assert exactly one
  managed block and absolute `--disk_cache` line present; assert `--check` round-trips
  (0 when current, 1 after a hand edit).
- **`.gitignore` test:** assert `.bazel-broker/` appended exactly once.

### 5.2 Two-worktree cache-hit experiment (the headline test)
1. Create worktrees A and B of the iOS project at distinct paths (`git worktree add`).
2. `setup.sh` in each (per-worktree prefix-map root).
3. `measure.sh A B //path/to:iosApp`.
4. **Pass:** B's reported disk-cache hit ratio is high and B's wall time is a large fraction lower
   than A's cold build. **Acceptance bar:** ≥ ~80% of A's executed compile/link actions become
   disk-cache hits in B for an unchanged tree (exact number recorded as the project baseline in T7;
   the *relative* improvement over the no-relocatability ablation T8 is the real proof). The
   architecture's "Done when" only requires a "high cache-hit ratio" — record the measured figure.
5. Cross-check B's ratio against Bazel's own `INFO: ... processes: ... disk cache hit` summary;
   they should agree (validates that the BEP number E4 will use is trustworthy).

### 5.3 Profile / Perfetto
- Open `<A>/.bazel-broker/command.profile.gz` in `ui.perfetto.dev`; confirm the trace loads and a
  **critical path** track is present. (Matches E4's Perfetto deep-link expectation.)

### 5.4 Acceptance criteria (from EPIC 1 "Done when")
- [ ] Building the same target in two worktrees → **second build shows high cache-hit ratio** in BEP
  `BuildMetrics`. *(T7, §5.2)*
- [ ] **Profile `.gz` opens in Perfetto with a critical path.** *(T6, §5.3)*
- [ ] Deliverables present: `.bazelrc` fragment + `setup.sh` + cache doc. *(T2–T4)*
- [ ] (Engineering bar, not in the original "Done when" but required to trust it) **ablation shows
  relocatability flags materially raise the hit ratio.** *(T8)*

---

## 6. Risks, edge cases, open decisions

### Risks & edge cases
- **R1. rules_apple / rules_swift ALREADY ship the relocatability features — design around them, do
  not duplicate.** (Upgraded from "may inject" to "do inject" after review.) apple_support provides
  `oso_prefix_is_pwd`/`relative_ast_path`/`remap_xcode_path`; rules_swift provides
  `swift.file_prefix_map`/`swift.coverage_prefix_map`/`swift.cacheable_swiftmodules`/
  `swift.use_global_module_cache` — **default-on** in recent versions. The v1 plan re-implemented a
  subset by hand via raw `--copt/--swiftcopt` maps, one of which (`--swiftcopt=-ffile-prefix-map`)
  was an *incorrect spelling* that would silently no-op. Mitigation: features-first design (§2.5),
  T0 version check, T8 `aquery` proof that the features fire, raw clang maps only as a verified
  C/C++/ObjC supplement. *(OD-2, OD-2b)*
- **R2. `--incompatible_strict_action_env` can break builds** that rely on a leaked env var
  (a tool found via the user's `PATH`, locale, code-signing helpers). **iOS clarification (review):**
  `DEVELOPER_DIR`/`SDKROOT` are set by the apple_support toolchain + `remap_xcode_path`, *not*
  inherited from the client env, so strict mode does **not** strip the Xcode env and is usually safe
  for the Apple compile/link path — the real breakage is **non-hermetic tools on `PATH`** (homebrew,
  a system Python — cf. bazelbuild/bazel#8536). Mitigation: re-admit the *specific* missing var with
  `--action_env=NAME` rather than dropping strict mode; document the failure signature in README.
  *(OD-6)*
- **R3. Path maps can break debugging / source navigation** in lldb/Xcode if the debugger can't
  re-map `.` back to the worktree. The features use the same `$PWD`→`.` convention, so this risk
  applies to features and raw maps alike. Mapping to `.` keeps paths resolvable from the worktree
  root; if Xcode debugging suffers, the apple_support/rules_swift features are the better lever
  (they coordinate with dSYM/lldb), and an lldb `target.source-map` can patch the rest. Surface,
  don't silently choose. *(OD-3)*
- **R4. Symlinked worktrees / `/private/var` aliasing on macOS** could make the prefix-map root not
  match the path clang sees → cold cache. Mitigated by `pwd -P` (§2.6); still verify in T7 that the
  map actually fires (T8 ablation will reveal a non-firing map as "no improvement").
- **R5. Disk cache grows unbounded.** Shared cache across many worktrees/agents fills the disk.
  Out of scope for E1 (GC is E4 §8.4), but the README must warn and point at E4; until E4 ships,
  document a manual `rm -rf` / size-check.
- **R6. Version-specific flag drift (Bazel 9.x).** `--experimental_output_paths` is still
  `experimental`-prefixed and could be renamed/removed. Pin the exact Bazelisk-resolved version in
  the README and gate behind `--check`. (Note: `--build_event_json_file_path_conversion` was dropped
  from the fragment in v2 — it was misused; see §2.3.) The relocatability **features** can also be
  renamed across apple_support/rules_swift majors — T0 pins those versions too. *(OD-5)*
- **R7. Concurrent first-touch of an empty disk cache (thundering herd).** If A and B start the same
  action simultaneously, *both* miss (neither has written yet). Not solvable by config — it's E5's
  admission stagger. E1 just notes it so the measured hit ratio in T7 is taken with builds run
  *sequentially* (cold-A-then-warm-B), not simultaneously.
- **R8. `bazel clean` in measure.sh** drops more than intended if someone swaps in `--expunge`.
  measure.sh pins plain `clean`; documented.

### Open decisions (surface, do not resolve here)
- **OD-1. BEP/profile file lifecycle:** single per-worktree relative file (E1's current choice) vs
  per-invocation files. **Now elevated to a cross-epic conflict — see EC-1** (E4 actively recommends
  per-invocation). Owner: **E1↔E4** jointly. §2.4.
- **OD-2. Relocatability via rules features vs raw maps:** confirm apple_support/rules_swift
  versions ship the features default-on (T0) and that they fire (`aquery`, T8); decide whether the
  raw clang maps stay as a supplement or are dropped. §2.5 / R1.
- **OD-2b. Raw clang map LHS correctness under sandboxing:** does `=<WORKTREE_ABS>=.` match the path
  the compiler actually embeds (which may be the exec-root path, not the worktree path)? If not,
  rely on features and drop the raw maps. §2.5.
- **OD-3. Path-map target value:** `=.` (relative, default — used by both features and raw maps) vs a
  fixed synthetic root (better for lldb). Depends on whether iOS debugging breaks. R3.
- **OD-4. Exact BEP `actionSummary` field names/casing on Bazel 9.x** (`protojson` → lowerCamelCase
  expected). measure.sh's `jq` keys are hardcoded to camelCase — confirm during T7. §2.7.
- **OD-5. Experimental/version-specific flags** (`--experimental_output_paths=strip`; the
  relocatability feature names) — confirm presence/spelling on the pinned versions. R6.
- **OD-6. strict-action-env allowlist** for iOS — which non-hermetic `PATH` tools (if any) must be
  re-admitted via `--action_env`. R2.
- **OD-7. Cache location convention** — `$HOME/.cache/bazel-broker` (chosen default) vs a more
  visible path; must match whatever E4's GC and any docs assume. §4. Confirm with E4 owner.

### Escalations (cross-epic — must be locked before E1 freezes)
- **EC-1. BEP/profile path lifecycle contract (E1↔E4).** E1 emits a per-*worktree* static relative
  path; E4 (§2.2/§4/D-E4-2) *recommends per-invocation* and otherwise must build a truncation
  supervisor. Pick one owner/shape. Recommendation: keep E1 static for the no-wrapper M0/M3, E4 owns
  the supervisor; revisit when E5's wrapper lands. §2.4.
- **EC-2. Disk-cache GC flag ownership (E1↔E4).** If E4 drives Bazel-native GC
  (`--experimental_disk_cache_gc_max_size`, its D-E4-4 recommendation), that flag must live in E1's
  managed rc block. Decide whether E1 writes it (gated on version) or E4 injects it elsewhere. §4.

> Per project constraint, none of OD-1..7 / EC-1..2 is unilaterally resolved here; each names an
> owning epic or a verification step that settles it.

---

## 7. Effort & internal ordering

**Total: ~1–1.5 days**, dominated by real-iOS-project verification, not authoring.

| Phase | Tasks | Effort | Blockers |
|---|---|---|---|
| 0. Version pin (gates the design) | T0 | ~15 m | needs the iOS project's MODULE graph |
| A. Config authoring (repo-local) | T1–T4 | ~3–4 h | T0 (informs which relocatability flags to write) |
| B. Single-worktree apply + Perfetto | T5–T6 | ~1–2 h | needs a real iOS workspace + Bazel 9.x toolchain |
| C. Two-worktree measurement + ablation | T7–T8 | ~3–4 h | needs ≥2 worktrees; settles OD-2/2b/3/4/5/6 empirically |
| D. Make/CLAUDE wiring | T9 | ~30 m | E0's root Makefile must exist (E1 only adds targets) |

**Internal ordering rationale:** do **T0 first** (it decides whether the relocatability lever is
"assert default-on features" or "enable features + hand-roll maps"). Then Phase A, before touching
an iOS project (cheap, fast `/verify`, no external deps). Phase B is the smoke test that the rc is
even valid. Phase C is where all the empirical open decisions (OD-2/2b/3/4/5/6) get answered — budget
the most time here, since relocatability with rules_apple/rules_swift is the genuine unknown. Phase D
is trivial glue.

**Lock before freezing E1:** EC-1 (BEP/profile path lifecycle) and EC-2 (GC flag ownership) with
E4's owner — both materially change E4's task 4 complexity and E1's rc content.

**Cross-epic placement:** E1 is on the critical path *only* for E4 (which depends on E1's BEP/path
contracts). It is otherwise parallelizable with E2/E3. Land E1 first (architecture M0) so the cache
win is real before any daemon code, and so E4 has a stable contract to build against.

---

## Staff Engineer Review

*Reviewer pass on Draft v1 → revised in place as Draft v2. Verdict + findings + change log + escalations.*

### (a) Verdict

**Approve with required changes — do not implement v1 as written.** The architecture is sound
(disk-cache sharing + relocatability + per-build profiling, output base untouched), the structure is
excellent, and the "surface decisions, don't resolve" discipline is exactly right. **But v1 contained
one ship-blocking correctness bug and one mis-framed relocatability strategy that would have produced
a cache that *looks* configured yet stays cold.** Those are fixed in v2. The headline cross-epic
contract (BEP path) is in active conflict with E4 and must be locked before either epic freezes.
Net: green to start **T0 + Phase A** immediately; **gate Phase B/C on the relocatability rewrite**
landing (it has).

### (b) Top findings

1. **[Blocker, fixed] `--swiftcopt=-ffile-prefix-map=<ABS>=.` is the wrong flag spelling.** `swiftc`
   does not accept clang's `-ffile-prefix-map`; it uses single-dash `-file-prefix-map` /
   `-debug-prefix-map` / `-coverage-prefix-map`. v1 would silently no-op on the Swift compile path —
   i.e. Swift actions stay path-keyed → cold cross-worktree → the epic's headline win quietly fails
   for the largest part of an iOS build. Removed; replaced with the rules_swift feature.
2. **[Major, fixed] Relocatability was hand-rolled instead of using the idiomatic features.** The
   Bazel Apple ecosystem already ships `oso_prefix_is_pwd`, `relative_ast_path`, `remap_xcode_path`
   (apple_support) and `swift.file_prefix_map`, `swift.coverage_prefix_map`,
   `swift.cacheable_swiftmodules`, `swift.use_global_module_cache` (rules_swift), **default-on** in
   recent versions — this is how shops actually get cross-machine cache hits. They also cover OSO/AST/
   Xcode-path cases that raw `--copt` maps miss. v2 makes features the primary lever, raw clang maps a
   verified C/C++/ObjC supplement.
3. **[Correctness, fixed] `--build_event_json_file_path_conversion=no` was misdescribed and misused.**
   It does not compact the stream or stabilize tailing (its v1 rationale); it controls `file://` URI
   rewriting of paths *inside* BEP content (default `true`). Dropped from E1; whether E4 wants plain
   vs `file://` paths inside events is E4's call.
4. **[strict-action-env / iOS] Clarified, not as scary as feared.** `DEVELOPER_DIR`/`SDKROOT` come
   from the apple_support toolchain (+ `remap_xcode_path`), not the client env, so strict mode does
   not strip them. Real risk is non-hermetic tools on `PATH` → re-admit specific vars with
   `--action_env`. Kept the flag; sharpened the caveat (R2/OD-6).
5. **[Cross-epic conflict, escalated] E1↔E4 BEP/profile path contract.** E1 picks a per-*worktree*
   static path; E4 *recommends per-invocation* and otherwise must build a truncation supervisor.
   This is a genuine open decision, not settled — elevated to **EC-1**.
6. **[Measurement] The hit metric proxy understates and can mis-key.** `1 - executed/created`
   includes non-cacheable internal actions and the `jq` selector is hardcoded camelCase. Made the
   Bazel summary-line `disk cache hit` count the authoritative figure (aligned with E4 D-E4-3);
   proxy is now a labelled secondary check.
7. **[Minor] §2.4 clobber-safety argument is correct but conditional** on E1 never setting
   `--output_base`/`--output_user_root`; added the scope caveat and reinforced it in the §4 contract
   table so E5's wrapper can't quietly invalidate it.

### (c) What I changed

- **§Scope + §2.3 + §2.5:** rewrote relocatability to features-first; removed the bad
  `--swiftcopt=-ffile-prefix-map`; restricted raw maps to `--copt`/`--objccopt`; added the
  `--features=…` block and the swiftc-spelling correction box.
- **§2.3:** dropped `--build_event_json_file_path_conversion=no` with an explanation of why it was
  wrong; added an iOS note on strict-action-env + Xcode env.
- **§2.4:** added the output-base scope caveat; rewrote OD-1 into an explicit E1↔E4 conflict (EC-1)
  with options + recommendation.
- **§2.5:** new feature table; the swiftc-spelling correction; the "does the map LHS even match under
  sandboxing" subtlety (OD-2b); sharpened R2/iOS env wording.
- **§2.6 setup.sh:** removed the bad swiftcopt line; commented why Swift relocatability lives in the
  fragment’s feature, not here.
- **§2.7 measure.sh:** added the metric caveat (authoritative = summary line) and the `jq` casing
  correction.
- **§3:** added **T0** (version pin) as the gating task; strengthened **T8** with an `aquery` proof
  that the flags actually fire.
- **§4:** marked the BEP/profile path contracts CONTESTED; added the disk-cache GC-flag contract
  (EC-2) and a relocatability-features contract row; reinforced the output-base invariant.
- **§6:** upgraded R1 ("do inject" not "may"); clarified R2/R3/R6; added OD-2b; added the
  **Escalations (EC-1, EC-2)** subsection.
- **§7:** added T0 to the effort table and the "lock before freeze" note.

All verified against current Bazel 9.x / rules_swift / apple_support / BuildBuddy docs (see review
notes); the flag spellings and feature names are confirmed, not assumed.

### (d) Decisions / risks to escalate

- **EC-1 (highest priority): BEP/profile path lifecycle — E1↔E4.** Per-worktree static (E1, needs
  E4 truncation supervisor) vs per-invocation (E4's preference, needs E5 wrapper → reintroduces a
  dependency E1 was meant to avoid). **Recommendation:** stay static for M0/M3, E4 owns the
  supervisor it already specced, switch to per-invocation once E5 lands. Owner sign-off required
  before E1 freezes.
- **EC-2: disk-cache GC flag ownership — E1↔E4.** If E4 drives Bazel-native GC
  (`--experimental_disk_cache_gc_max_size`), the flag belongs in E1's managed rc block. Decide who
  writes it. Recommendation: E1 writes it, gated behind a version check.
- **OD-2 / OD-2b (empirical, settles in T8):** confirm the relocatability features fire and that any
  retained raw clang map's LHS matches the path the compiler actually embeds under the project's
  sandbox mode; if not, drop the raw maps and rely on features.
- **OD-6: strict-action-env allowlist** — discover (only on a real build) which non-hermetic PATH
  tools, if any, must be re-admitted. Could in the worst case force `--action_env` additions that
  slightly reduce cross-machine (not cross-worktree) reuse — acceptable for this single-Mac scope.
- **Residual risk:** the entire relocatability win is contingent on the apple_support/rules_swift
  version assumption (T0). If the project pins old versions, Phase C effort rises and the manual maps
  become load-bearing (and harder to get right). Flag to the owner at T0, not at T8.
