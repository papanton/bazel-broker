//go:build darwin

package discovery

/*
// libproc's symbols live in libSystem, which is linked automatically — there is no
// libproc.dylib to find, so we add no "#cgo LDFLAGS: -lproc".
#include <libproc.h>
#include <sys/proc_info.h>
#include <sys/errno.h>
#include <stdlib.h>
#include <string.h>

// Thin C shims. Each captures errno into *err so the Go side can distinguish EPERM
// (alive but unreadable -> skip, keep) from ESRCH (gone -> reap) from a genuine
// failure. Collapsing both to -1 loses a distinction the reaper and killer need.

// Enumerate all PIDs. Returns bytes written (or <0 on error); errno -> *err.
static int bb_list_all_pids(int *pids, int cap_pids, int *err) {
    *err = 0;
    int n = proc_listpids(PROC_ALL_PIDS, 0, (void *)pids, cap_pids * (int)sizeof(int));
    if (n <= 0) *err = errno;
    return n;
}

// Size probe: bytes the kernel would write (NULL buffer). errno -> *err.
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
// On success copies the NUL-terminated cwd into out and returns 0; on error returns
// -1 and sets *err to errno. proc_pidinfo for this flavor returns sizeof(struct
// proc_vnodepathinfo) exactly on success, so we require == (not >=).
static int bb_pid_cwd(int pid, char *out, int outsize, int *err) {
    struct proc_vnodepathinfo vpi;
    *err = 0;
    memset(&vpi, 0, sizeof(vpi));
    int ret = proc_pidinfo(pid, PROC_PIDVNODEPATHINFO, 0, &vpi, (int)sizeof(vpi));
    if (ret != (int)sizeof(vpi)) {
        *err = errno;                 // EPERM (not readable), ESRCH (gone), or short copy
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
	"fmt"
	"unsafe"
)

const (
	maxPathLen     = 1024 // MAXPATHLEN: the size of vip_path (cwd buffer)
	pidPathMaxSize = 4096 // PROC_PIDPATHINFO_MAXSIZE (4*MAXPATHLEN): required for proc_pidpath
)

// listPIDs returns every PID currently known to the kernel.
func listPIDs() ([]int32, error) {
	var cerr C.int
	nbytes := int(C.bb_count_all_pids(&cerr))
	if nbytes <= 0 {
		return nil, fmt.Errorf("proc_listpids size probe failed: %w", classifyErrno(cerr))
	}
	// Over-allocate by ~64 slots: PIDs can appear between the probe and the read.
	capN := nbytes/int(unsafe.Sizeof(C.int(0))) + 64
	buf := make([]int32, capN)
	written := int(C.bb_list_all_pids(
		(*C.int)(unsafe.Pointer(&buf[0])), C.int(capN), &cerr))
	if written <= 0 {
		return nil, fmt.Errorf("proc_listpids read failed: %w", classifyErrno(cerr))
	}
	n := written / int(unsafe.Sizeof(C.int(0)))
	if n > capN {
		n = capN // defensive: never index past the buffer if the kernel grew the list
	}
	out := make([]int32, 0, n)
	for _, p := range buf[:n] {
		if p > 0 { // kernel pads with zero entries
			out = append(out, p)
		}
	}
	return out, nil
}

// pidPath returns the absolute executable path for a PID. Buffer is
// PROC_PIDPATHINFO_MAXSIZE (4096) as Apple requires. Returns a typed error
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
// This flavor is the one most likely to return EPERM even for same-user processes
// under the hardened runtime / restricted task ports (OD-D). On this Mac (macOS 26)
// same-user cwd reads succeed; see the package doc.
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
