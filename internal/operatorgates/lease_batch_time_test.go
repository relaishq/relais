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
		offsets := []int64{0, 50000000, 100000000, 150000000}
		p := &observationPeer{respond: func(req operator.Request) operator.Reply {
			c.advance(delay)
			out := operator.HostReadBatchOutput{SentAfterNS: offsets}
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
		require.NoError(t, err)
		require.NoError(t, g.accept(r))
		for i, slot := range inventory.Slots {
			v, err := g.leases[slot].At(c.Elapsed())
			require.NoError(t, err)
			wantExpiry := time.Second + time.Duration(offsets[i]) + 1900*time.Millisecond
			if time.Duration(v.RemainingNS)+c.Elapsed() != wantExpiry {
				t.Fatal("MUTATION: lease-slot-send", slot, delay, v.RemainingNS, wantExpiry)
			}
		}
	}
}

func TestLeaseBatchTimingMalformed(t *testing.T) {
	for _, offsets := range [][]int64{nil, {0, 0, 0}, {0, 0, 0, 0, 0}, {-1, 0, 0, 0}, {0, 0, 0, 100000001}} {
		s, c := fixtureSession(t)
		s.capabilities[operator.HostLeaseReadBatch] = true
		p := &observationPeer{respond: func(req operator.Request) operator.Reply {
			c.Advance(100 * time.Millisecond)
			out := operator.HostReadBatchOutput{SentAfterNS: offsets}
			for _, slot := range inventory.Slots {
				child := hostReply(t, []byte(`{"errorBoundNs":0,"rateErrorPpm":0,"measuredUnixNs":"1793930000000000000"}`))
				child.ID = slot
				out.Replies = append(out.Replies, child)
			}
			return operator.Reply{Transport: operator.Succeeded, ExitCode: new(0), Payload: oracleJSON(out)}
		}}
		_, err := s.ReadLeases(context.Background(), p, time.Minute)
		if err == nil || !s.Latched() {
			t.Fatal("MUTATION: lease-batch-timing", offsets, err, s.Latched())
		}
	}
}
