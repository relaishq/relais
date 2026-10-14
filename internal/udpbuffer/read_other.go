//go:build !unix

package udpbuffer

import "net"

// read cannot report sizes here; Log prints them as unknown.
func read(net.PacketConn) (Size, error) { return Size{}, nil }
