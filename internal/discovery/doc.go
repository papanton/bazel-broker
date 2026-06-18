// Package discovery finds running Bazel client processes via libproc (cgo, darwin)
// and resolves each one to a git worktree, so the broker can trace and kill builds
// with NO wrapper required.
//
// Layout:
//   - scanner.go         Scanner interface, ProcInfo, isBazelClient filter (pure Go)
//   - libproc_darwin.go  cgo libproc bindings: listPIDs / pidPath / pidCwd
//   - errno_darwin.go    EPERM vs ESRCH classification (kept distinct)
//   - scanner_darwin.go  the libproc-backed Scanner (darwin)
//   - scanner_stub.go    non-darwin stub returning ErrUnsupported (keeps CI green)
//   - worktree.go        cwd -> git worktree root/name/gitdir resolution
//   - reconcile.go       periodic scan -> registry.Upsert / ReapMissingDiscovered
//   - kill.go            SIGTERM/SIGINT -> grace -> SIGKILL state machine + Killer
//
// Client vs server: both the bazel client and the JVM server appear in the PID list,
// and exe-path matching can match both (the server runs from the embedded launcher,
// not a bare JVM). The AUTHORITATIVE discriminator is the cwd: the client's cwd is the
// worktree, the server's cwd is the output base (no reachable .git -> dropped by the
// reconciler). isBazelClient is only the cheap first-pass filter.
//
// Integration recipe (orchestrator adds these to cmd/broker/main.go; no E3 file edits
// to main.go itself). After `srv := httpapi.New(...)` is constructed and before/at
// Serve, insert:
//
//	disco := discovery.NewReconciler(discovery.NewScanner(), reg, log, discovery.DefaultInterval)
//	killer := discovery.NewKiller(reg, discovery.KillConfig{}, log, disco.ReconcileOnce)
//	srv = httpapi.New(cfg, reg, hub, log, httpapi.WithVersion(version.Version), httpapi.WithKiller(killer))
//	go disco.Run(ctx) // ctx is the SIGINT/SIGTERM-cancelled context already in run()
//
// (The killer must be passed as a WithKiller option at New time; the reconciler loop is
// started with `go disco.Run(ctx)`.)
//
// OD-D spike (measured on this Mac, macOS 26 / Darwin 25.3, Go 1.26, cgo on):
// same-user proc_pidinfo(PROC_PIDVNODEPATHINFO) cwd reads SUCCEED and are NOT denied
// under the hardened runtime. errno is EPERM(1) only for foreign/root-owned processes
// (e.g. launchd) and ESRCH(3) for vanished pids — exactly the EPERM != ESRCH split the
// design relies on. The no-wrapper discovery value-prop therefore holds on this host.
package discovery
