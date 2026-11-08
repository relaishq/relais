package operatorgates

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/operator"
)

func TestLeaseTransportKeepsScriptFloor(t *testing.T) {
	for _, op := range []operator.Operation{operator.HostExecute, operator.HostReadBatch} {
		s, _ := fixtureSession(t)
		s.capabilities[op] = true
		p := &observationPeer{respond: func(req operator.Request) operator.Reply { return reply(req, operator.Succeeded) }}
		certainty, err := s.Exchange(context.Background(), p, op, "hosts", "clock", nil, []byte("printf non-lease"), 3900*time.Millisecond, func(r operator.Reply) operator.Certainty { return r.Transport })
		if err != nil || certainty != operator.Failed || p.sent != 0 || s.Latched() {
			t.Fatal("MUTATION: lease-keeps-script-floor", op, certainty, err, p.sent)
		}
	}
}

func TestLeaseProbeFileScope(t *testing.T) {
	for _, mode := range []string{"empty", "configured", "other-path", "other-slot"} {
		t.Run(mode, func(t *testing.T) {
			s, _ := fixtureSession(t)
			s.capabilities[operator.HostLeaseRead] = true
			s.inventoryComponents = map[string][]string{"shared": {"signal"}}
			s.clockProbeFile = "/configured"
			slot, path := "shared", "/configured"
			switch mode {
			case "empty":
				path = ""
			case "other-path":
				path = "/other"
			case "other-slot":
				slot = "w1"
			}
			args, _ := json.Marshal(operator.HostLeaseReadArgs{Reads: []operator.HostLeaseReadInput{{Target: slot, ProbeFile: path}}})
			_, err := s.Issue(operator.HostLeaseRead, slot, "clock", args, nil, time.Minute)
			allowed := mode == "empty" || mode == "configured"
			if (err == nil) != allowed {
				t.Fatal("MUTATION: lease-probe-file", mode, err)
			}
		})
	}
}
