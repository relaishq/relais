package nettopology

import (
	"errors"
	"net"
	"net/netip"
)

func listenCallerSocket(bind netip.AddrPort) (net.PacketConn, error) {
	if !bind.Addr().Is4() || bind.Addr().IsUnspecified() {
		return nil, errors.New("caller socket must bind a specific IPv4 UDP address")
	}
	return net.ListenUDP("udp4", net.UDPAddrFromAddrPort(bind))
}
