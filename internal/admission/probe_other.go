//go:build !darwin

package admission

// NewLoadProbe on non-darwin platforms reports an idle machine: the broker is a
// macOS daemon, so this exists only to keep `go build ./...` green elsewhere.
func NewLoadProbe() LoadProbe { return nopProbe{} }
