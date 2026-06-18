//go:build darwin

package discovery

/*
#include <sys/errno.h>
*/
import "C"

import (
	"fmt"
	"syscall"
)

func classifyErrno(e C.int) error {
	switch syscall.Errno(e) {
	case syscall.ESRCH:
		return errProcGone
	case syscall.EPERM, syscall.EACCES:
		return errProcUnavailable
	default:
		return fmt.Errorf("discovery: libproc errno %d", int(e))
	}
}
