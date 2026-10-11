package main

import (
	"github.com/relais/pkg/callharness"
	"net/netip"
)

func externalTopology(signaling string, relay netip.AddrPort) callharness.ExternalTopology {
	return callharness.ExternalTopology{SignalingURL: signaling, RelayAddr: relay}
}
