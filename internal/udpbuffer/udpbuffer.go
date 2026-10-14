// Package udpbuffer sizes the kernel buffers of the media path's UDP sockets
// and reads back what the kernel granted. A socket on the OS default receive
// buffer drops datagrams in the kernel when its read loop stalls briefly.
//
// Sizing is best effort and never fails a socket. Linux clamps a request to
// net.core.rmem_max (wmem_max for sends) without an error; macOS clamps to
// kern.ipc.maxsockbuf. The binaries log both sizes at startup and warn on a
// clamp, so an operator can raise the host limit.
package udpbuffer

import (
	"errors"
	"fmt"
	"net"
	"runtime"
)

// Size is a buffer request in bytes, as passed to setsockopt.
type Size struct {
	Read, Write int
}

// Requests by socket role. Relay sockets carry every call; a worker socket
// must absorb hold-release bursts for many sessions at once; a caller
// receives one call's echo. The kernel charges memory only for queued
// datagrams, so a large limit costs little while the reader keeps up.
var (
	Relay  = Size{Read: 16 << 20, Write: 4 << 20}
	Worker = Size{Read: 8 << 20, Write: 4 << 20}
	Caller = Size{Read: 4 << 20, Write: 1 << 20}
)

// Result is what one socket requested and got.
type Result struct {
	Requested Size
	// Effective is the granted size in the units of the request. Linux
	// reports double the granted size through getsockopt (and ss), for its
	// bookkeeping overhead; Effective is half of that. Zero is unknown.
	Effective Size
	// Err joins sizing and read-back errors.
	Err error
}

// Short reports whether a known effective size is below its request.
func (r Result) Short() bool {
	return short(r.Effective.Read, r.Requested.Read) || short(r.Effective.Write, r.Requested.Write)
}

func short(effective, requested int) bool { return effective > 0 && effective < requested }

type sizer interface {
	SetReadBuffer(bytes int) error
	SetWriteBuffer(bytes int) error
}

// Apply requests size on conn and reads back the effective size. Errors are
// reported in the Result; conn keeps working on whatever buffers it has.
func Apply(conn net.PacketConn, size Size) Result {
	result := Result{Requested: size}
	s, ok := conn.(sizer)
	if !ok {
		result.Err = fmt.Errorf("udpbuffer: %T cannot set buffer sizes", conn)
		return result
	}
	setErr := errors.Join(s.SetReadBuffer(size.Read), s.SetWriteBuffer(size.Write))
	effective, readErr := read(conn)
	result.Effective, result.Err = effective, errors.Join(setErr, readErr)
	return result
}

// Log writes one startup line for a socket, plus a warning for each buffer
// the kernel clamped and for any sizing error. logf is log.Printf in the
// binaries.
func Log(logf func(format string, args ...any), socket string, r Result) {
	logf("%s udp buffers: read effective=%s requested=%d, write effective=%s requested=%d",
		socket, sizeText(r.Effective.Read), r.Requested.Read, sizeText(r.Effective.Write), r.Requested.Write)
	if short(r.Effective.Read, r.Requested.Read) {
		logf("warning: %s udp receive buffer is %d bytes, below the %d requested; raise %s to %d or more on the host",
			socket, r.Effective.Read, r.Requested.Read, limit("rmem_max"), r.Requested.Read)
	}
	if short(r.Effective.Write, r.Requested.Write) {
		logf("warning: %s udp send buffer is %d bytes, below the %d requested; raise %s to %d or more on the host",
			socket, r.Effective.Write, r.Requested.Write, limit("wmem_max"), r.Requested.Write)
	}
	if r.Err != nil {
		logf("warning: %s udp buffers: %v", socket, r.Err)
	}
}

func sizeText(n int) string {
	if n <= 0 {
		return "unknown"
	}
	return fmt.Sprint(n)
}

// limit names the host setting that caps a request.
func limit(linux string) string {
	switch runtime.GOOS {
	case "linux":
		return "net.core." + linux
	case "darwin":
		return "kern.ipc.maxsockbuf"
	default:
		return "the OS socket buffer limit"
	}
}
