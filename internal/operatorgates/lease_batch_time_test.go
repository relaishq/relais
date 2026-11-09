package operatorgates

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/relaishq/relais/internal/operatorgates/inventory"
	"github.com/relaishq/relais/pkg/operator"
	"github.com/stretchr/testify/require"
)

func TestLeaseSlotExpiry(t *testing.T) {
	// A slow peer delays collection, but cannot change another slot's expiry.
	for _, delay := range []time.Duration{300 * time.Millisecond, 600 * time.Millisecond} {
		c := &leaseTestClock{tick: time.Second}
		g, sample := leaseGuardFixture(t, c)
		g.leases = map[string]clocks.Lease{}
		s, _ := fixtureSession(t)
		s.clock, s.Budget.clock = c, c
		s.capabilities[operator.HostLeaseReadBatch] = true
		p := &observationPeer{respond: func(req operator.Request) operator.Reply {
			c.advance(delay)
			out := operator.HostReadBatchOutput{}
			for _, slot := range inventory.Slots {
				row := sample.Evidence.Clocks[slot]
				row.MeasuredUnixNS = row.Lease.MeasuredUnixNS
				raw, err := json.Marshal(row)
				require.NoError(t, err)
				child := hostReply(t, raw)
				child.ID = slot
				out.Replies = append(out.Replies, child)
			}
			return operator.Reply{Transport: operator.Succeeded, ExitCode: new(0), Payload: oracleJSON(out)}
		}}
		r, err := s.ReadLeases(context.Background(), p, time.Minute)
		require.NoError(t, err, "MUTATION: lease-batch-send")
		require.False(t, s.Latched(), "MUTATION: lease-batch-send")
		require.NoError(t, g.accept(r), "MUTATION: lease-batch-send")
		for _, slot := range inventory.Slots {
			v, err := g.leases[slot].At(c.Elapsed())
			require.NoError(t, err)
			wantExpiry := time.Second + 1900*time.Millisecond
			if time.Duration(v.RemainingNS)+c.Elapsed() != wantExpiry {
				t.Fatal("MUTATION: lease-batch-send", slot, delay, v.RemainingNS, wantExpiry)
			}
		}
	}
}
