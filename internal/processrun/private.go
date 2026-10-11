package processrun

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
)

// ValidatePrivateListener allows loopback by default, plus explicitly allowed
// RFC 1918 or unique-local prefixes. A broad prefix cannot admit public IPs.
func ValidatePrivateListener(addr, allowed string) error {
	var prefixes []netip.Prefix
	if allowed != "" {
		private := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("172.16.0.0/12"), netip.MustParsePrefix("192.168.0.0/16"), netip.MustParsePrefix("fc00::/7")}
		for _, entry := range strings.Split(allowed, ",") {
			p, err := netip.ParsePrefix(strings.TrimSpace(entry))
			valid := false
			if err == nil {
				for _, limit := range private {
					valid = valid || (limit.Contains(p.Addr()) && p.Bits() >= limit.Bits())
				}
			}
			if !valid {
				return fmt.Errorf("RELAIS_PRIVATE_NETS must contain private CIDR prefixes: %q", entry)
			}
			prefixes = append(prefixes, p.Masked())
		}
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || ip.Zone() != "" || ip.IsUnspecified() {
		return errors.New("private HTTP listener must use a literal loopback or explicitly allowed private IP")
	}
	ip = ip.Unmap()
	if ip.IsLoopback() {
		return nil
	}
	for _, p := range prefixes {
		if p.Contains(ip) {
			return nil
		}
	}
	return errors.New("private HTTP listener must use a literal loopback or explicitly allowed private IP")
}
