package main

import (
	"fmt"
	"net/netip"
)

func validateListeners(privateNet string, endpoints ...string) error {
	var prefix netip.Prefix
	if privateNet != "" {
		var err error
		prefix, err = netip.ParsePrefix(privateNet)
		private := false
		if err == nil {
			for _, allowed := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"} {
				block := netip.MustParsePrefix(allowed)
				private = private || (prefix.Bits() >= block.Bits() && block.Contains(prefix.Addr()))
			}
		}
		if !private {
			return fmt.Errorf("-spike-private-net must be an RFC1918 IPv4 CIDR")
		}
	}
	for _, endpoint := range endpoints {
		address, err := netip.ParseAddrPort(endpoint)
		if err != nil || address.Addr().IsUnspecified() || (!address.Addr().IsLoopback() && !prefix.Contains(address.Addr())) {
			return fmt.Errorf("listener %q requires a literal loopback IP or an IP within -spike-private-net", endpoint)
		}
	}
	return nil
}
