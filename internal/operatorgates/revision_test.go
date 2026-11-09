package operatorgates

import (
	"context"
	"encoding/json"
	"testing"
	"testing/synctest"
	"time"

	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/relaishq/relais/internal/operatorgates/observe"
	"github.com/relaishq/relais/pkg/operator"
	"github.com/stretchr/testify/require"
)

type delayedLeasePeer struct {
	*observationPeer
	delay time.Duration
}

func (p delayedLeasePeer) Receive(ctx context.Context) (operator.Frame, error) {
	select {
	case <-ctx.Done():
		return operator.Frame{}, ctx.Err()
	case <-time.After(p.delay):
		return p.observationPeer.Receive(ctx)
	}
}
func TestLeaseAttemptContext(t *testing.T) {
	for _, queued := range []bool{false, true} {
		t.Run(map[bool]string{false: "deadline", true: "queued"}[queued], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s, _ := fixtureSession(t)
				s.clock = NewRealClock()
				var err error
				now := s.clock.Wall()
				s.Budget = Budget{clock: s.clock, armed: now.UnixMilli(), wallEnd: now.Add(time.Hour).UnixMilli(), end: time.Hour}
				s.capabilities[operator.HostLeaseRead] = true
				p := &observationPeer{respond: func(q operator.Request) operator.Reply { return reply(q, operator.Succeeded) }}
				ctx, cancel := context.WithTimeout(context.Background(), leaseReadAttempt)
				defer cancel()
				if queued {
					s.exchange <- struct{}{}
				}
				done := make(chan error, 1)
				go func() {
					_, err := s.Exchange(ctx, delayedLeasePeer{observationPeer: p, delay: 100 * time.Millisecond}, operator.HostLeaseRead, "w1", "clock", json.RawMessage(`{"reads":[{"target":"w1"}]}`), nil, 4*time.Second, func(operator.Reply) operator.Certainty { return operator.Succeeded })
					done <- err
				}()
				synctest.Wait()
				if queued {
					time.Sleep(leaseReadAttempt - 50*time.Millisecond)
					<-s.exchange
				}
				err = <-done
				if queued {
					if err == nil || err.Error() != "clock-lease-expired" || s.Latched() || p.sent != 0 {
						t.Fatal("MUTATION: lease-attempt-context", err, s.Latched(), p.sent)
					}
				} else if err != nil || p.request.RemainingMS > leaseReadAttempt.Milliseconds() {
					t.Fatal("MUTATION: lease-attempt-deadline", err, p.request)
				}
			})
		})
	}
}
func TestBoundStoreRequiresGuard(t *testing.T) {
	r, p, _ := runtimeFixture(t)
	r.Session.capabilities[operator.StoreFailover] = true
	r.Session.clockBinding = &observe.ClockBinding{}
	original := p.respond
	p.respond = func(q operator.Request) operator.Reply {
		if q.Operation == operator.StoreFailover {
			return resourceTestReply(map[string]bool{"completed": true})
		}
		return original(q)
	}
	err := (SessionStoreCapability{Session: r.Session, Peer: p}).Failover(context.Background(), time.Minute)
	if err == nil || err.Error() != "clock-lease-missing" || p.sent != 0 || r.Session.Latched() {
		t.Fatal("MUTATION: store-unleased", err, p.sent)
	}
}
func TestVerifyBoundSnapshotError(t *testing.T) {
	r, _, _ := runtimeFixture(t)
	c := &leaseTestClock{tick: time.Second}
	r.Session.clock, r.Session.Budget.clock = c, c
	g, _ := leaseGuardFixture(t, c)
	r.Leases = g
	r.Session.clockBinding = &observe.ClockBinding{}
	sample := g.samples["shared"]
	sample.RateErrorPPM = 101
	g.samples["shared"] = sample
	require.NoError(t, g.Check())
	observed := false
	r.ClockObserved = func(_ clocks.Evidence) { observed = true }
	if err := r.VerifyClocks(context.Background(), time.Minute); err == nil || len(r.ClockSamples) != 0 || observed {
		t.Fatal("MUTATION: verify-bound-ignore", err, observed)
	}
}

func TestLeaseGuardReturnsKnownExpiry(t *testing.T) {
	c := &leaseTestClock{tick: time.Second}
	g, _ := leaseGuardFixture(t, c)
	c.advance(5 * time.Second)
	start := c.Elapsed()
	calls := 0
	g.pause = func(_ context.Context, d time.Duration) error { c.advance(d); return nil }
	g.read = func(context.Context, time.Duration) (LeaseRead, error) { calls++; return LeaseRead{}, nil }
	err := g.Refresh(context.Background(), start+500*time.Millisecond)
	if err == nil || err.Error() != "clock-lease-expired" || c.Elapsed()-start > 100*time.Millisecond || calls != 0 {
		t.Fatal("MUTATION: lease-short-budget-waits", err, c.Elapsed()-start, calls)
	}
}

func TestSerialLeaseServiceFloor(t *testing.T) {
	for _, tc := range []struct {
		name      string
		remaining time.Duration
		sent      bool
	}{
		{"insufficient", 350 * time.Millisecond, false},
		{"below", 549 * time.Millisecond, false},
		{"exact", 550 * time.Millisecond, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s, _ := fixtureSession(t)
				s.clock = NewRealClock()
				now := s.clock.Wall()
				s.Budget = Budget{clock: s.clock, armed: now.UnixMilli(), wallEnd: now.Add(time.Hour).UnixMilli(), end: time.Hour}
				s.capabilities[operator.HostLeaseRead] = true
				p := &observationPeer{respond: func(q operator.Request) operator.Reply { return reply(q, operator.Succeeded) }}
				ctx, cancel := context.WithTimeout(context.Background(), leaseReadAttempt)
				defer cancel()
				s.exchange <- struct{}{}
				done := make(chan error, 1)
				go func() {
					_, err := s.Exchange(ctx, delayedLeasePeer{observationPeer: p, delay: 390 * time.Millisecond}, operator.HostLeaseRead, "w1", "clock", json.RawMessage(`{"reads":[{"target":"w1"}]}`), nil, 4*time.Second, func(operator.Reply) operator.Certainty { return operator.Succeeded })
					done <- err
				}()
				synctest.Wait()
				time.Sleep(leaseReadAttempt - tc.remaining)
				<-s.exchange
				err := <-done
				if tc.sent {
					if err != nil || p.sent != 1 || s.Latched() {
						t.Fatal("MUTATION: lease-serial-service-floor", err, p.sent, s.Latched())
					}
				} else if err == nil || err.Error() != "clock-lease-expired" || p.sent != 0 || s.Latched() {
					t.Fatal("MUTATION: lease-serial-service-floor", err, p.sent, s.Latched())
				}
			})
		})
	}
}

func TestProductionWorkerStopRequiresLease(t *testing.T) {
	for _, mode := range []string{"missing", "expired-after-arm"} {
		t.Run(mode, func(t *testing.T) {
			r, _, order := runtimeFixture(t)
			r.Config.Operation, r.Config.Target = "upgrade", "to"
			c := &leaseTestClock{tick: time.Second}
			r.Session.clock, r.Session.Budget.clock = c, c
			g, _ := leaseGuardFixture(t, c)
			r.Leases = g
			want := "clock-lease-expired"
			if mode == "missing" {
				r.Leases = nil
				want = "clock-lease-missing"
			} else {
				r.BeforeMutation = func(context.Context) error { c.advance(5 * time.Second); return nil }
			}
			_, err := r.Step(context.Background(), "worker-stop", "w1", time.Minute, ProcessExtra{Worker: "w1"})
			if err == nil || err.Error() != want || r.Session.Latched() {
				t.Fatal("MUTATION: worker-stop-lease", err, *order)
			}
			for _, op := range *order {
				if op == "worker-stop:w1" {
					t.Fatal("MUTATION: worker-stop-lease", *order)
				}
			}
		})
	}
}

func TestBoundClockRefusalDiagnostic(t *testing.T) {
	for _, cause := range []string{"clock-lease-missing", "clock-lease-expired", "clock-lease-bound", "clock-config-differ"} {
		t.Run(cause, func(t *testing.T) {
			r, _, _ := runtimeFixture(t)
			c := &leaseTestClock{tick: time.Second}
			r.Session.clock, r.Session.Budget.clock = c, c
			g, read := leaseGuardFixture(t, c)
			r.Leases = g
			switch cause {
			case "clock-lease-missing":
				r.Leases = nil
			case "clock-lease-expired":
				c.advance(5 * time.Second)
				g.pause = func(_ context.Context, d time.Duration) error { c.advance(d); return nil }
			case "clock-lease-bound":
				read.Evidence.Clocks["shared"].Lease.ErrorBoundNS = 200000
				require.NoError(t, g.accept(read))
			case "clock-config-differ":
				g.invalid = true
			}
			calls := 0
			r.Session.SetObservationFailureSink(func(slot, action string, err error) {
				calls++
				if slot != "hosts" || action != "clock" || err.Error() != cause {
					t.Fatal("MUTATION: lease-guard-diagnostic", slot, action, err)
				}
			})
			err := r.VerifyClocks(context.Background(), c.Elapsed()+time.Second)
			if err == nil || code(err) != cause || calls != 1 || r.Session.Latched() {
				t.Fatal("MUTATION: lease-guard-diagnostic", err, calls)
			}
		})
	}
}
