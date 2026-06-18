//go:build !darwin

package discovery

// livePidPath is unavailable off darwin (proc_pidpath is cgo); kill.go's GUARD 0 treats
// errUnsupportedPidPath as "skip the identity check".
func livePidPath(pid int32) (string, error) { return "", errUnsupportedPidPath }
