package main

import (
	"errors"
	"github.com/relais/internal/nettopology"
	"github.com/relais/pkg/callharness"
	"net"
	"net/netip"
)

func externalTopology(signaling string, relay netip.AddrPort) callharness.ExternalTopology {
	return callharness.ExternalTopology{SignalingURL: signaling, RelayAddr: relay}
}

func childTopology(signaling string, relay netip.AddrPort, namespace, bind string) (callharness.ExternalTopology, error) {
	external := externalTopology(signaling, relay)
	if namespace == "" && bind == "" {
		return external, nil
	}
	addr, err := netip.ParseAddrPort(bind)
	if namespace == "" || err != nil || !addr.Addr().Is4() || addr.Addr().IsUnspecified() {
		return external, errors.New("caller namespace requires a specific IPv4 UDP bind address")
	}
	external.CallerSocket = func() (net.PacketConn, error) {
		return nettopology.CallerSocketInNamespace(namespace, addr)
	}
	return external, nil
}
