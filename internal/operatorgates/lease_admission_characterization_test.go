package operatorgates

import (
	"context"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/stretchr/testify/require"
)

// Characterize acquisition before adding fault scheduling: a valid cache needs
// no I/O, attempts retain their deadline, and unknown transport is never retried.
func TestLeaseAdmissionCharacterization(t *testing.T) {
	for _, mode := range []string{"cached", "short-budget", "unknown"} {
		t.Run(mode, func(t *testing.T) {
			c := &leaseTestClock{tick: time.Second}
			g, _ := leaseGuardFixture(t, c)
			calls := 0
			g.pause = func(_ context.Context, d time.Duration) error { c.advance(d); return nil }
			g.read = func(ctx context.Context, _ time.Duration) (LeaseRead, error) {
				calls++
				deadline, ok := ctx.Deadline()
				require.True(t, ok)
				require.LessOrEqual(t, time.Until(deadline), 1800*time.Millisecond)
				return LeaseRead{}, fault("command-result-unknown")
			}
			end := time.Minute
			if mode != "cached" {
				c.advance(5 * time.Second)
			}
			if mode == "short-budget" {
				end = c.Elapsed() + 1800*time.Millisecond
			}
			err := g.Refresh(context.Background(), end)
			switch mode {
			case "cached":
				require.NoError(t, err)
				require.Zero(t, calls)
			case "short-budget":
				require.EqualError(t, err, "clock-lease-expired")
				require.Zero(t, calls)
			case "unknown":
				require.EqualError(t, err, "command-result-unknown")
				require.Equal(t, 1, calls)
			}
		})
	}
}

func TestLeaseAdmissionRestorationCharacterization(t *testing.T) {
	r, _, _ := runtimeFixture(t)
	c := &leaseTestClock{tick: time.Second}
	r.Session.clock, r.Session.Budget.clock = c, c
	g, _ := leaseGuardFixture(t, c)
	g.leases = map[string]clocks.Lease{}
	g.pause = func(context.Context, time.Duration) error { t.Fatal("restoration waited for clocks"); return nil }
	g.read = func(context.Context, time.Duration) (LeaseRead, error) {
		t.Fatal("restoration read clocks")
		return LeaseRead{}, nil
	}
	r.BeforeMutation = func(context.Context) error { t.Fatal("restoration armed a fault"); return nil }
	_, err := r.StepWithLeases(context.Background(), "unfreeze", "shared", time.Minute, ProcessExtra{}, LeaseStep{Guard: g})
	require.NoError(t, err)
	require.Equal(t, time.Second, c.Elapsed())
	require.False(t, r.Session.Latched())
}
