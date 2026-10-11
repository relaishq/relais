package processidentity

import (
	"bytes"
	"context"
	"encoding/binary"
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
	if err := localScope(id); err != nil {
		return err
	}
	return fence(ctx, id, inspect, func(pid int) error {
		if err := verifyRecordedExecutable(pid, id.Executable); err != nil {
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
	if _, err := procInfo(pid, 11, 0, buf); err != nil {
		return "", err
	}
	end := bytes.IndexByte(buf, 0)
	if end <= 0 {
		return "", errors.New("empty process executable path")
	}
	return string(buf[:end]), nil
}

func gonePlatform(ctx context.Context, id Identity) (bool, error) {
	if err := localScope(id); err != nil {
		return false, err
	}
	return gone(ctx, id, inspect, func(pid int) error { return verifyRecordedExecutable(pid, id.Executable) })
}

func procInfo(pid, flavor int, address uint64, buf []byte) (int, error) {
	// x/sys does not expose proc_pidinfo. Read kernel vnode data without CGO.
	//nolint:staticcheck // SA1019: the required proc_info wrapper is not exposed by x/sys.
	n, _, errno := unix.Syscall6(unix.SYS_PROC_INFO, 2, uintptr(pid), uintptr(flavor), uintptr(address), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if errno != 0 {
		return 0, errno
	}
	return int(n), nil
}
func scopePlatform() (string, string, error) {
	var uuid [16]byte
	timeout := unix.Timespec{Sec: 1}
	// kern.uuid is the kernel build UUID. gethostuuid reads the machine UUID.
	//nolint:staticcheck // SA1019: x/sys has no gethostuuid wrapper.
	_, _, errno := unix.Syscall(unix.SYS_GETHOSTUUID, uintptr(unsafe.Pointer(&uuid[0])), uintptr(unsafe.Pointer(&timeout)), 0)
	if errno != 0 {
		return "", "", errno
	}
	if uuid == ([16]byte{}) {
		return "", "", errors.New("empty Darwin host UUID")
	}
	return fmt.Sprintf("darwin:%x", uuid), "darwin", nil
}
func executableIdentity(pid int) (string, error) {
	path, err := processPath(pid)
	if err != nil {
		return "", err
	}
	// proc_regionwithpathinfo: regioninfo(96), vnode_info(152), path(1024).
	// Read the mapped vnode, not stat(path), which can resolve an upgraded file.
	buf := make([]byte, 1272)
	var address uint64
	for range 128 {
		n, err := procInfo(pid, 22, address, buf) // PROC_PIDREGIONPATHINFO2: mapped vnodes only.
		if err != nil {
			return "", err
		}
		if n != len(buf) {
			return "", errors.New("short Darwin region vnode reply")
		}
		end := bytes.IndexByte(buf[248:], 0)
		if end >= 0 && string(buf[248:248+end]) == path {
			dev, ino := binary.LittleEndian.Uint32(buf[96:]), binary.LittleEndian.Uint64(buf[104:])
			if ino == 0 {
				return "", errors.New("empty Darwin executable vnode")
			}
			return fmt.Sprintf("%x:%x", dev, ino), nil
		}
		next := binary.LittleEndian.Uint64(buf[80:]) + binary.LittleEndian.Uint64(buf[88:])
		if next <= address {
			break
		}
		address = next
	}
	return "", errors.New("cannot find Darwin executable vnode")
}
