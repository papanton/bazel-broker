package admission

import (
	"os"
	"syscall"

	"github.com/antoniospapantoniou/bazel-broker/internal/api"
	"github.com/antoniospapantoniou/bazel-broker/internal/build"
	"github.com/antoniospapantoniou/bazel-broker/internal/registry"
)

// RegistryAdapter bridges *registry.Registry to the engine's RegistryReader and
// TerminalEvents seams. It is the single integration point main.go constructs;
// the engine itself stays decoupled from the registry/store stack.
type RegistryAdapter struct {
	reg *registry.Registry
	hub *registry.Hub
}

// NewRegistryAdapter wraps the daemon's registry + hub for the engine.
func NewRegistryAdapter(reg *registry.Registry, hub *registry.Hub) *RegistryAdapter {
	return &RegistryAdapter{reg: reg, hub: hub}
}

// SnapshotActive returns the currently non-terminal builds (registered AND
// passively-discovered) so stagger/headroom account for un-wrapped builds too.
func (a *RegistryAdapter) SnapshotActive() RegistrySnapshot {
	var out RegistrySnapshot
	for _, b := range a.reg.Snapshot() {
		if b.State.IsTerminal() {
			continue
		}
		out.Builds = append(out.Builds, ActiveBuild{
			InvocationID: b.InvocationID,
			Targets:      b.Targets,
			PID:          b.PID,
			StartedAt:    b.StartTime,
		})
	}
	return out
}

// Alive reports whether pid is a live process (PID-liveness reaper backstop).
// Signal 0 probes existence without delivering a signal; ESRCH => gone.
func (a *RegistryAdapter) Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	// Prefer the registry's own view: a build the registry has reaped to a
	// terminal/gone state is no longer alive regardless of pid reuse.
	if b, ok := a.reg.FindByPID(pid); ok && b.State.IsTerminal() {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Signal 0 probes existence without delivering a signal; nil => alive,
	// ESRCH => gone. EPERM (alive but not ours) also counts as alive.
	err = proc.Signal(syscall.Signal(0))
	return err == nil || err == syscall.EPERM
}

// TerminalIDs subscribes to the registry Hub and emits the invocation_id of each
// build that transitions to a terminal state. This is what frees a slot
// server-side on deregister / BEP-finished / process-gone reconcile, so the
// wrapper's exec (which skips its EXIT trap) never leaks a slot.
func (a *RegistryAdapter) TerminalIDs() (<-chan string, func()) {
	if a.hub == nil {
		ch := make(chan string)
		return ch, func() {}
	}
	sub, unsub := a.hub.Subscribe()
	out := make(chan string, 64)
	done := make(chan struct{})
	go func() {
		defer close(out)
		for {
			select {
			case <-done:
				return
			case ev, ok := <-sub.Events():
				if !ok {
					return
				}
				if ev.Type == api.EventBuild && ev.Build != nil && build.State(ev.Build.State).IsTerminal() {
					select {
					case out <- ev.Build.InvocationID:
					case <-done:
						return
					}
				}
			}
		}
	}()
	cancel := func() {
		close(done)
		unsub()
	}
	return out, cancel
}

// Compile-time guards that the adapter satisfies the engine seams.
var (
	_ RegistryReader = (*RegistryAdapter)(nil)
	_ TerminalEvents = (*RegistryAdapter)(nil)
)
