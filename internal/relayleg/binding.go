package relayleg

import (
	"encoding/hex"
	"net/netip"
)

const Hello byte = 2
const Observed byte = 3
const Keepalive byte = 4

// Binding frames are distinct from legacy barriers and caller-address headers.
// Tokens are 128 random bits issued by control, never by a worker.
func Binding(kind byte, token string, address netip.AddrPort) []byte {
	key, err := hex.DecodeString(token)
	if err != nil || len(key) != 16 {
		return nil
	}
	b := append([]byte{'R', 'L', 'S', 2, kind}, key...)
	if kind == Observed {
		b = append(b, []byte(address.String())...)
	}
	return b
}
func ParseBinding(b []byte) (byte, string, netip.AddrPort, bool) {
	if len(b) < 21 || string(b[:4]) != "RLS\x02" || b[4] < Hello || b[4] > Keepalive {
		return 0, "", netip.AddrPort{}, false
	}
	var addr netip.AddrPort
	if b[4] == Observed {
		var err error
		addr, err = netip.ParseAddrPort(string(b[21:]))
		if err != nil {
			return 0, "", addr, false
		}
	} else if len(b) != 21 {
		return 0, "", addr, false
	}
	return b[4], hex.EncodeToString(b[5:21]), addr, true
}
