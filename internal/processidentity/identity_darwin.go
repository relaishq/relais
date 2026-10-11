package processidentity

import (
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"os"
)

func inspect(pid int) (string, bool, error) {
	p, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		// x/sys returns EIO for the zero-length reply of a nonexistent PID.
		// Confirm absence; any other error must fail closed.
		if errors.Is(err, unix.ESRCH) || errors.Is(err, unix.EIO) && errors.Is(unix.Kill(pid, 0), unix.ESRCH) {
			return "", false, os.ErrNotExist
		}
		return "", false, err
	}
	return fmt.Sprintf("%d:%d", p.Proc.P_starttime.Sec, p.Proc.P_starttime.Usec), p.Proc.P_stat == 5, nil // XNU SZOMB
}
