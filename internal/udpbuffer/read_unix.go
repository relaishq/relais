//go:build unix

package udpbuffer

import (
	"errors"
	"fmt"
	"net"
	"runtime"
	"syscall"

	"golang.org/x/sys/unix"
)

// read returns the socket's buffer sizes through its descriptor. Go's
// SetReadBuffer and SetWriteBuffer do not report a clamp.
func read(conn net.PacketConn) (Size, error) {
	sc, ok := conn.(syscall.Conn)
	if !ok {
		return Size{}, fmt.Errorf("udpbuffer: %T has no descriptor to read sizes from", conn)
	}
	raw, err := sc.SyscallConn()
	if err != nil {
		return Size{}, fmt.Errorf("udpbuffer: %w", err)
	}
	var size Size
	var readErr, writeErr error
	err = raw.Control(func(fd uintptr) {
		size.Read, readErr = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_RCVBUF)
		size.Write, writeErr = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_SNDBUF)
	})
	if err := errors.Join(err, readErr, writeErr); err != nil {
		return Size{}, fmt.Errorf("udpbuffer: read sizes: %w", err)
	}
	if runtime.GOOS == "linux" {
		// The kernel stores and reports double the request (socket(7)).
		size.Read /= 2
		size.Write /= 2
	}
	return size, nil
}
