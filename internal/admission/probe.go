package admission

// LoadProbe reports machine load for the token-bucket gate.
//
// Sample returns cpuBusy in [0,1] (1 = fully busy) and the macOS memory-pressure
// level (1=normal, 2=warn, 4=critical). The RAM signal is deliberately the OS's
// own pressure level (sysctl kern.memorystatus_vm_pressure_level), NOT a
// used-memory fraction: on macOS, used% sits at 80-90% on a healthy idle Mac
// (inactive/compressed pages), so gating on it would refuse admission constantly.
type LoadProbe interface {
	Sample() (cpuBusy float64, ramPressure int, err error)
}

// nopProbe always reports an idle machine (used when load gating is disabled).
type nopProbe struct{}

func (nopProbe) Sample() (float64, int, error) { return 0, 1, nil }
