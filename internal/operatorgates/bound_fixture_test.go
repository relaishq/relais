package operatorgates

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/operatorgates/bindingtest"
	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/relaishq/relais/internal/operatorgates/inventory"
	"github.com/relaishq/relais/pkg/operator"
	"github.com/stretchr/testify/require"
)

func testCoverageBinding(t *testing.T, s *Session) {
	t.Helper()
	f, err := bindingtest.New("run", time.Unix(1793930000, 0))
	require.NoError(t, err)
	f.Policy.AllowanceNS = 0
	f.Policy.FloorNS = 1000000
	f.Policy.SignalReserveNS = 1000000
	f.Policy.ControlCeilingNS = 2000000
	f.Policy.WorkerCeilingNS = 3000000
	front, e1 := inventory.ComponentSlot(s.inventoryComponents, "signal")
	control, e2 := inventory.ComponentSlot(s.inventoryComponents, "control")
	if e1 && e2 {
		f.Input.FrontendSlot, f.Input.ControlSlot = front, control
		f.Input.Falsification.FrontendSlot = front
	}
	selected, err := clocks.Select(f.Input, f.Policy)
	require.NoError(t, err)
	e, err := (clocks.CoverageEvidence{FrontendSlot: f.Input.FrontendSlot, ControlSlot: f.Input.ControlSlot}).WithBinding(clocks.ReceiptBinding{ReceiptSHA256: selected.SHA256, EvidenceSHA256: selected.Receipt.EvidenceSHA256, Bounds: selected.Receipt.Bounds}, selected.Receipt)
	require.NoError(t, err)
	s.clockBinding = e.Binding
}
func coverageFixtureSession(t *testing.T) (*Session, *FakeClock) {
	s, c := fixtureSession(t)
	testCoverageBinding(t, s)
	return s, c
}

// Old scalar-reader tables remain useful diagnostic inputs. This fixture gives
// them explicit bound leases; production has no scalar-clock authorization path.
func testRuntimeLeases(t *testing.T, r *ProcessRuntime) {
	t.Helper()
	s := r.Session
	if s.inventoryComponents == nil {
		s.inventoryComponents = map[string][]string{"shared": {"signal", "control"}, "w1": {"w1"}, "w2": {"w2"}}
	}
	testCoverageBinding(t, s)
	s.capabilities[operator.HostLeaseRead] = true
	proof := clocks.CoverageEvidence{FrontendSlot: s.clockBinding.Receipt.FrontendSlot, ControlSlot: s.clockBinding.Receipt.ControlSlot, Bounds: s.clockBinding.Binding.Bounds, ClockReceiptSHA256: s.clockBinding.Binding.ReceiptSHA256, Binding: s.clockBinding}
	g, err := NewLeaseGuard(proof, s, func(ctx context.Context, end time.Duration) (LeaseRead, error) {
		read, err := s.ReadLeases(ctx, r.Peer, end)
		s.retainClockAttempt(read.Evidence)
		if err != nil && r.ClockObserved != nil && !read.Evidence.At.IsZero() {
			r.ClockObserved(read.Evidence)
		}
		return read, err
	}, func(_ context.Context, d time.Duration) error {
		switch c := s.clock.(type) {
		case *FakeClock:
			c.Advance(d)
		case *leaseTestClock:
			c.advance(d)
		}
		return nil
	})
	require.NoError(t, err)
	r.Leases = g
}

func testStoreLease(t *testing.T, s *Session) {
	t.Helper()
	if c, ok := s.clock.(*FakeClock); ok && c.Elapsed() < time.Second {
		c.Advance(time.Second)
	}
	g, _ := leaseGuardFixture(t, s.clock)
	s.liveLeaseGuard = g
}

func testPrecision(rows []clocks.TrackingRow) {
	for i := range rows {
		if rows[i].MaxErrorDecimal == "" {
			rows[i].MaxErrorDecimal = fmt.Sprintf("%.3e", rows[i].MaxErrorSeconds)
		}
	}
}

func retainTestClockAttempt(s *Session, e clocks.Evidence) {
	for _, sample := range e.Clocks {
		if sample.TrackingLog != nil {
			testPrecision(sample.TrackingLog.Rows)
		}
	}
	s.retainClockAttempt(e)
}

func retainTestDrainAttempt(s *Session, e clocks.Evidence) {
	for _, sample := range e.Clocks {
		if sample.TrackingLog != nil {
			testPrecision(sample.TrackingLog.Rows)
		}
	}
	s.retainDrainClockAttempt(e)
}
