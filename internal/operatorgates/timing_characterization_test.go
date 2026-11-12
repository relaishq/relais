package operatorgates

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/operator"
	"github.com/stretchr/testify/require"
)

// The reference timing (one-second poll, local host reads). These values were
// fixed constants before the timing budget; the budget must derive the same.
const (
	leaseReadAttempt      = 1800 * time.Millisecond
	leaseReadMinimum      = 550 * time.Millisecond
	leaseReadBatchMinimum = 1600 * time.Millisecond
)

// slowLeaseReads makes every lease read take delay on the session clock.
func slowLeaseReads(r *ProcessRuntime, p *observationPeer, delay time.Duration) {
	clock := r.Session.clock.(*FakeClock)
	base := p.respond
	p.respond = func(q operator.Request) operator.Reply {
		if q.Operation == operator.HostLeaseRead || q.Operation == operator.HostLeaseReadBatch {
			clock.Advance(delay)
		}
		return base(q)
	}
}

// Characterization, written before the timing budget: at the reference timing
// a lease read is unknown once its reply is later than the 1.8 s attempt, and
// an unknown lease read latches the session without a second submission.
func TestReferenceLeaseReadLatchCharacterization(t *testing.T) {
	for _, tc := range []struct {
		name    string
		delay   time.Duration
		unknown bool
		latched bool
		rule    string
	}{
		// Four serial reads must also end inside the first lease's five-second validity.
		{name: "in-time", delay: time.Second},
		{name: "late-reply", delay: 1790 * time.Millisecond, latched: true, rule: "timeout"},
		{name: "unknown-transport", unknown: true, latched: true, rule: "transport-unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, p, _ := runtimeFixture(t)
			// The fixture adds 20 ms of its own to every lease read.
			slowLeaseReads(r, p, tc.delay)
			if tc.unknown {
				p.respond = func(q operator.Request) operator.Reply { return operator.Reply{Transport: operator.Unknown} }
			}
			err := r.Leases.Refresh(context.Background(), r.Session.clock.Elapsed()+time.Minute)
			if !tc.latched {
				require.NoError(t, err)
				require.False(t, r.Session.Latched())
				require.Equal(t, 4, p.sent)
				return
			}
			require.Error(t, err)
			if !r.Session.Latched() || p.sent != 1 {
				t.Fatal("MUTATION: unknown-lease-read-latch", err, r.Session.Latched(), p.sent)
			}
			require.Equal(t, tc.rule, r.Session.uncertainties[0].Rule)
			// The latch is terminal: a later refresh submits nothing.
			require.Error(t, r.Leases.Refresh(context.Background(), r.Session.clock.Elapsed()+time.Minute))
			require.Equal(t, 1, p.sent)
		})
	}
}

// Characterization: a lease read is refused locally, as a definite expiry and
// without a latch, when less than its send minimum remains.
func TestReferenceLeaseSendMinimumCharacterization(t *testing.T) {
	for _, tc := range []struct {
		op      operator.Operation
		target  string
		args    string
		minimum time.Duration
	}{
		{operator.HostLeaseRead, "w1", `{"reads":[{"target":"w1"}]}`, leaseReadMinimum},
		{operator.HostLeaseReadBatch, "hosts", `{"reads":[{"target":"relay"},{"target":"w1"},{"target":"w2"},{"target":"shared"}]}`, leaseReadBatchMinimum},
	} {
		for _, short := range []bool{true, false} {
			t.Run(string(tc.op)+map[bool]string{true: "/short", false: "/enough"}[short], func(t *testing.T) {
				s, c := fixtureSession(t)
				s.capabilities[tc.op] = true
				p := &observationPeer{respond: func(q operator.Request) operator.Reply { return reply(q, operator.Succeeded) }}
				remaining := tc.minimum + 50*time.Millisecond
				if short {
					remaining = tc.minimum - 10*time.Millisecond
				}
				_, err := s.Exchange(context.Background(), p, tc.op, tc.target, "clock", json.RawMessage(tc.args), nil, c.Elapsed()+remaining, func(operator.Reply) operator.Certainty { return operator.Succeeded })
				if short {
					if err == nil || err.Error() != "clock-lease-expired" || p.sent != 0 || s.Latched() {
						t.Fatal("MUTATION: lease-send-minimum", err, p.sent, s.Latched())
					}
					return
				}
				require.NoError(t, err)
				require.Equal(t, 1, p.sent)
			})
		}
	}
}
