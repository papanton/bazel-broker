//go:build darwin

package discovery

// libprocScanner is the darwin/cgo Scanner implementation.
type libprocScanner struct{}

// NewScanner returns the libproc-backed Scanner on darwin.
func NewScanner() Scanner { return libprocScanner{} }

func (libprocScanner) Snapshot() ([]ProcInfo, error) {
	pids, err := listPIDs()
	if err != nil {
		return nil, err
	}
	var out []ProcInfo
	for _, pid := range pids {
		path, err := pidPath(pid)
		if err != nil {
			continue // EPERM (unreadable) or ESRCH (gone mid-scan) -> skip this PID
		}
		if !isBazelClient(path) {
			continue
		}
		cwd, err := pidCwd(pid)
		if err != nil {
			// Matched on exe but cwd unreadable (EPERM under hardened runtime, OD-D) or
			// just exited. A build we can see but not place in a worktree is not
			// actionable, so skip it. On macOS 26 same-user cwd reads succeed (OD-D
			// spike), so this is the rare path.
			continue
		}
		out = append(out, ProcInfo{PID: pid, ExePath: path, Cwd: cwd})
	}
	return out, nil
}
