package operatorgates

import (
	"context"
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
