package relay

import (
	"encoding/binary"
	"errors"
	"net/netip"
)

// The relay leg is the private UDP path between the relay and the media
// workers. Every packet on it travels in its own datagram behind a small
// header that names a caller's address. From the relay to a worker the
// header says which caller sent the packet; from a worker to the relay it
// says which caller to send the packet to. The caller's packet follows the
// header unchanged.
//
//	 0                   1                   2                   3
//	 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
//	+---------------+---------------+-------------------------------+
//	|  version = 1  | family: 4 / 6 |   caller UDP port (big end.)  |
//	+---------------+---------------+-------------------------------+
//	|     caller IP: 4 bytes for family 4, 16 bytes for family 6    |
//	+---------------------------------------------------------------+
//	|     the caller's packet (STUN, DTLS, SRTP or SRTCP), as is    |
//	+---------------------------------------------------------------+
//
// The header is 8 bytes for an IPv4 caller and 20 for an IPv6 caller. An
// IPv4-mapped IPv6 address travels as IPv4, and IPv6 zones are not carried.
// A receiver drops a datagram with an unknown version or family, or one too
// short for its header. Any change to the layout takes a new version.

const (
	// HeaderVersion is the version of the relay-leg header this package
	// writes and accepts.
	HeaderVersion = 1

	// MaxHeaderLen is the longest relay-leg header: an IPv6 caller.
	MaxHeaderLen = headerFixedLen + 16

	headerFixedLen = 4
	familyIPv4     = 4
	familyIPv6     = 6
)

// ErrMalformedHeader is returned for a relay-leg datagram whose header is
// too short or has an unknown version or address family.
var ErrMalformedHeader = errors.New("relay: malformed relay-leg header")

// HeaderLen is the length of the relay-leg header for a caller address.
func HeaderLen(caller netip.AddrPort) int {
	if caller.Addr().Unmap().Is4() {
		return headerFixedLen + 4
	}

	return headerFixedLen + 16
}

// AppendHeader appends the relay-leg header for a caller address to dst and
// returns the extended slice. The caller's packet goes right after it.
func AppendHeader(dst []byte, caller netip.AddrPort) []byte {
	ip := caller.Addr().Unmap()
	if ip.Is4() {
		dst = append(dst, HeaderVersion, familyIPv4)
		dst = binary.BigEndian.AppendUint16(dst, caller.Port())
		v4 := ip.As4()

		return append(dst, v4[:]...)
	}
	dst = append(dst, HeaderVersion, familyIPv6)
	dst = binary.BigEndian.AppendUint16(dst, caller.Port())
	v6 := ip.As16()

	return append(dst, v6[:]...)
}

// ParseHeader splits a relay-leg datagram into the caller address in its
// header and the caller's packet that follows. The packet aliases datagram.
func ParseHeader(datagram []byte) (caller netip.AddrPort, packet []byte, err error) {
	if len(datagram) < headerFixedLen || datagram[0] != HeaderVersion {
		return netip.AddrPort{}, nil, ErrMalformedHeader
	}

	port := binary.BigEndian.Uint16(datagram[2:4])
	var ip netip.Addr
	var n int
	switch datagram[1] {
	case familyIPv4:
		n = headerFixedLen + 4
		if len(datagram) < n {
			return netip.AddrPort{}, nil, ErrMalformedHeader
		}
		ip = netip.AddrFrom4([4]byte(datagram[headerFixedLen:n]))
	case familyIPv6:
		n = headerFixedLen + 16
		if len(datagram) < n {
			return netip.AddrPort{}, nil, ErrMalformedHeader
		}
		ip = netip.AddrFrom16([16]byte(datagram[headerFixedLen:n]))
	default:
		return netip.AddrPort{}, nil, ErrMalformedHeader
	}

	return netip.AddrPortFrom(ip, port), datagram[n:], nil
}
