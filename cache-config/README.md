# bazel-broker E1 — cross-worktree cache sharing & profiling (config only)

This directory makes Bazel builds run from **different git worktrees** share a single
on-disk cache, so work done in worktree A becomes a cache **hit** in worktree B, and makes
every build emit a Perfetto-openable timing profile — **with zero application code**. It is
pure config: a `.bazelrc` fragment, a `setup.sh` that writes the absolute-path lines into a
target workspace, a `measure.sh` that proves the cross-worktree hit ratio, and this doc.

```
cache-config/
  bazelrc.fragment   # static, path-independent flags (committed verbatim)
  setup.sh           # writes the absolute-path lines into a workspace's .bazelrc
  measure.sh         # two-location cache-hit experiment (cold A → warm B)
  README.md          # this doc
```

## Quick start

```bash
# In each worktree of your iOS project (re-run per worktree — see "Why per worktree"):
/path/to/bazel-broker/cache-config/setup.sh .            # uses $HOME/.cache/bazel-broker
# or pick the shared cache location explicitly:
/path/to/bazel-broker/cache-config/setup.sh . --cache-dir /Volumes/fast/bb-cache

# Prove cross-worktree hits (cold build in A populates the cache, warm build in B hits it):
/path/to/bazel-broker/cache-config/measure.sh ../worktree-a ../worktree-b //:YourApp

# Drift check (CI / Makefile): exit 0 if the managed block is current, 1 otherwise:
/path/to/bazel-broker/cache-config/setup.sh . --check
```

`setup.sh` appends an idempotent **managed block** (between `# >>> bazel-broker E1 (managed) >>>`
and `# <<< ... <<<` markers) to the workspace's `.bazelrc`, creates the shared cache dirs and the
per-worktree `.bazel-broker/` output dir, and adds `.bazel-broker/` to the workspace `.gitignore`.
Re-running rewrites the block in place (never duplicates).

---

## The cardinal rule: share the `--disk_cache`, NEVER share the output base

Bazel keys a long-lived JVM **server** by its **output base** =
`output_user_root / md5(workspace_path)`, and holds an **exclusive lock per output base** — only
one command runs per server at a time.

- **Different worktree path → different output base → its own server, lock, and local cache.**
  That is what lets two worktrees build genuinely in parallel.
- If you forced every worktree onto the **same** output base (by setting `--output_base` /
  `--output_user_root`), they would all contend on **one** lock and **serialize** — the exact
  opposite of the goal.

So the lever is the **disk cache** (`--disk_cache`, a plain directory) which is a shared
content-addressed action cache: worktree A's executed action result becomes a **hit** for worktree
B whenever the action hash matches. **E1 never touches the output base.** This invariant is also
what makes the relative BEP/profile paths collision-safe (see "Why relative paths are safe").

### The three cache layers

| Layer | Scope | What E1 does |
|---|---|---|
| Analysis cache (Skyframe, in-memory) | per server / output base | **cannot** share — leave alone (warm-server tuning is a later epic) |
| Local action cache | per output base | superseded by the disk cache; nothing to do |
| **Disk cache** (`--disk_cache`) | a directory | **share** — the main lever (`setup.sh` writes an absolute path) |
| Repository cache (`--repository_cache`) | per-user | already shared by default; set explicitly as belt-and-suspenders |

---

## Relocatability — making the shared cache *actually* hit

The disk cache is content-addressed: B hits A's result only when the **action key** matches. The
key hashes the command line, input file paths, environment, and output paths. Three things bust it
across worktrees even for byte-identical source — and each has a fix in the fragment:

1. **Absolute worktree paths leak into command lines / debug info.** `clang`/`swiftc` embed the
   source's absolute path in object files, DWARF, `.swiftmodule`, OSO/AST sections.
   `/wt/feature-a/Foo.swift` ≠ `/wt/feature-b/Foo.swift` → different key → miss.
2. **Environment variation** (PATH, locale, agent-specific vars) busts the env component.
3. **Config-specific output path segments** (`bazel-out/<cfg>/...`) can differ.

### FEATURES first, raw clang maps second (the important part)

The Bazel Apple ecosystem already solves relocatability with **toolchain-aware features** that emit
the correct per-compiler flags (including OSO/AST/Xcode-path cases that raw `--copt` maps do not
cover). These are **default-on** in apple_support ≥ ~1.5 and rules_swift ≥ ~1.5; the fragment pins
them with `--features=` as a guard + documentation.

| Feature | Module | Fixes |
|---|---|---|
| `oso_prefix_is_pwd` | apple_support | rewrites the OSO (debug-map) absolute prefix in linked binaries to `.` |
| `relative_ast_path` | apple_support | makes embedded AST paths relative |
| `remap_xcode_path` | apple_support | replaces the absolute Xcode/DEVELOPER_DIR path with a fixed token |
| `swift.file_prefix_map` | rules_swift | emits swiftc's **single-dash** `-file-prefix-map` (`$PWD`→`.`) — the *correct* Swift spelling |
| `swift.coverage_prefix_map` | rules_swift | same for `-coverage-prefix-map` |
| `swift.cacheable_swiftmodules` | rules_swift | strips absolute paths embedded in `.swiftmodule` |
| `swift.use_global_module_cache` | rules_swift | fixed module-cache location so it is reused, not path-keyed |

> **Why NOT `--swiftcopt=-ffile-prefix-map` (a real bug we corrected):** `swiftc` does **not**
> accept clang's `-ffile-prefix-map` spelling. The Swift frontend uses single-dash
> `-file-prefix-map` / `-debug-prefix-map` / `-coverage-prefix-map`. Passing the clang spelling via
> `--swiftcopt` errors or is silently dropped — a cache that *looks* configured yet stays cold for
> the largest part of an iOS build. The `swift.file_prefix_map` feature emits the correct flag.
> Verified empirically via `aquery` on the fixture: SwiftCompile actions carry `-file-prefix-map`
> and `-file-prefix-pwd-is-dot`, never `--swiftcopt=-ffile-prefix-map`.

### Raw clang maps — supplement for the C/C++/ObjC compile path only

`setup.sh` additionally writes, for the plain C/C++/ObjC clang path (which is not driven by a Swift
feature), per-worktree:

```
build --copt=-ffile-prefix-map=<WORKTREE_ABS>=.
build --copt=-fdebug-prefix-map=<WORKTREE_ABS>=.
build --objccopt=-ffile-prefix-map=<WORKTREE_ABS>=.
build --objccopt=-fdebug-prefix-map=<WORKTREE_ABS>=.
```

These map to `.` (not empty) so paths stay resolvable as `./Foo.m` for lldb/Xcode. They are
**per-worktree** (the LHS is this worktree's absolute root), which is exactly why `setup.sh` must be
re-run in each worktree.

### The other two levers

- `--incompatible_strict_action_env` pins a static action env so it does not vary with each shell's
  `$PATH`. **iOS note:** `DEVELOPER_DIR`/`SDKROOT` come from the apple_support toolchain (+
  `remap_xcode_path`), *not* the client env, so strict mode does not strip them and is safe for the
  Apple compile/link path. The genuine risk is a build action that depends on a **non-hermetic tool
  found on `PATH`** (a homebrew tool, a system Python). If a build breaks under strict mode, re-admit
  the *specific* var with `--action_env=NAME` rather than dropping strict mode. Under strict mode
  PATH defaults to `/bin:/usr/bin`.
- `--experimental_output_paths=strip` strips config-specific segments from output paths in action
  command lines. Still `experimental`-prefixed on Bazel 8.x/9.x — confirm the spelling if you pin a
  newer Bazel.

---

## Why relative `--profile` / `--build_event_json_file` paths are collision-safe

The fragment uses **relative** paths:

```
build --profile=.bazel-broker/command.profile.gz
build --build_event_json_file=.bazel-broker/bep.json
```

- A relative path resolves against the **workspace root** → `/wt/feature-a/.bazel-broker/bep.json`
  in A, `/wt/feature-b/.bazel-broker/bep.json` in B. **Different worktrees → different files.** No
  cross-worktree clobber.
- *Within* one worktree, two concurrent `bazel build`s cannot both write `bep.json`: all builds from
  one worktree share one output base → one server → the **exclusive per-output-base lock** runs at
  most one command at a time. A second build blocks (or fails fast with `--block_for_lock=false`).
  So there is never more than one writer to a given worktree's `bep.json`. **This safety depends on
  E1 never setting `--output_base`/`--output_user_root`** (the cardinal rule).
- An **absolute static** path (e.g. `/tmp/bep.json` in a shared rc) *would* clobber across concurrent
  worktrees — different output bases build in genuine parallel and write the same file at once.
  Relative paths sidestep that without minting per-invocation filenames.

> **Cross-epic contract (consolidated-review C7 / P4):** the BEP path string is locked to exactly
> `.bazel-broker/bep.json` (workspace-relative). The broker's BEP tailer (E4) relies on this exact
> string. The same worktree **reuses** (in-place truncates) it on each rebuild; E4 owns a truncation
> supervisor for that. Bazel does **not** create `.bazel-broker/` itself — `setup.sh` creates it (and
> `.gitignore`s it).

---

## Measuring the hit ratio — the binding definition

`measure.sh` runs a cold build in A (populating the shared cache) then the same target in B, and
reports B's cross-worktree disk-cache hit ratio.

The hit ratio is derived from Bazel's **`ActionSummary.runner_count[]`** — the structured form of
the end-of-build summary line `INFO: N processes: X disk cache hit, Y internal, Z worker`. This is
the **same number E4 computes** (consolidated-review C8 / P4):

```
disk_hits        = runner_count[name == "disk cache hit"].count
executed_runners = Σ runner_count[i].count  for names that LOCALLY EXECUTED an action,
                   excluding "total" (synthetic sum), "internal" (bookkeeping/symlink
                   actions that never touch the disk cache), and "disk cache hit" itself
                   (so the remainder is e.g. "worker", "darwin-sandbox", "local").
hit_ratio        = disk_hits / (disk_hits + executed_runners)
```

> `actions_created - actions_executed` is **WRONG** and is not used: a disk-cache hit is counted
> *inside* `actions_executed`, so that formula understates/inverts the real hit rate.

`measure.sh` prints the runner_count-derived ratio **and** the raw `N processes:` summary line as a
casing-independent cross-check; they agree by construction.

### Measured on the fixture (`testdata/ios-app`, `//:BrokerDemo`, Bazel 8.3.1)

| Scenario | Summary line | Hit ratio |
|---|---|---|
| Cold build in A (populates cache) | `108 processes: 69 internal, 34 darwin-sandbox, 1 local, 4 worker` | 0% (cold) |
| **Warm build in B** (different path, relocatability ON) | `108 processes: 35 disk cache hit, 69 internal, 4 worker` | **89.7% (35 / 39)** |
| Ablation in C (different path, relocatability OFF) | `108 processes: 69 internal, 34 darwin-sandbox, 1 local, 4 worker` | **0%** |

The ablation is the proof that relocatability is the lever: with the features/maps off, the
different worktree path busts every action key and B re-executes everything; with them on, 35 of the
39 executable actions are served from A's results across a different path. (The 4 `worker` actions
are the persistent-worker Swift module compiles that always run locally; they are the non-hit
remainder of the denominator.)

---

## Operational notes & caveats

- **Why re-run `setup.sh` per worktree:** the disk/repo cache is shared (same `--cache-dir`), but the
  raw clang prefix-map LHS is **this worktree's absolute path**, resolved with `pwd -P` (so symlinked
  worktrees and `/var`→`/private/var` aliasing match what clang sees). Swift relocatability is
  path-independent (the feature maps the action's real `$PWD`), so Swift hits even without re-running;
  the C/C++/ObjC maps are what need the per-worktree value.
- **Disk cache grows unbounded.** The shared cache across many worktrees/agents fills the disk.
  GC/size-cap is a later epic (E4). Until then, prune manually: `rm -rf "$BAZEL_BROKER_CACHE/disk"`
  (default `$HOME/.cache/bazel-broker/disk`), or check size with `du -sh`.
- **Thundering herd.** If A and B start the *same* action simultaneously against an empty cache, both
  miss (neither has written yet). Not solvable by config — measure with builds run **sequentially**
  (cold A, then warm B), which is what `measure.sh` does. Admission stagger is a later epic (E5).
- **Bazel version.** Validated on Bazel **8.3.1**, apple_support **2.6.1**, rules_apple **4.5.3**,
  rules_swift **3.6.1** (all ≥ ~1.5, so the relocatability features are default-on). The
  `--experimental_output_paths` flag and the feature names can drift across majors — pin your Bazel
  via `.bazelversion`/Bazelisk and re-check with `setup.sh --check`.
- **Repository cache** (`--repository_cache`) is shared by default; setting it explicitly is
  belt-and-suspenders so all worktrees share one fetched-external-deps store.
