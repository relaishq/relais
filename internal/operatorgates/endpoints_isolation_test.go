package operatorgates

import (
	"fmt"
	"testing"
)

func TestPrivateEndpointsRejectFaultExceptions(t *testing.T) {
	for _, port := range []int{443, 53} {
		for _, target := range []string{"control", "relay", "relay-two", "store"} {
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
			}
			if err := ValidateProcessParameters(g.c.Parameters); err == nil {
				t.Fatal("private endpoint bypasses isolation", target)
			}
		}
	}
}
