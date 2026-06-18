//go:build !darwin

package discovery

// stubScanner keeps the broker compiling on non-darwin (CI/Linux). Snapshot always
// returns ErrUnsupported; the reconciler treats that as "no discovery on this OS".
type stubScanner struct{}

// NewScanner returns the non-darwin stub.
func NewScanner() Scanner { return stubScanner{} }

func (stubScanner) Snapshot() ([]ProcInfo, error) { return nil, ErrUnsupported }
