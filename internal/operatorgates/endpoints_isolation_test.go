package operatorgates

import (
	"fmt"
	"net/netip"
	"testing"
)

func TestPrivateEndpointsRejectFaultExceptions(t *testing.T) {
	for _, port := range []int{443, 53} {
		for _, target := range []string{"control", "relay", "relay-two", "store", "w1-media", "w2-media"} {
			g, _ := testProcedure(t)
			switch target {
			case "control":
				g.c.Parameters.Control = fmt.Sprintf("http://192.0.2.1:%d", port)
			case "relay":
				g.c.Parameters.Relay = fmt.Sprintf("http://192.0.2.2:%d", port)
			case "relay-two":
				g.c.Parameters.RelayTwo = fmt.Sprintf("http://192.0.2.5:%d", port)
			case "store":
				g.c.Parameters.StorePort = port
			case "w1-media", "w2-media":
				worker := target[:2]
				endpoint := g.c.Parameters.Workers[worker]
				endpoint.Media = netip.MustParseAddrPort(fmt.Sprintf("192.0.2.3:%d", port))
				g.c.Parameters.Workers[worker] = endpoint
			}
			if err := ValidateProcessParameters(g.c.Parameters); err == nil {
				t.Fatal("private endpoint bypasses isolation", target)
			}
		}
	}
}
