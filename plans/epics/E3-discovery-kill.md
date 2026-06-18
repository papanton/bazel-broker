# E3 — Process discovery & kill

> ⚠️ **Superseded where it conflicts with [`00-consolidated-review.md`](00-consolidated-review.md) + E2 §4 (frozen contract).** Conform before coding:
> - **P2:** kill route is `POST /builds/{invocation_id}/kill` (not `POST /kill`).
> - **OD-A:** reconcile kill signal/exit-code with E0 fake-bazel — a backgrounded stub ignores SIGINT and dies by SIGKILL (137), not the graceful code 8; the test must accept 8 **or** 137 within grace.
> - User decisions: **D-stack-1** (cgo vs shell-out), **D4** (SIGINT vs command-server Cancel), **OD-D** (spike proc-cwd `EPERM` under the hardened runtime in T1 — gates the no-wrapper value prop).

> Executable implementation plan for one epic of **Bazel Broker**.
> See and stop running builds with **no wrapper required**.

Status: **Draft v1** · Owner: Antonis · Last updated: 2026-06-17
Depends on: **E2** (broker daemon core: registry + HTTP/WS API + SQLite store).
Maps to architecture milestone **M2**. Target: **macOS arm64, Go 1.26, cgo enabled**.

---

## 1. Goal & scope recap

**Goal (from 02-epics E3):** deliver the *passive* control path (architecture §4/§7) —
the broker can see every running Bazel build and kill any one of them **without** the
`tools/bazel` wrapper. This is the half of the value matrix (§7) that needs no interception:
*trace* and *kill* both come from process discovery alone.

**In scope (deliverables):**
1. **Discovery module** — macOS `libproc` via cgo: enumerate `bazel`/`bazelisk` **client**
   PIDs, resolve each PID's executable path and current working directory.
2. **PID→cwd→worktree mapping** — walk up from the cwd to the git worktree root (`.git`
   file or dir), resolve the real worktree path/name.
3. **Registry reconciliation** — merge discovered processes into the E2 registry, deduping
   by `(pid, invocation_id)`; reconcile against `Register`'d builds without clobbering them.
4. **Kill module** — `Kill(invocation_id | pid)`: `SIGINT` → grace timeout → `SIGKILL`
   state machine; **optional** out-of-band command-server gRPC `Cancel`
   (`<output_base>/server/command_port`).
5. **API:** `POST /kill` added to the E2 HTTP API.

**Out of scope (other epics):** admission / queueing / wrapper (E5), BEP ingest & metrics
(E4), CLI `kill` subcommand wiring (E6 — this epic delivers the *endpoint*, E6 the verb),
cache (E1). We rely **only** on E2's registry, store, and HTTP server.

**Done when (acceptance, verbatim from epic):** launch fake-bazel (long duration) in a
worktree → it appears in `ls` with the correct worktree → `kill <id>` makes it exit with
the cancel code in <1s.

**Open decisions this epic surfaces but does NOT unilaterally resolve:** **D-stack-1**
(cgo+libproc vs shell-out to `lsof`/`ps`) and **D4** (owned-PID `SIGINT` vs command-server
`Cancel`). See §6. The plan is structured so D-stack-1 is recommended-but-swappable behind
an interface, and D4 ships SIGINT first with `Cancel` behind a feature flag.

---

## 2. Design & implementation details

### 2.0 Package layout

```
internal/
  procscan/                 # D-stack-1: process discovery (cgo libproc)
    libproc_darwin.go       # cgo wrapper: signatures + bindings (build tag darwin,arm64/amd64)
    libproc_darwin_test.go
    scanner.go              # pure-Go: ProcInfo, Scanner interface, filtering bazel clients
    scanner_stub.go         # build tag !darwin → returns ErrUnsupported (keeps non-mac CI green)
  worktree/
    resolve.go              # cwd → git worktree root + name
    resolve_test.go
  reconcile/
    reconcile.go            # discovered ProcInfo ⨝ registry → merged Build set
    reconcile_test.go
  killer/
    killer.go               # SIGINT→grace→SIGKILL state machine
    cancel_darwin.go        # OPTIONAL command-server Cancel (D4), behind flag
    killer_test.go
  registry/                 # OWNED BY E2 — we add fields/methods, do not re-create
```

The cgo lives **only** in `procscan/libproc_darwin.go`. Everything else is pure Go and
testable on any platform (the scanner is injected as an interface, so reconcile/killer tests
need no Mac syscalls).

### 2.1 The cgo libproc wrapper — C signatures & Go bindings (D-stack-1)

`libproc` is the system library exposed via `<libproc.h>`. **There is no `libproc.dylib` to
link** — the symbols are exported from `libSystem` (the standard C library), so `-lproc` is
unnecessary and on recent toolchains is *harmless but pointless*; we drop it (see the shim
below). We need three calls (signatures verified against the macOS 15/26 SDK
`usr/include/{libproc.h,sys/proc_info.h}`):

| C function | Verified signature | Purpose |
|---|---|---|
| `proc_listpids` | `int proc_listpids(uint32_t type, uint32_t typeinfo, void *buffer, int buffersize)` | enumerate all PIDs (`type = PROC_ALL_PIDS = 1`; `typeinfo` ignored for ALL_PIDS, pass 0). Returns **bytes** written (or a size estimate when `buffer == NULL`), `< 0` on error. |
| `proc_pidpath`  | `int proc_pidpath(int pid, void *buffer, uint32_t buffersize)` | absolute executable path. **Apple requires `buffersize >= PROC_PIDPATHINFO_MAXSIZE` (`4*MAXPATHLEN` = 4096)** — a smaller buffer can yield `0`/`ENOMEM`. Returns path **length** on success, `<= 0` on error. |
| `proc_pidinfo`  | `int proc_pidinfo(int pid, int flavor, uint64_t arg, void *buffer, int buffersize)` | per-pid info; `flavor = PROC_PIDVNODEPATHINFO = 9` → `struct proc_vnodepathinfo` whose `pvi_cdir.vip_path` is the **cwd**. Returns **bytes copied** (== `sizeof(struct proc_vnodepathinfo)` on success), `<= 0` on error. |

> **Buffer-size correction.** The previous draft sized the exe-path buffer at `MAXPATHLEN`
> (1024). That is the documented size for `proc_pidpath`'s *result* but **not** the size Apple
> requires you to *pass*: the call wants `PROC_PIDPATHINFO_MAXSIZE` (4096) or it can fail. The
> cwd buffer (`vip_path`) is genuinely `MAXPATHLEN` (1024) — `vip_path` is declared
> `char[MAXPATHLEN]` in the SDK — so 1024 is correct *there only*. The two buffers have
> different sizes; do not unify them.

```go
//go:build darwin

package procscan

/*
// No "#cgo LDFLAGS: -lproc": libproc's symbols live in libSystem, which is linked
// automatically. Adding -lproc is unnecessary (and there is no libproc.dylib to find).
#include <libproc.h>
#include <sys/proc_info.h>
#include <sys/errno.h>
#include <stdlib.h>
#include <string.h>

// Thin C shims. Each shim returns the libproc result AND captures errno into *err so the
// Go side can distinguish EPERM (alive but unreadable -> skip) from ESRCH (gone -> reap)
// from a genuine failure. Collapsing both to -1 (as the previous draft did) loses that
// distinction, which the kill state machine and the reaper actually need.

// Enumerate PIDs. Returns bytes written (or <0 on error); errno -> *err.
static int bb_list_all_pids(int *pids, int cap_pids, int *err) {
    *err = 0;
    int n = proc_listpids(PROC_ALL_PIDS, 0, (void *)pids, cap_pids * (int)sizeof(int));
    if (n <= 0) *err = errno;
    return n;
}

// Size probe: bytes the kernel would write (pass NULL buffer). errno -> *err.
static int bb_count_all_pids(int *err) {
    *err = 0;
    int n = proc_listpids(PROC_ALL_PIDS, 0, NULL, 0);
    if (n <= 0) *err = errno;
    return n; // bytes; divide by sizeof(int) on the Go side
}

// Executable path. Caller MUST pass bufsize >= PROC_PIDPATHINFO_MAXSIZE (4096).
// Returns length on success, <=0 on error; errno -> *err.
static int bb_pid_path(int pid, char *buf, int bufsize, int *err) {
    *err = 0;
    int n = proc_pidpath(pid, buf, (uint32_t)bufsize);
    if (n <= 0) *err = errno;
    return n;
}

// Current working directory via PROC_PIDVNODEPATHINFO -> pvi_cdir.vip_path.
// On success copies the NUL-terminated cwd into out and returns 0; on error returns -1
// and sets *err to errno. proc_pidinfo for this flavor returns sizeof(struct
// proc_vnodepathinfo) exactly on success, so we require == (not >=).
static int bb_pid_cwd(int pid, char *out, int outsize, int *err) {
    struct proc_vnodepathinfo vpi;
    *err = 0;
    memset(&vpi, 0, sizeof(vpi));
    int ret = proc_pidinfo(pid, PROC_PIDVNODEPATHINFO, 0, &vpi, (int)sizeof(vpi));
    if (ret != (int)sizeof(vpi)) {
        *err = errno;          // EPERM (not readable), ESRCH (gone), or short copy
        if (*err == 0) *err = EINVAL; // short copy with errno unset -> synthesize
        return -1;
    }
    // vip_path is declared char[MAXPATHLEN] (1024); strlcpy NUL-terminates within outsize.
    strlcpy(out, vpi.pvi_cdir.vip_path, (size_t)outsize);
    return 0;
}
*/
import "C"

import (
    "errors"
    "fmt"
    "syscall"
    "unsafe"
)

const (
    maxPathLen     = 1024 // MAXPATHLEN: the size of vip_path (cwd buffer)
    pidPathMaxSize = 4096 // PROC_PIDPATHINFO_MAXSIZE (4*MAXPATHLEN): required for proc_pidpath
)

// errno classification. The scanner skips both errProcUnavailable (EPERM: alive but
// unreadable) and errProcGone (ESRCH: exited mid-scan) — but they are KEPT DISTINCT so the
// reconciler can reap on ESRCH and the killer can treat "gone" as success while still
// re-validating on EPERM.
var (
    errProcUnavailable = errors.New("procscan: process info unavailable (EPERM)")
    errProcGone        = errors.New("procscan: process gone (ESRCH)")
)

func classifyErrno(e C.int) error {
    switch syscall.Errno(e) {
    case syscall.ESRCH:
        return errProcGone
    case syscall.EPERM, syscall.EACCES:
        return errProcUnavailable
    default:
        return fmt.Errorf("procscan: libproc errno %d", int(e))
    }
}

// listPIDs returns every PID currently known to the kernel.
func listPIDs() ([]int32, error) {
    var cerr C.int
    nbytes := int(C.bb_count_all_pids(&cerr))
    if nbytes <= 0 {
        return nil, fmt.Errorf("proc_listpids size probe failed: %w", classifyErrno(cerr))
    }
    // Over-allocate by ~64 slots: PIDs can appear between the probe and the read.
    cap := nbytes/int(unsafe.Sizeof(C.int(0))) + 64
    buf := make([]int32, cap)
    written := int(C.bb_list_all_pids(
        (*C.int)(unsafe.Pointer(&buf[0])), C.int(cap), &cerr))
    if written <= 0 {
        return nil, fmt.Errorf("proc_listpids read failed: %w", classifyErrno(cerr))
    }
    n := written / int(unsafe.Sizeof(C.int(0)))
    if n > cap {
        n = cap // defensive: never index past the buffer if the kernel grew the list
    }
    out := make([]int32, 0, n)
    for _, p := range buf[:n] {
        if p > 0 { // kernel pads with zero entries
            out = append(out, p)
        }
    }
    return out, nil
}

// pidPath returns the absolute executable path for a PID.
// Buffer is PROC_PIDPATHINFO_MAXSIZE (4096) as Apple requires. Returns a typed error
// (errProcUnavailable / errProcGone) the caller can branch on.
func pidPath(pid int32) (string, error) {
    buf := make([]byte, pidPathMaxSize)
    var cerr C.int
    n := int(C.bb_pid_path(C.int(pid),
        (*C.char)(unsafe.Pointer(&buf[0])), C.int(len(buf)), &cerr))
    if n <= 0 {
        return "", classifyErrno(cerr)
    }
    return string(buf[:n]), nil
}

// pidCwd returns the current working directory for a PID via PROC_PIDVNODEPATHINFO.
// NOTE: this flavor is the one most likely to return EPERM even for same-user processes
// under the hardened runtime / restricted targets (see §6 privilege caveat).
func pidCwd(pid int32) (string, error) {
    buf := make([]byte, maxPathLen)
    var cerr C.int
    ret := int(C.bb_pid_cwd(C.int(pid),
        (*C.char)(unsafe.Pointer(&buf[0])), C.int(len(buf)), &cerr))
    if ret != 0 {
        return "", classifyErrno(cerr)
    }
    return goStr(buf), nil
}

func goStr(b []byte) string {
    for i, c := range b {
        if c == 0 {
            return string(b[:i])
        }
    }
    return string(b)
}
```

`errProcUnavailable` (EPERM/EACCES — process not readable, e.g. another user's daemon or a
hardened-runtime target) and `errProcGone` (ESRCH — exited mid-scan, the PID-reuse race in
§6) are both treated by the scanner as "skip this PID, not fatal", but they are **distinct**
sentinels so downstream code (reaper, killer) can act on "gone" specifically.

### 2.2 The pure-Go scanner (platform-agnostic surface)

```go
// scanner.go (no cgo here — only the interface + the bazel-client filter)

type ProcInfo struct {
    PID      int32
    ExePath  string // from proc_pidpath
    Cwd      string // from PROC_PIDVNODEPATHINFO
}

// Scanner is the seam behind D-stack-1: the libproc impl is the default,
// but a shell-out (lsof/ps) impl can satisfy the same interface if we flip D-stack-1.
type Scanner interface {
    // Snapshot returns all *bazel client* processes currently running.
    Snapshot() ([]ProcInfo, error)
}

// libprocScanner is the darwin/cgo implementation.
type libprocScanner struct{}

func NewScanner() Scanner { return libprocScanner{} } // darwin build returns cgo impl

func (libprocScanner) Snapshot() ([]ProcInfo, error) {
    pids, err := listPIDs()
    if err != nil {
        return nil, err
    }
    var out []ProcInfo
    for _, pid := range pids {
        path, err := pidPath(pid)
        if err != nil {
            continue // errProcUnavailable (EPERM) or errProcGone (ESRCH) → skip this PID
        }
        if !isBazelClient(path) {
            continue
        }
        cwd, err := pidCwd(pid)
        if err != nil {
            // A bazel client we matched on path but whose cwd we cannot read (EPERM under
            // hardened runtime, or it just exited). We log-once at DEBUG and skip: a build
            // we can see but not place in a worktree is not actionable. See §6 caveat.
            continue
        }
        out = append(out, ProcInfo{PID: pid, ExePath: path, Cwd: cwd})
    }
    return out, nil
}
```

**Client vs server distinction (critical — see §6).** Both the bazel *client* and the
JVM *server* show up in the PID list. We want the **client** (the process that owns the
terminal, whose cwd is the worktree, and that responds to `SIGINT`):

```go
// exeAllowRe is compiled once from BB_DISCOVERY_EXE_ALLOW (test hook); nil in production.
var exeAllowRe = compileEnvRegexp("BB_DISCOVERY_EXE_ALLOW")

func isBazelClient(exePath string) bool {
    base := filepath.Base(exePath)
    // Test override: if set, it is the ONLY allowlist (lets tests match fake-bazel.sh etc.).
    if exeAllowRe != nil {
        return exeAllowRe.MatchString(exePath)
    }
    switch base {
    case "bazel", "bazelisk", "bazel-real":
        return true
    }
    return false
}
```

> ⚠️ **The exe-path filter is NOT sufficient on its own to separate client from server — and
> the prior draft's "the server is a java/jdk binary" assumption is wrong.** Bazel does **not**
> run its server as a plain `java`/`/jdk/` process: the server is launched from Bazel's own
> **embedded launcher** and its `proc_pidpath` is typically the embedded
> `…/install/<hash>/bazel` binary (and on some versions the client and server share that same
> embedded exe). So basename/path matching can match **both** the client and the server. The
> **only robust discriminator is the cwd**: the **client's cwd is the workspace/worktree**,
> while the **server's cwd is its output base** (`…/server`, no `.git` reachable). Therefore
> client-vs-server is decided **in reconcile (2.4), not here**: any matched proc whose cwd does
> not resolve to a git worktree (`ResolveFromCwd` → `ErrNotInWorktree`) is dropped — this is
> what excludes the server, strays run outside a workspace, and the bazelisk download dir.
> `isBazelClient` is just the cheap first-pass exe filter; the cwd→worktree resolution is the
> authoritative gate. The e2e test (5.5) must assert that with a real client+server pair only
> the **client** survives both stages. The `BB_DISCOVERY_EXE_ALLOW` regex (used by tests to
> match `fake-bazel.sh`) is honored above and is unset in production.

### 2.3 PID → cwd → worktree algorithm

Given a cwd, find the **git worktree root** and a stable worktree identity. Worktrees are the
key data model (§3 of architecture: builds are keyed by worktree path).

```go
// worktree/resolve.go

type Worktree struct {
    Root    string // absolute path of the worktree working tree
    Name    string // last path component (display) — e.g. "feature-a"
    GitDir  string // resolved .git directory for THIS worktree (for output-base lookup later)
}

// ResolveFromCwd walks up from dir until it finds a `.git` (dir OR file), then resolves
// the actual worktree. Returns ErrNotInWorktree if none is found before the FS root.
func ResolveFromCwd(dir string) (Worktree, error) {
    cur := filepath.Clean(dir)
    for {
        dotgit := filepath.Join(cur, ".git")
        fi, err := os.Lstat(dotgit)
        if err == nil {
            if fi.IsDir() {
                // Primary working tree: .git is a directory.
                return Worktree{Root: cur, Name: filepath.Base(cur), GitDir: dotgit}, nil
            }
            // Linked worktree: .git is a FILE containing "gitdir: <path>".
            gitdir, err := readGitdirFile(dotgit)
            if err != nil {
                return Worktree{}, err
            }
            return Worktree{Root: cur, Name: filepath.Base(cur), GitDir: gitdir}, nil
        }
        parent := filepath.Dir(cur)
        if parent == cur { // reached "/"
            return Worktree{}, ErrNotInWorktree
        }
        cur = parent
    }
}

// readGitdirFile parses `.git` files of the form "gitdir: /abs/path/.git/worktrees/<name>".
func readGitdirFile(path string) (string, error) {
    b, err := os.ReadFile(path)
    if err != nil {
        return "", err
    }
    line := strings.TrimSpace(string(b))
    line = strings.TrimPrefix(line, "gitdir:")
    return strings.TrimSpace(line), nil
}
```

Notes:
- We deliberately do **not** shell out to `git rev-parse`; a pure filesystem walk is faster,
  has no process-spawn cost per discovered PID, and works even if `git` isn't on `$PATH`.
- `Root` is what we display and what we dedupe builds on. For a **linked** worktree, `Root`
  is still the worktree's own working directory (the `cwd`'s ancestor that holds `.git`),
  which is exactly the per-worktree identity we want — distinct output bases per §3.
- `GitDir` is captured now because E3's *optional* command-server `Cancel` path (2.6) and
  later epics need to find the **output base** (`bazel info output_base`, or
  `output_user_root/md5(workspace_path)`); we keep the hook but don't require it for SIGINT.

### 2.4 Registry reconciliation

E2 owns `registry.Registry` (in-memory map + SQLite mirror). E3 adds a periodic reconcile
pass that folds discovered processes in **without clobbering** wrapper-`Register`'d builds.

> ⚠️ **Interface mismatch with E2 — must be reconciled with the E2 owner (T5).** E2 §2.3
> already declares `Upsert(b *build.Build) (*build.Build, error)` and its in-memory map is
> **keyed solely by `InvocationID`** (a `string`). E2 §2.2 also types `Build.PID` as **`int`**,
> not `int32`. This epic must NOT silently invent a parallel `UpsertSpec` shape; it must either
> (a) adopt E2's `Upsert(*build.Build)` and add a **secondary PID index** to the E2 registry, or
> (b) get E2 to widen `Upsert` to the spec form below. Either way the PID dedupe in this section
> **requires a `pid → InvocationID` index that does not exist in E2 today** — the map is
> string-keyed, so "find a build by pid" is O(n) without it. **Action: T5 adds `FindByPID` backed
> by a `map[int]string` secondary index, and the synthetic-id scheme below must use a value that
> is a valid `InvocationID` map key (E2's primary key) — not a second key space.** `int32` PIDs
> from libproc are converted to `int` at the registry boundary.

**Build identity & dedupe key.** A build can be known by two identifiers:
- `invocation_id` — authoritative, present only when the wrapper or BEP supplied it (E4/E5).
- `pid` — always present for discovered builds.

Dedupe rule (in priority order):
1. If a registry build already has this **pid**, it's the same build → update `Source`,
   `Cwd`, `Worktree`, `LastSeen`; keep its `invocation_id`, `targets`, `state`.
2. Else if a registry build has the same `invocation_id` **and** no pid yet (wrapper
   registered before the process was scanned) → attach the discovered `pid`.
3. Else it's a brand-new **discovered** build → insert with `Source = SourceDiscovered`,
   synthesize a stable id `disco:<pid>` until/unless an `invocation_id` arrives.

```go
// reconcile/reconcile.go

type Reconciler struct {
    scan     procscan.Scanner
    reg      *registry.Registry     // from E2 (pointer; E2 §2.3 methods are on *Registry)
    resolve  func(string) (worktree.Worktree, error) // worktree.ResolveFromCwd
    clock    func() time.Time
}

// ReconcileOnce performs one discovery pass and merges into the registry.
func (r *Reconciler) ReconcileOnce(ctx context.Context) error {
    procs, err := r.scan.Snapshot()
    if err != nil {
        return err
    }
    now := r.clock()
    seenPIDs := make(map[int]bool, len(procs)) // int to match E2 Build.PID

    for _, p := range procs {
        wt, err := r.resolve(p.Cwd)
        if err != nil {
            continue // not in a worktree → not a build we surface (filters servers/strays)
        }
        pid := int(p.PID) // libproc int32 → E2's int at the registry boundary
        seenPIDs[pid] = true

        // Build an E2 build.Build and hand it to E2's existing Upsert (E2 §2.3). The dedupe/
        // precedence rules (2.4) live INSIDE registry.Upsert, which must consult the PID index.
        // For a brand-new discovered process with no invocation_id yet, synthesize a primary
        // key in E2's key space; it is overwritten when a real invocation_id later arrives.
        r.reg.Upsert(&build.Build{
            InvocationID: synthDiscoID(pid), // e.g. "disco-<pid>"; valid map key, replaced on real id
            PID:          pid,
            ExePath:      p.ExePath,   // NEW field (E2 §4.3)
            Cwd:          p.Cwd,       // NEW field
            Worktree:     wt.Root,
            WorktreeName: wt.Name,     // NEW field
            GitDir:       wt.GitDir,   // NEW field
            Source:       build.SourceDiscovered, // E2 const "discovered"; precedence: never downgrades "registered"
            State:        build.StateRunning,
            StartTime:    now,         // E2's StartTime doubles as first-seen
            LastSeen:     now,         // NEW field
        })
    }

    // Reap: mark discovered builds whose PID vanished as StateUnknown (E2's enum; there is no
    // "gone" state). Wrapper/registered builds are NOT reaped here — their lifecycle is
    // Deregister (E5). ReapMissingDiscovered must only touch source=="discovered" rows.
    r.reg.ReapMissingDiscovered(seenPIDs, now)
    return nil
}

// synthDiscoID returns a primary-key string for a discovered build that has no invocation_id
// yet. It MUST live in E2's single InvocationID key space (the map is string-keyed), not a
// second namespace, so a later real invocation_id supersedes it via the dedupe rules in 2.4.
func synthDiscoID(pid int) string { return fmt.Sprintf("disco-%d", pid) }
```

`registry.Upsert` is **E2's existing method** (E2 §2.3), not a new `UpsertSpec`; the dedupe
priority above lives inside it. `Source` uses a precedence so a later discovery pass never
overwrites `SourceRegistered` ("registered") with `SourceDiscovered` (so admission/registered
state from E5 is preserved). **Synthetic-id caveat:** keying a no-invocation_id build on
`disco-<pid>` means that if the *same* PID is reused for a *different* later build before the
reaper runs, the stale row could be updated rather than replaced — the reaper's per-pass
`seenPIDs` plus the `LastSeen` staleness check (and PID-exe re-validation, §6) bound this.

**Cadence.** A goroutine in the broker runs `ReconcileOnce` on a ticker (default **1s**;
fast enough that a build appears in `ls` "instantly" for the acceptance test, cheap enough
that a full `proc_listpids` sweep — a few hundred PIDs — costs well under a millisecond).
Also reconcile **on demand** immediately before serving `ListBuilds` and before a `Kill`
lookup, so the API is never staler than one request.

### 2.5 Kill state machine: SIGINT → grace → SIGKILL (D4 default path)

```go
// killer/killer.go

type Config struct {
    Grace      time.Duration // default 750ms — under the <1s acceptance budget
    PollEvery  time.Duration // default 50ms
    UseCancel  bool          // D4: if true, try command-server Cancel first (2.6)
}

// KillSpec is what the /kill handler resolves a request into. We carry the EXE PATH that
// discovery recorded for this pid so the killer can re-validate identity immediately before
// signalling — closing the PID-reuse window (a recycled pid would now point at a different
// exe). `int` matches E2's Build.PID.
type KillSpec struct {
    PID         int
    ExpectExe   string // build.ExePath captured at discovery; "" => skip re-validation
    Force       bool   // API "force": skip SIGINT/grace, go straight to SIGKILL
}

// Kill terminates a build. Returns the terminal outcome.
func (k *Killer) Kill(ctx context.Context, s KillSpec) (Outcome, error) {
    pid := s.PID

    // GUARD 0 (PID-reuse, mandatory): re-validate the live exe path is STILL the bazel client
    // discovery recorded. If the pid was recycled to an unrelated process, refuse to signal it.
    // This runs for every Kill, not just kill-by-id. ESRCH here == already gone == success.
    if s.ExpectExe != "" {
        cur, err := pidPath(int32(pid))
        switch {
        case errors.Is(err, errProcGone):
            return OutcomeAlreadyGone, nil
        case err != nil:
            // EPERM: cannot confirm identity. Sudo-free, single-user scope means our targets are
            // ours; an EPERM here means the pid is now a foreign process → refuse (do NOT signal).
            return OutcomeError, fmt.Errorf("pid %d: cannot verify identity before kill: %w", pid, err)
        case cur != s.ExpectExe:
            return OutcomeError, fmt.Errorf("pid %d reused (exe %q != expected %q); refusing to signal", pid, cur, s.ExpectExe)
        }
    }

    if gone, _ := pidGone(pid); gone {
        return OutcomeAlreadyGone, nil
    }

    // API "force": skip the graceful path entirely.
    if s.Force {
        return k.sigkill(ctx, pid)
    }

    // D4 OPTIONAL out-of-band path, tried first only when enabled.
    if k.cfg.UseCancel {
        if err := k.tryCommandServerCancel(pid); err == nil {
            if waitGone(ctx, pid, k.cfg.Grace, k.cfg.PollEvery) {
                return OutcomeCancelled, nil
            }
        }
        // fall through to signal path on failure / timeout
    }

    // Step 1: SIGINT — bazel client's graceful cancellation (architecture §3.6).
    if err := syscall.Kill(pid, syscall.SIGINT); err != nil {
        if errors.Is(err, syscall.ESRCH) {
            return OutcomeAlreadyGone, nil // PID-reuse-safe: gone == success
        }
        return OutcomeError, err
    }

    // Step 2: grace window — poll for exit.
    if waitGone(ctx, pid, k.cfg.Grace, k.cfg.PollEvery) {
        return OutcomeSIGINT, nil
    }

    // Step 3: escalate to SIGKILL.
    return k.sigkill(ctx, pid)
}

func (k *Killer) sigkill(ctx context.Context, pid int) (Outcome, error) {
    if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
        if errors.Is(err, syscall.ESRCH) {
            return OutcomeSIGKILL, nil // already gone == killed
        }
        return OutcomeError, err
    }
    if waitGone(ctx, pid, 250*time.Millisecond, k.cfg.PollEvery) {
        return OutcomeSIGKILL, nil
    }
    return OutcomeError, fmt.Errorf("pid %d survived SIGKILL", pid)
}

// pidGone reports whether the pid definitively does not exist. It returns (true, ESRCH) only
// on ESRCH. CRITICAL: unlike a naive "alive" probe, EPERM is treated as NOT-gone but is
// surfaced so callers never spin forever on a foreign pid (see waitGone). For OUR OWN
// processes (the only kill targets in this sudo-free tool) signal 0 returns nil or ESRCH —
// never EPERM — so EPERM here means the pid was recycled to another user's process.
func pidGone(pid int) (bool, error) {
    err := syscall.Kill(pid, 0)
    switch {
    case err == nil:
        return false, nil
    case errors.Is(err, syscall.ESRCH):
        return true, syscall.ESRCH
    default:
        return false, err // EPERM etc.: exists but not ours → treat as "not gone" but flag
    }
}

// waitGone polls until the pid is ESRCH-gone or the budget expires. An EPERM (foreign,
// recycled pid) ends the wait early with false rather than spinning the whole budget — the
// caller's identity re-validation (GUARD 0) already refused such pids, so reaching here with
// EPERM is a recycled-mid-kill edge; we stop fast and report non-exit.
func waitGone(ctx context.Context, pid int, budget, every time.Duration) bool {
    deadline := time.Now().Add(budget)
    for {
        gone, err := pidGone(pid)
        if gone {
            return true
        }
        if err != nil && !errors.Is(err, syscall.ESRCH) {
            return false // EPERM: not ours anymore; do not claim success, do not spin
        }
        if !time.Now().Before(deadline) {
            return false
        }
        select {
        case <-ctx.Done():
            return false
        case <-time.After(every):
        }
    }
}
```

**PID-reuse safety (hardened):** three layers. (1) **GUARD 0** re-validates the live exe path
equals the discovered `ExePath` immediately before *any* signal — a recycled pid pointing at a
different binary is refused, not signalled. (2) `ESRCH` from any `syscall.Kill` / signal-0 probe
is treated as *gone == success*; we never escalate `SIGKILL` against a vanished pid. (3)
`waitGone` stops fast on `EPERM` instead of spinning, so a pid recycled to a foreign process
mid-kill cannot produce a false "survived SIGKILL". Reconcile-on-demand before `/kill` (2.4)
keeps `ExpectExe` fresh. Residual window (pid recycled to *another bazel client* between
re-validation and signal) is microseconds and accepted.

**Cancel code:** `fake-bazel.sh` traps `SIGINT` and exits with the agreed cancel code (E0
defines it; architecture §10 / 02-epics E0 require the trap). The acceptance test asserts
that exact exit code, proving the **graceful** path (SIGINT), not SIGKILL, did the work.

### 2.6 Command-server `Cancel` sketch (D4 out-of-band, OPTIONAL, flag-gated)

Bazel's server exposes a gRPC **command server**; cancellation is out-of-band of the client.
This path is sketched, built behind `Config.UseCancel`, and **not** on the default path
(SIGINT is simpler and satisfies the acceptance criteria). It exists so D4 can be evaluated
empirically.

```go
// cancel_darwin.go (build behind UseCancel)
//
// Files under the server dir of the worktree's output base:
//   <output_base>/server/command_port      -> "127.0.0.1:<port>" (gRPC endpoint)
//   <output_base>/server/request_cookie     -> opaque string echoed on every RPC
//   <output_base>/server/response_cookie     -> expected on every response
//
// 1. Resolve output_base for the worktree (shell `bazel info output_base`, or compute
//    output_user_root/md5(workspace_path) — captured GitDir/worktree from 2.3 feeds this).
// 2. Read command_port + request_cookie + response_cookie.
// 3. Dial the CommandServer gRPC (proto: src/main/protobuf/command_server.proto).
// 4. To cancel a *running* command you need its command_id (from RunResponse on the
//    in-flight RPC) — which the OUT-OF-BAND broker does not have unless it owns the stream.
//    => Practical broker approach: call CommandServer.Cancel with the cookie; if we lack a
//       command_id, fall back to PID SIGINT. This is the core D4 caveat (see §6).
```

> Honest assessment carried into D4: out-of-band `Cancel` requires the in-flight command's
> `command_id`, which only the process that issued the `Run` RPC holds. The broker, having
> not issued it, would have to scrape it (not exposed in the server dir). Therefore the
> **recommended default is owned-PID SIGINT**; `Cancel` is kept as a sketch for the case
> where the wrapper (E5) issues the build and can hand the broker the `command_id`.

---

## 3. Sequencing (ordered, checkpointed)

Each task is independently verifiable. Checkpoints (`✓`) are the verify gate to pass before
moving on.

1. **T1 — Scaffolding & interfaces.** Create `internal/procscan` (Scanner interface +
   `ProcInfo`), `internal/worktree`, `internal/reconcile`, `internal/killer` with stubs and
   build tags. Non-darwin stub returns `ErrUnsupported`.
   `✓` `go build ./...` and `go vet ./...` pass on arm64; non-darwin cross-compile (`GOOS=linux go build`) still compiles (stub).

2. **T2 — cgo libproc wrapper.** Implement `libproc_darwin.go` (`listPIDs`, `pidPath`,
   `pidCwd`) + the C shims.
   `✓` Unit test `libproc_darwin_test.go`: scan finds the **test process's own PID** (`os.Getpid()`),
   `pidPath` returns the test binary path, `pidCwd` returns the test's cwd (set via `os.Chdir` in a temp dir).

3. **T3 — bazel-client filter.** Implement `isBazelClient` + `Snapshot`.
   `✓` Launch a `sleep 60` renamed to `bazel`/`fake-bazel.sh` (or the E0 `fake-bazel.sh`),
   assert `Snapshot()` includes it and excludes ordinary processes and `java`.

4. **T4 — worktree resolver.** Implement `ResolveFromCwd` + `.git`-file parsing.
   `✓` Table tests over: primary tree (`.git` dir), linked worktree (`.git` file →
   `gitdir:`), nested subdir of a worktree, and a non-repo dir (`ErrNotInWorktree`). Create
   real fixtures with `git init` + `git worktree add` in `t.TempDir()`.

5. **T5 — registry additions (coordinate with E2 — this is the cross-epic seam).** Extend
   E2's **existing** `Upsert(*build.Build)` with the 2.4 dedupe/precedence rules, add the
   **NEW** `Build` fields (§4.3), `ReapMissingDiscovered(map[int]bool, time.Time)` (sets
   `StateUnknown`, not a new state), `FindByPID(int)` backed by a `map[int]string` PID index,
   and `FindByInvocationID(string)`. Reuse E2's `Source`/`State` enums verbatim (no
   `Wrapper`/`Gone`). **Must be agreed with the E2 owner — touches the E2-frozen contract.**
   `✓` E2's existing registry tests still pass; new upsert/dedupe/reap/PID-index tests pass.

6. **T6 — Reconciler.** Wire `Scanner` + resolver + registry; the 1s ticker goroutine inside
   the broker (started from E2's daemon `main`).
   `✓` Inject a **fake Scanner** returning a fixed `ProcInfo` whose `Cwd` points at a temp
   worktree → `ReconcileOnce` → registry shows one `discovered` build with the right
   `Worktree`. Drop the proc from the fake → next pass reaps it to `StateUnknown`.

7. **T7 — Killer state machine.** Implement `Kill(KillSpec)` (GUARD 0 re-validation →
   SIGINT→grace→SIGKILL, plus `Force`), `pidGone`, `waitGone`.
   `✓` Spawn a child that traps `SIGINT` and exits with the cancel code → `Kill` returns
   `OutcomeSIGINT` and child's exit code matches, in <1s. Spawn a child that **ignores**
   SIGINT → `Kill` returns `OutcomeSIGKILL` after grace.

8. **T8 — `POST /kill` endpoint.** Add handler to E2's HTTP mux: parse body, resolve
   `invocation_id|pid` → registry build → `Killer.Kill`, return outcome JSON. Reconcile
   on-demand before lookup.
   `✓` `curl -XPOST .../kill -d '{"pid":<pid>}'` against a running fake-bazel returns
   `{"outcome":"sigint",...}` and the process is gone.

9. **T9 — End-to-end acceptance.** The full epic "Done when": fake-bazel long-duration in a
   real worktree → `ls` shows it with correct worktree → `kill <id>` → cancel exit <1s.
   `✓` Scripted in `make verify-e3` (see §5).

10. **T10 (optional, flag-gated) — command-server Cancel sketch.** Implement `cancel_darwin.go`
    behind `UseCancel`; document the `command_id` limitation in D4.
    `✓` With a real bazel build and `UseCancel=true`, exercise the path; record findings into
    D4 in this doc. Non-blocking for epic completion.

---

## 4. Interfaces & contracts

### 4.1 New HTTP endpoint — `POST /kill` (added to the E2 API)

This is the E3 addition to the E2 localhost HTTP+WS API (architecture §6 row `Kill`).

**Request** `POST /kill` (loopback only, bearer token from E2; `Content-Type: application/json`):
```json
{ "invocation_id": "abc-123" }     // OR
{ "pid": 48213 }                    // exactly one of the two is required
{ "pid": 48213, "force": true }     // force => skip SIGINT+grace, SIGKILL immediately (escape hatch)
{ "pid": 48213, "use_cancel": true }// opt into D4 command-server path — see caveat below
```

> `use_cancel` is **effectively a no-op today**: the passive broker has no `command_id`
> (D4, §6), so the Cancel path always falls through to SIGINT. The flag is wired now so the
> API contract is stable, but it MUST NOT be advertised as a working capability until E5
> forwards command_ids. `force` maps to `KillSpec.Force` and goes straight to SIGKILL.

**Response 200:**
```json
{
  "killed": true,
  "pid": 48213,
  "invocation_id": "abc-123",
  "outcome": "sigint",          // sigint | sigkill | cancelled | already_gone
  "elapsed_ms": 213
}
```

**Error responses:** `400` (neither/both id+pid, or malformed), `404`
(`{"killed":false,"error":"no build matching ..."}`), `403` (bad bearer token, from E2),
`500` (`{"killed":false,"outcome":"error","error":"pid N survived SIGKILL"}`).

**Resolution rule:** `invocation_id` → registry lookup → `pid`. If only `pid` is given, it
must correspond to a discovered/registered build (we don't kill arbitrary unrelated PIDs;
the registry membership check is the authorization boundary in a sudo-free, single-user tool).

### 4.2 Discovery → registry feed

`Reconciler.ReconcileOnce` is the only writer of `SourceDiscovered` builds. It calls
`registry.Upsert` / `ReapMissingDiscovered` (E2-owned methods we add). The broker's
`ListBuilds` (E2) and `POST /kill` (E3) both read the same registry — discovery and the
wrapper converge on one registry, deduped by `(pid, invocation_id)` per 2.4.

### 4.3 Shared `Build` fields (E2 registry — fields E3 adds/uses)

E3 extends the **E2-owned** `build.Build` (E2 §2.2) — it does **not** fork the type. Fields
already present in E2 are reused with E2's exact names/types; only the rows marked **NEW** are
additions E3 requests of the E2 owner in T5. **Two contract corrections vs the previous draft:**
(1) E2's `Source` consts are `SourceRegistered` ("registered") and `SourceDiscovered`
("discovered") — there is **no `Wrapper` / `SourceWrapper`**; everywhere this plan says
"wrapper source", read **"registered"**. (2) E2's lifecycle for a vanished *discovered* build is
`StateUnknown` ("unknown"), per E2 §2.2's state machine — there is **no new `Gone` state**; the
reaper sets `StateUnknown`. Inventing `Gone`/`gone` would break the E2 wire enum frozen in E2 §4.1.

| Field | Type | Status | Source | Used by |
|---|---|---|---|---|
| `InvocationID` | `string` | E2 (primary key; empty→synthesized, see 2.4) | wrapper/BEP | dedupe, `/kill` lookup |
| `PID` | **`int`** (E2's type, not `int32`) | E2 | discovery; libproc `int32` cast at boundary | dedupe, `/kill`, reap |
| `Worktree` | `string` | E2 | `ResolveFromCwd().Root` | `ls`, dedupe per worktree |
| `Targets` | `[]string` | E2 | wrapper/BEP | display |
| `State` | E2 enum incl. `StateUnknown` | E2 | reap sets `StateUnknown`, `/kill` sets `StateKilled` | `ls`, lifecycle |
| `Source` | E2 enum `{registered, discovered}` | E2 | reconcile | precedence (don't downgrade) |
| `StartTime` | `time.Time` | E2 (== "first seen"; no separate `FirstSeen`) | reconcile | elapsed, reap |
| `ExePath` | `string` | **NEW** | `proc_pidpath` | client/server filtering, display |
| `Cwd` | `string` | **NEW** | `PROC_PIDVNODEPATHINFO` | worktree resolution |
| `WorktreeName` | `string` | **NEW** | `ResolveFromCwd().Name` | `ls` display |
| `GitDir` | `string` | **NEW** | `ResolveFromCwd().GitDir` | output-base lookup (D4 Cancel) |
| `LastSeen` | `time.Time` | **NEW** | reconcile | staleness, reap |

> The **NEW** fields are non-serialized internal fields (E2 §2.2 already reserves space for
> exactly this kind of E3 extension below the wire line) **except** anything that must reach the
> UI. If `ls` needs `worktree_name` / `cwd`, those go through the **E2 wire DTO + a `ToWire()`
> change**, which is an E2-frozen-contract edit and must be agreed with the E2 owner — flag in T5.

E2-side method additions requested by this epic (T5) — **expressed in E2's existing shapes**:
- **Reuse E2's `Upsert(b *build.Build) (*build.Build, error)`** (already declared in E2 §2.3)
  rather than a new `UpsertSpec`; the 2.4 dedupe/precedence rules live inside it. If a spec
  struct is preferred, that is an E2 API change to negotiate, not an E3 fait accompli.
- `ReapMissingDiscovered(seen map[int]bool, now time.Time)` — mark vanished `discovered`
  builds `StateUnknown` (NOT a new state). `int` keys to match `Build.PID`.
- `FindByInvocationID(string) (*build.Build, bool)` and `FindByPID(int) (*build.Build, bool)` —
  for `/kill`. `FindByPID` needs the secondary `map[int]string` PID index added to E2's registry
  (see the §2.4 callout); without it E2's string-keyed map makes pid lookup O(n).

---

## 5. Testing & verification

### 5.1 The fake-bazel harness (from E0)

`testdata/fake-bazel.sh` (E0 deliverable) is the verification engine: it sleeps
`FAKE_BAZEL_DURATION` seconds, traps `SIGINT` to exit with the **cancel code**, and honors
`--build_event_json_file`. For discovery to pick it up, the scanner accepts an env override
`BB_DISCOVERY_EXE_ALLOW` (regex) so the test can match `fake-bazel.sh`/`bazel` basenames
without shipping a real bazelisk. In production this override is unset and the built-in
`isBazelClient` allowlist applies.

### 5.2 Unit tests (per task, §3 checkpoints)

- `procscan`: self-PID discovery (own pid/path/cwd) — the only Mac-syscall test, gated by
  `//go:build darwin`.
- `worktree`: real `git init` + `git worktree add` fixtures in `t.TempDir()`; primary tree,
  linked worktree, nested dir, non-repo.
- `reconcile`: **fake Scanner** (no syscalls) → assert upsert, dedupe by pid then
  invocation_id, source precedence, and reap-on-vanish.
- `killer`: spawn helper children (one traps SIGINT→cancel-code, one ignores SIGINT) →
  assert `OutcomeSIGINT` <1s and `OutcomeSIGKILL` after grace; assert `ESRCH`→`already_gone`;
  assert `KillSpec.Force` goes straight to SIGKILL; assert **GUARD 0**: a `KillSpec` whose
  `ExpectExe` no longer matches the live exe (simulate by passing a deliberately wrong
  `ExpectExe` for a live helper) is **refused** (`OutcomeError`, no signal delivered — verify
  the helper is still alive afterward).

### 5.3 `/verify` recipe — `make verify-e3`

```make
verify-e3: build
	# 1. Start broker (E2), wait for /healthz.
	./bin/broker --addr 127.0.0.1:8765 --token $$BB_TOKEN &  echo $$! > /tmp/bb.pid
	until curl -fsS -H "Authorization: Bearer $$BB_TOKEN" localhost:8765/healthz; do sleep 0.1; done

	# 2. Launch a long fake build INSIDE a throwaway worktree.
	( cd testdata/workspace-wt-a && \
	  BB_DISCOVERY_EXE_ALLOW='fake-bazel|bazel' \
	  FAKE_BAZEL_DURATION=120 ../../testdata/fake-bazel.sh build //:app \
	    --build_event_json_file=/tmp/wt-a.json ) &
	echo $$! > /tmp/fake.pid
	sleep 1.2   # one reconcile tick + margin

	# 3. It must appear in `ls` with the correct worktree.
	#    E2 /builds returns {"builds":[...]} (E2 §4.1 BuildsResponse), NOT a bare array.
	curl -fsS -H "Authorization: Bearer $$BB_TOKEN" localhost:8765/builds \
	  | tee /tmp/ls.json | jq -e '.builds[] | select(.worktree | test("workspace-wt-a$"))'

	# 4. Kill by pid, assert <1s and cancel exit code (e.g. 8 — defined in E0).
	PID=$$(jq -r '.builds[0].pid' /tmp/ls.json)
	START=$$(date +%s%N)
	curl -fsS -XPOST -H "Authorization: Bearer $$BB_TOKEN" \
	  localhost:8765/kill -d "{\"pid\":$$PID}" | jq -e '.outcome=="sigint"'
	wait $$(cat /tmp/fake.pid); EC=$$?
	END=$$(date +%s%N); MS=$$(( (END-START)/1000000 ))
	test $$MS -lt 1000          # under one second
	test $$EC -eq 8             # graceful cancel code from fake-bazel SIGINT trap

	kill $$(cat /tmp/bb.pid)
```

### 5.4 Acceptance criteria (maps 1:1 to epic "Done when")

| Criterion | Verified by |
|---|---|
| Long fake-bazel in a worktree **appears in `ls`** | step 3: `jq` selects the build by worktree |
| with the **correct worktree** | step 3 worktree regex matches `workspace-wt-a` |
| `kill <id>` makes it **exit with the cancel code** | step 4: `EC == 8` |
| in **<1s** | step 4: `MS < 1000` |
| Discovery needs **no wrapper** | the fake build runs unwrapped; only discovery sees it |

### 5.5 Occasional real e2e

Against `testdata/workspace/` (E0) with **real bazelisk** in a real `git worktree`: run a
trivial slow target, confirm `ls` shows the real worktree + real bazel client pid, and
`/kill` SIGINTs it (real bazel prints "build interrupted"). This validates `isBazelClient`
against an actual client/server pair and the real cwd→output-base relationship.

---

## 6. Risks, edge cases, open decisions

### Open decisions (surfaced, NOT resolved here)

- **D-stack-1 — cgo+libproc vs shell-out to `lsof`/`ps`. (NOT resolved here — escalate.)**
  *This plan recommends and implements cgo+libproc* because: (a) one in-process syscall sweep
  costs sub-millisecond vs spawning `ps`/`lsof` per poll (1s cadence × N pids), (b) no fragile
  text parsing, (c) exact struct access. **Honest correction to the prior draft's claim:** both
  `proc_pidinfo(PROC_PIDVNODEPATHINFO)` *and* `lsof` read cwd via the same kernel mechanism and
  have **the same privilege ceiling** — neither is privileged for fully-owned processes, and
  *both* can hit `EPERM` against hardened-runtime targets. So privilege is **not** a
  differentiator; cost and robustness are. **Costs of cgo:** (1) broker is no longer pure-Go /
  cross-compilable and needs the macOS SDK + `CGO_ENABLED=1`; CI must build on macOS; (2) it
  pulls cgo into a binary E2 deliberately kept cgo-free (pure-Go SQLite). The `Scanner`
  interface (2.2) is the seam so a shell-out impl can replace it if D-stack-1 flips — and the
  cwd-`EPERM` caveat above is a *reason the flip might be partially forced* (shell-out only for
  cwd). **Decision owner:** Antonis. **Default carried forward: cgo+libproc; revisit if cwd
  `EPERM` is observed in the field.**

- **D4 — owned-PID SIGINT vs command-server `Cancel`. (NOT resolved here — escalate.)**
  *This plan ships SIGINT as the default* (simple, satisfies acceptance, no output-base
  resolution needed) and keeps `Cancel` as a flag-gated sketch (2.6). **The structural blocker,
  stated plainly:** Bazel's `CommandServer.Cancel` RPC takes the **`command_id`** of the
  in-flight command, which is returned only on the `Run` RPC's response stream to the *client
  that issued the build*. A passive, out-of-band broker **never sees that `command_id`** — it
  is not written to any file under `<output_base>/server/` (only `command_port`,
  `request_cookie`, `response_cookie` are). Therefore `Cancel` is **not viable for the passive
  path at all**, and `use_cancel` falls back to SIGINT whenever the id is absent. It becomes
  viable **only** if the **E5 wrapper** (which issues the `Run`) captures and forwards the
  `command_id` to the broker — a real cross-epic dependency, not just a flag. **This means
  `use_cancel` in the `/kill` API (4.1) is effectively a no-op until E5 supplies command_ids;
  that should be called out so the endpoint doesn't promise a capability it can't deliver yet.**
  **Default carried forward: SIGINT; revisit `Cancel` jointly with E5.** **Decision owner:**
  Antonis.

### Edge cases & risks

- **Client vs server process distinction.** Both appear in `proc_listpids`. Mitigations
  (layered): exe-path allowlist (`isBazelClient`) **and** worktree resolution
  (server's cwd is the output base → no `.git` → dropped). Risk: a future bazel that runs
  the client from an embedded JDK path could be misclassified — covered by the e2e test
  (5.5) catching it against real bazel.
- **PID-reuse race.** A PID can exit and be reused between scan and kill. Mitigations (now
  enforced in code, 2.5): (1) **mandatory** exe-path re-validation in `Kill` GUARD 0 (not just
  "when killing by id") — a recycled pid pointing at a different binary is *refused*, not
  signalled; (2) `ESRCH` everywhere treated as gone==success, never escalate to a vanished pid;
  (3) `waitGone` stops fast on `EPERM` so a pid recycled to a foreign process cannot fake a
  "survived SIGKILL"; (4) reconcile-on-demand before `/kill` keeps the captured `ExePath` fresh.
  Residual window (recycled to *another* bazel client) is microseconds; accepted.
- **Sandbox / permissions (sudo-free constraint) — REVISED, this is a real caveat, not a
  non-issue.** Two libproc calls have *different* privilege profiles:
  - `proc_pidpath` on a process **we own** generally succeeds without privilege.
  - **`proc_pidinfo(PROC_PIDVNODEPATHINFO)` (the cwd) is the fragile one.** Even for a
    *same-user* target it can return `EPERM` when the target runs under the **hardened
    runtime**, has a restricted/`get-task-allow`-disabled signature, or is otherwise
    task-port-restricted — and it reliably needs **root** for *other users'* processes. Real
    `bazel`/`bazelisk` clients the developer launched are normally readable, but this is
    **not guaranteed** and must be handled, not assumed away. A bazel client whose cwd we
    cannot read is **discovered but unplaceable** (no worktree) and is dropped from `ls` — a
    real functional gap, not a cosmetic one.
  - **Mitigation / escalation:** the scanner skips such pids gracefully (typed
    `errProcUnavailable`) and logs once. **If field testing shows a non-trivial fraction of
    real bazel clients return `EPERM` on cwd, the fallbacks are: (a) `proc_pidpath` +
    `lsof -p <pid>`/`-d cwd` shell-out for cwd only (D-stack-1 partial flip), or (b) granting
    the broker the right entitlement / running it with elevated rights — which contradicts the
    "no sudo" goal.** This is **flagged to escalate** (see Staff review) because it can
    undermine the "no-wrapper discovery" value prop if it bites. No App Store sandbox (per
    02-epics: notarized cask, not sandboxed) — so *our* process is unrestricted; the risk is
    the *target's* restrictions, not ours. `syscall.Kill` to our own processes needs no sudo.
- **cgo build/portability.** Requires CGO_ENABLED=1 + macOS SDK; the broker binary becomes
  macOS-only. Mitigated by the non-darwin stub (compiles green for linters/CI matrix) and
  by isolating all cgo to one file. **Note:** because E2's store uses the *pure-Go*
  `modernc.org/sqlite` specifically to stay cgo-free, enabling cgo here makes the *broker*
  binary cgo-bound regardless — that trade-off belongs to D-stack-1 and is worth restating to
  the E2 owner.
- **Buffer sizing — exe path vs cwd are DIFFERENT.** The exe-path buffer must be
  `PROC_PIDPATHINFO_MAXSIZE` (`4*MAXPATHLEN` = 4096) — Apple requires it for `proc_pidpath` —
  while the cwd buffer is `MAXPATHLEN` (1024), the declared size of `vip_path`. The earlier
  draft's single 1024 buffer for both was a bug (corrected in 2.1). Paths longer than the
  kernel's own limits are truncated by libproc itself; a worktree path that doesn't resolve to
  a `.git` ancestor surfaces as `ErrNotInWorktree`.
- **Reconcile vs lifecycle ownership.** Discovery must not reap `registered` builds (E5 owns
  their `Deregister`). `ReapMissingDiscovered` only touches `source=="discovered"` builds;
  `Source` precedence prevents discovery from downgrading registered state.
- **Zombie / defunct bazel client.** A `<defunct>` client may linger; signal-0 (`pidGone`)
  still reports it as existing; SIGINT/SIGKILL are no-ops on a zombie (reaped by its parent).
  We classify a build whose PID is a zombie as **`StateUnknown`** (E2's enum — there is no
  `Gone` state) on the next reap.

---

## 7. Effort & internal ordering

**Critical path:** T1 → T2 → T3 → T4 → T6 → T7 → T8 → T9. T5 (E2 registry additions) must
land before T6 and is the one **cross-epic coordination** point (touches E2-owned code).
T10 is optional and off the critical path.

| Task | Est. | Notes |
|---|---|---|
| T1 scaffolding & interfaces | 0.5d | build tags, stubs |
| T2 cgo libproc wrapper | 1.0d | the only novel/risky bit (cgo + struct layout) |
| T3 bazel-client filter | 0.5d | + `BB_DISCOVERY_EXE_ALLOW` test hook |
| T4 worktree resolver | 0.5d | pure FS walk + `.git`-file parse |
| T5 registry additions (with E2) | 0.5d | coordinate; idempotent Upsert + reap + finders |
| T6 reconciler + ticker | 0.5d | fake-Scanner tests |
| T7 killer state machine | 0.5d | child-process tests |
| T8 `POST /kill` endpoint | 0.5d | wire into E2 mux |
| T9 e2e `make verify-e3` | 0.5d | the acceptance gate |
| T10 command-server Cancel (optional) | 1.0d | D4 spike; non-blocking |
| **Total (T1–T9)** | **~5.5d** | + ~1d if T10 is taken |

**Suggested order rationale:** prove discovery reads real PIDs (T2) before building anything
on top; land the worktree resolver (T4) in parallel-able isolation; gate the kill machine
(T7) on its own child-process tests before exposing it via HTTP (T8); only then run the full
acceptance script (T9). Defer T10/D4 until the E5 wrapper exists to supply `command_id`.

---

## Staff Engineer Review

*Reviewer: Staff Eng · Date: 2026-06-17 · Scope: macOS libproc correctness, kill-state-machine
safety, E2/E5 interface alignment, reconciliation dedupe.*

### (a) Verdict

**Approve with required changes — now applied in-place.** The architecture is sound and the
right things are behind interfaces (Scanner seam for D-stack-1, flag-gated Cancel for D4). But
the original draft shipped **two genuine macOS bugs** (exe-path buffer undersized; errno
collapsed so EPERM/ESRCH were indistinguishable), an **unsound client/server heuristic**, and
**several hard interface mismatches with E2's frozen contract** (`UpsertSpec` vs E2's
`Upsert(*build.Build)`, `int32` vs `int` PID, invented `Wrapper`/`Gone` enums, `disco:`
second key space, bare-array `jq`). These would not have compiled or interoperated as written.
With the edits below the plan is implementable and acceptance-test-credible. **One risk
(cwd EPERM under hardened runtime) is escalated, not closed** — it can dent the headline
"no-wrapper discovery" value if it bites in the field.

### (b) Top findings

1. **[libproc correctness — exe buffer]** `proc_pidpath` requires a buffer of
   `PROC_PIDPATHINFO_MAXSIZE` (`4*MAXPATHLEN` = **4096**), not `MAXPATHLEN` (1024). The draft's
   shared 1024 buffer can make `proc_pidpath` fail. Verified against the macOS 15/26 SDK
   `sys/proc_info.h`. **Fixed:** separate `pidPathMaxSize=4096` (exe) vs `maxPathLen=1024` (cwd).
2. **[libproc correctness — struct/field chain]** `proc_vnodepathinfo.pvi_cdir.vip_path`,
   `struct vnode_info_path`, `vip_path[MAXPATHLEN]`, flavor `PROC_PIDVNODEPATHINFO`(9) — **all
   verified correct** against the SDK. The `ret != sizeof(struct proc_vnodepathinfo)` success
   check is correct (this flavor returns the full struct size). Left intact, with the `==`
   tightened and errno captured.
3. **[libproc correctness — errno lost]** The shims collapsed every failure to `-1`, so the
   scanner could not tell ESRCH (gone → reap) from EPERM (alive but unreadable → skip + keep).
   The reaper and kill state machine both need that distinction. **Fixed:** shims write `errno`
   to an out-param; Go classifies into `errProcGone` / `errProcUnavailable`.
4. **[privilege caveat understated — ESCALATE]** The draft asserted "owned processes need no
   privileges, our targets are always owned." That is true for `proc_pidpath` but **not reliably
   for `proc_pidinfo(PROC_PIDVNODEPATHINFO)` (cwd)**, which can return EPERM even same-user under
   the hardened runtime / restricted task ports. Since cwd is *how we place a build in a
   worktree*, a denied cwd = a build we can see but not show. **Fixed framing + flagged to
   escalate** with concrete fallbacks (lsof-for-cwd shell-out, or entitlement).
5. **[client/server distinction unsound]** The draft excluded the server by assuming its exe is
   `java`/`/jdk/`. **Bazel's server runs from the embedded `…/install/…/bazel` launcher, not a
   bare JVM binary** — so exe matching can match both. **Fixed:** made cwd→worktree resolution
   the authoritative discriminator (server cwd = output base, no `.git`), demoted `isBazelClient`
   to a cheap pre-filter, and actually wired the `BB_DISCOVERY_EXE_ALLOW` override that was
   described but not implemented.
6. **[E2 interface mismatches — would not interoperate]** (a) E2 already defines
   `Upsert(b *build.Build)`, keyed by `InvocationID` (string); the draft invented `UpsertSpec`.
   (b) E2's `Build.PID` is `int`, draft used `int32` at the registry boundary. (c) Draft invented
   `Source{Wrapper}` and `State{Gone}`; E2's frozen enums are `{registered,discovered}` and use
   `StateUnknown` for vanished discovered builds. (d) `FindByPID` needs a PID secondary index E2
   doesn't have (string-keyed map → O(n)). (e) `/builds` returns `{"builds":[…]}`, not a bare
   array — the verify recipe's `jq` was wrong. **All fixed and flagged as T5 coordination.**
7. **[kill state machine — PID-reuse hardening]** The reuse guard was "best-effort, only when
   killing by id." **Hardened:** identity re-validation (`ExpectExe` vs live `proc_pidpath`) is
   now **mandatory GUARD 0 for every Kill**; `waitGone` no longer treats EPERM as "alive" and
   spins (which could fake "survived SIGKILL" against a recycled foreign pid) — it stops fast;
   the `force` flag from the API is now actually implemented (straight to SIGKILL).
8. **[D4 Cancel — API honesty]** `use_cancel` in `/kill` is effectively a **no-op** for a passive
   broker (no `command_id` available, not in the server dir). Flagged so the endpoint doesn't
   advertise a capability it can't yet deliver; the real fix is an E5 hand-off of `command_id`.

### (c) What I changed (all in-place, 7-section structure preserved)

- **§2.1:** dropped the bogus `-lproc` LDFLAGS (libproc is in libSystem); corrected exe buffer to
  4096; added errno out-params + `classifyErrno` → typed `errProcGone`/`errProcUnavailable`;
  tightened the vnodepathinfo size check; verified-signature table with return-value semantics;
  added a buffer-size correction callout.
- **§2.2:** rewrote the client/server distinction to rely on cwd resolution (correcting the
  wrong "server is java" claim); wired `BB_DISCOVERY_EXE_ALLOW`; typed-error skip in `Snapshot`.
- **§2.4:** added an E2-interface-mismatch callout (Upsert shape, `int` PID, PID index);
  rewrote `ReconcileOnce` to use E2's `Upsert(*build.Build)`, `int` PIDs, `StateUnknown` reap,
  `SourceDiscovered`, and a key-space-safe `synthDiscoID`; documented the synthetic-id reuse risk.
- **§2.5:** introduced `KillSpec` (PID `int`, `ExpectExe`, `Force`); mandatory GUARD 0 identity
  re-validation; `force` path; `pidGone`/`waitGone` that don't spin on EPERM; hardened
  PID-reuse note.
- **§4.1/§4.3:** `use_cancel`/`force` semantics callout; rewrote the fields table to E2's exact
  names/types/enums, marked NEW vs E2 rows, replaced `UpsertSpec`/`int32`/`Gone` with E2 shapes;
  noted wire-DTO edits need E2 sign-off.
- **§5:** fixed `jq` to `.builds[…]`; added GUARD 0 + `Force` killer tests.
- **§6:** rewrote the privilege caveat (real cwd-EPERM risk + fallbacks); corrected the
  PID-reuse, buffer-sizing, source/state, and zombie bullets; sharpened D-stack-1 (privilege is
  *not* a libproc-vs-lsof differentiator) and D4 (command_id is structurally unavailable to a
  passive broker) — both improved and explicitly marked **escalate**, not resolved.
- **§3:** updated T5 (E2 coordination) and T7 (`KillSpec`/`pidGone`) checkpoints.

### (d) Decisions / risks to escalate (NOT resolved here)

1. **D-stack-1 (cgo+libproc vs shell-out)** — *unresolved by design.* Recommendation
   (cgo+libproc) stands, but note it forces cgo into a binary E2 kept pure-Go, and the
   cwd-EPERM risk (below) may force a *partial* shell-out for cwd only. **Owner: Antonis.**
2. **D4 (SIGINT vs command-server Cancel)** — *unresolved by design.* Strengthened framing:
   Cancel is structurally impossible for the passive path (no `command_id`); it only becomes
   real via an E5 `command_id` hand-off. Ship SIGINT; revisit jointly with E5. **Owner: Antonis.**
3. **cwd `EPERM` under the hardened runtime (NEW risk surfaced).** If real `bazel`/`bazelisk`
   clients on the target Macs deny `PROC_PIDVNODEPATHINFO`, discovery can't place them in a
   worktree and the "no-wrapper trace/kill" value prop degrades. **Action: measure on real
   bazel early in T3/5.5; if non-trivial, pick a fallback (lsof-for-cwd vs entitlement vs
   accept-wrapper-only-placement).** Cheap to measure, expensive to discover late.
4. **T5 cross-epic contract edit.** E3 needs E2 to add a PID secondary index, the NEW Build
   fields, and possibly `ToWire()` changes for `worktree_name`/`cwd`. This **touches E2's frozen
   §4 contract** — needs the E2 owner's explicit sign-off before T6 can land.
5. **Synthetic `disco-<pid>` identity churn.** Acceptable for v1 but is a latent source of
   dedupe edge cases when a real `invocation_id` arrives or a PID is reused; worth a focused
   reconcile test matrix (pid-first-then-id, id-first-then-pid, pid-reuse-mid-window).
