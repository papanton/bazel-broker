//go:build darwin

package admission

import (
	"encoding/binary"

	"golang.org/x/sys/unix"
)

// NewLoadProbe returns the default macOS load probe. Pure-Go (no cgo): CPU busy%
// is derived from the 1-minute load average normalized by core count
// (sysctl vm.loadavg + hw.ncpu); RAM pressure is the OS's own pressure level
// (sysctl kern.memorystatus_vm_pressure_level). Both are cheap sysctls.
//
// Load-average-as-CPU is a coarse proxy (it counts runnable+uninterruptible, not
// pure CPU), but it needs no per-call diffing or priming and is honest about an
// oversubscribed machine, which is exactly what admission cares about. The
// cgo host_processor_info path (exact busy-tick %) stays behind this same
// interface as a documented swap-in if the proxy proves too coarse.
func NewLoadProbe() LoadProbe { return &darwinProbe{} }

type darwinProbe struct{}

// loadavg mirrors the kernel `struct loadavg`: 3 fixed-point loads + the scale
// (fscale) used as the divisor. Layout is [3]uint32 ldavg + uint32 fscale on
// 64-bit darwin (the long fscale packs into 4 bytes for the sysctl payload).
func (darwinProbe) Sample() (float64, int, error) {
	cpu := 0.0
	if raw, err := unix.Sysctl("vm.loadavg"); err == nil && len(raw) >= 16 {
		b := []byte(raw)
		ld1 := binary.LittleEndian.Uint32(b[0:4])
		fscale := binary.LittleEndian.Uint32(b[12:16])
		if fscale > 0 {
			load1 := float64(ld1) / float64(fscale)
			ncpu := 1
			if n, err := unix.SysctlUint32("hw.ncpu"); err == nil && n > 0 {
				ncpu = int(n)
			}
			cpu = load1 / float64(ncpu)
			if cpu > 1 {
				cpu = 1
			}
		}
	}

	ramP := 1 // normal
	if lvl, err := unix.SysctlUint32("kern.memorystatus_vm_pressure_level"); err == nil && lvl > 0 {
		ramP = int(lvl)
	}
	return cpu, ramP, nil
}
