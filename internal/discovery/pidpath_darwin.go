//go:build darwin

package discovery

// livePidPath returns the current exe path for a pid, for kill-time identity
// re-validation (GUARD 0). It returns errProcGone / errProcUnavailable typed errors.
func livePidPath(pid int32) (string, error) { return pidPath(pid) }
