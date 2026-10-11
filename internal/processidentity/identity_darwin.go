package processidentity

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"os"
	"unsafe"
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

func fencePlatform(ctx context.Context, id Identity) error {
	return fence(ctx, id, inspect, func(pid int) error {
		target, err := processPath(pid)
		if err != nil {
			return err
		}
		self, err := processPath(os.Getpid())
		if err != nil {
			return err
		}
		if err := sameExecutable(target, self); err != nil {
			return err
		}
		// macOS has no pidfd. Recheck start time after the executable lookup.
		start, dead, err := inspect(pid)
		if err != nil {
			return err
		}
		if start != id.Start || dead {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		return unix.Kill(pid, unix.SIGKILL)
	})
}

// PROC_INFO_CALL_PIDINFO=2, PROC_PIDPATHINFO=11: the kernel executable vnode
// path, not argv[0] or the mutable kern.procargs2 user stack.
func processPath(pid int) (string, error) {
	buf := make([]byte, 4096)
	// x/sys has no libSystem proc_pidpath wrapper. Keep this kernel vnode
	// lookup available without CGO, rather than trusting mutable argv data.
	//nolint:staticcheck // SA1019: the required proc_info wrapper is not exposed by x/sys.
	_, _, errno := unix.Syscall6(unix.SYS_PROC_INFO, 2, uintptr(pid), 11, 0, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if errno != 0 {
		return "", errno
	}
	end := bytes.IndexByte(buf, 0)
	if end <= 0 {
		return "", errors.New("empty process executable path")
	}
	return string(buf[:end]), nil
}
