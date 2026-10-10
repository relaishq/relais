package nettopology

import (
	"net/netip"
	"strings"
)

func overlapsRoutes(p Plan, routes string) bool {
	for _, line := range strings.Split(routes, "\n") {
		fields := strings.Fields(line)
		for _, field := range fields {
			prefix, err := netip.ParsePrefix(field)
			if err != nil {
				if ip, parseErr := netip.ParseAddr(field); parseErr == nil {
					prefix = netip.PrefixFrom(ip, ip.BitLen())
				} else {
					continue
				}
			}
			if prefix.Bits() > 0 && (prefix.Overlaps(p.Datacentre) || prefix.Overlaps(p.CallerNet)) {
				return true
			}
			break
		}
	}
	return false
}
