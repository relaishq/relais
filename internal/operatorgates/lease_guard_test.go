package operatorgates

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/relaishq/relais/internal/operatorgates/inventory"
	"github.com/relaishq/relais/internal/operatorgates/observe"
	"github.com/relaishq/relais/pkg/operator"
	"github.com/stretchr/testify/require"
)

type leaseTestClock struct {
	mu   sync.Mutex
	tick time.Duration
}

func (c *leaseTestClock) Elapsed() time.Duration  { c.mu.Lock(); defer c.mu.Unlock(); return c.tick }
func (c *leaseTestClock) Wall() time.Time         { return time.UnixMilli(1000000).Add(c.Elapsed()) }
func (c *leaseTestClock) advance(d time.Duration) { c.mu.Lock(); defer c.mu.Unlock(); c.tick += d }

func leaseGuardFixture(t *testing.T, c Clock, sessions ...*Session) (*LeaseGuard, LeaseRead) {
	t.Helper()
	raw, err := os.ReadFile("clocks/testdata/bound-receipt.json")
	require.NoError(t, err)
	var s clocks.ClockSelection
	require.NoError(t, json.Unmarshal(raw, &s))
	e, err := (clocks.CoverageEvidence{FrontendSlot: s.Receipt.FrontendSlot, ControlSlot: s.Receipt.ControlSlot}).WithBinding(clocks.ReceiptBinding{ReceiptSHA256: s.SHA256, EvidenceSHA256: s.Receipt.EvidenceSHA256, Bounds: s.Receipt.Bounds}, s.Receipt)
	require.NoError(t, err)
	r := LeaseRead{Evidence: clocks.Evidence{Clocks: map[string]clocks.Sample{}}, Sent: map[string]time.Duration{}}
	for _, slot := range inventory.Slots {
		o := &clocks.LeaseObservation{Available: true, ErrorBoundNS: 1000, MeasuredUnixNS: "1793930000100000000", ReferenceUnixNS: "1793930000000000000", ReferenceAgeNS: 100000000, PollSeconds: "1", ResidualPPM: "0", SkewPPM: "0", DispersionStartSeconds: "0.000001000", DispersionEndSeconds: "0.000001001", IntervalNS: 20000000, HostElapsedNS: 50000000}
		r.Evidence.Clocks[slot] = clocks.Sample{Lease: o, TrackingLog: &clocks.TrackingLog{ReadComplete: true}}
		r.Sent[slot] = c.Elapsed() - 50*time.Millisecond
	}
	session, _ := fixtureSession(t)
	if len(sessions) > 0 {
		session = sessions[0]
	}
	session.clock, session.Budget.clock = c, c
	g, err := NewLeaseGuard(e, session, func(context.Context, time.Duration) (LeaseRead, error) { return r, nil }, func(_ context.Context, d time.Duration) error {
		switch clock := c.(type) {
		case *leaseTestClock:
			clock.advance(d)
		case *FakeClock:
			clock.Advance(d)
		}
		return nil
	})
	require.NoError(t, err)
	read, err := g.readAttempt(context.Background(), c.Elapsed()+time.Minute)
	require.NoError(t, err)
	require.NoError(t, g.accept(read))
	return g, r
}

func TestLeaseGuardChecks(t *testing.T) {
	for _, mode := range []string{"ok", "missing", "expired", "control", "worker", "growth", "break", "falsified", "partial-break"} {
		t.Run(mode, func(t *testing.T) {
			c := &leaseTestClock{tick: time.Second}
			g, r := leaseGuardFixture(t, c)
			want := ""
			switch mode {
			case "missing":
				delete(g.leases, "w2")
				want = "clock-lease-missing"
			case "expired":
				c.advance(5 * time.Second)
				want = "clock-lease-expired"
			case "control":
				r.Evidence.Clocks["shared"].Lease.ErrorBoundNS = 120000
				want = "clock-lease-bound"
			case "worker":
				r.Evidence.Clocks["w2"].Lease.ErrorBoundNS = 130000
				want = "clock-lease-bound"
			case "growth":
				r.Evidence.Clocks["w1"].Lease.DispersionEndSeconds = "0.000002000"
				want = "clock-config-differ"
			case "break":
				r.Evidence.Clocks["relay"].TrackingLog.Breaks = []clocks.TrackingBreak{{UnixSeconds: g.frozenEnd}}
				want = "clock-config-differ"
			case "partial-break":
				delete(r.Sent, "relay")
				r.Evidence.Clocks["w2"].TrackingLog.Breaks = []clocks.TrackingBreak{{UnixSeconds: g.frozenEnd}}
				want = "clock-config-differ"
			case "falsified":
				r.Evidence.Falsification = &clocks.Falsification{CountsByStatus: map[string]int{"falsified": 1}}
				want = "clock-config-differ"
			}
			if mode != "missing" {
				err := g.accept(r)
				if mode == "partial-break" {
					require.Error(t, err)
				} else {
					require.NoError(t, err)
				}
			}
			err := g.Check()
			if want == "" {
				require.NoError(t, err)
			} else {
				require.EqualError(t, err, want)
			}
		})
	}
}

func TestLeaseGuardQueuedExpiry(t *testing.T) {
	c := &leaseTestClock{tick: time.Second}
	g, _ := leaseGuardFixture(t, c)
	s, _ := fixtureSession(t)
	s.clock, s.Budget.clock = c, c
	p := &observationPeer{respond: func(req operator.Request) operator.Reply { return reply(req, operator.Succeeded) }}
	s.exchange <- struct{}{}
	ready, done := make(chan struct{}), make(chan error, 1)
	go func() {
		close(ready)
		_, err := s.Exchange(context.Background(), p, operator.HostExecute, "shared", "freeze", nil, nil, time.Minute, func(operator.Reply) operator.Certainty { return operator.Succeeded }, ExchangeHooks{BeforeSend: g.Check})
		done <- err
	}()
	<-ready
	c.advance(5 * time.Second)
	<-s.exchange
	err := <-done
	if err == nil || p.sent != 0 || len(s.requests) != 0 {
		t.Fatal("MUTATION: lease-queued-expiry", err, p.sent)
	}
	if s.Latched() {
		t.Fatal("local lease refusal became uncertainty")
	}
}

func TestLeaseGuardArmAndRestore(t *testing.T) {
	for _, mode := range []string{"arm", "rollback", "recovery", "unknown", "inventory"} {
		t.Run(mode, func(t *testing.T) {
			r, _, order := runtimeFixture(t)
			c := &leaseTestClock{tick: time.Second}
			r.Session.clock, r.Session.Budget.clock = c, c
			g, _ := leaseGuardFixture(t, c)
			if mode == "arm" {
				r.BeforeMutation = func(context.Context) error { c.advance(5 * time.Second); return nil }
			} else {
				g.invalid = true
				r.BeforeMutation = func(context.Context) error { return fmt.Errorf("arm must be bypassed") }
			}
			if mode == "rollback" {
				r.Config.Operation, r.Config.Target = "rollback", "from"
			}
			if mode == "unknown" {
				r.Session.TransportLost()
			}
			if mode == "inventory" {
				r.Pins = nil
			}
			op := "freeze"
			if mode != "arm" {
				op = "replace"
			}
			_, err := r.StepWithLeases(context.Background(), op, "shared", time.Minute, ProcessExtra{Worker: "w1"}, LeaseStep{Guard: g, Recovery: mode != "arm" && mode != "rollback"})
			if mode == "rollback" || mode == "recovery" {
				if err != nil {
					t.Fatal("MUTATION: lease-rollback-bypass", mode, err)
				}
			} else {
				require.Error(t, err)
			}
			if mode == "arm" {
				require.EqualError(t, err, "clock-lease-expired")
			}
			if mode == "rollback" || mode == "recovery" {
				require.GreaterOrEqual(t, len(*order), 5)
				require.Equal(t, "inventory:relay", (*order)[0])
			}
		})
	}
	for _, op := range []string{"worker-start", "control-start", "signal-start", "unfreeze", "relay-replenish"} {
		require.True(t, leaseRestoration(op, false, false))
	}
	require.False(t, leaseRestoration("worker-down", true, true))
}

func TestLeaseGuardBoundedRefresh(t *testing.T) {
	for _, fresh := range []bool{true, false} {
		c := &leaseTestClock{tick: time.Second}
		g, r := leaseGuardFixture(t, c)
		c.advance(5 * time.Second)
		start := c.Elapsed()
		calls := 0
		g.pause = func(_ context.Context, d time.Duration) error { c.advance(d); return nil }
		g.read = func(context.Context, time.Duration) (LeaseRead, error) {
			calls++
			if fresh && calls == 1 {
				return LeaseRead{}, nil
			}
			if fresh {
				for slot, s := range r.Evidence.Clocks {
					o := *s.Lease
					o.ReferenceUnixNS, o.MeasuredUnixNS = "1793930001000000000", "1793930001100000000"
					s.Lease = &o
					r.Evidence.Clocks[slot] = s
					r.Sent[slot] = c.Elapsed() - 50*time.Millisecond
				}
			}
			return r, nil
		}
		err := g.Refresh(context.Background(), time.Minute)
		if fresh {
			require.NoError(t, err)
		} else {
			require.EqualError(t, err, "clock-lease-expired")
			require.GreaterOrEqual(t, c.Elapsed()-start, 10*time.Second-leaseReadAttempt)
			require.Less(t, c.Elapsed()-start, 10*time.Second)
		}
	}
}

func TestLeaseReadRequestSend(t *testing.T) {
	for _, batch := range []bool{false, true} {
		s, c := fixtureSession(t)
		s.capabilities[operator.HostLeaseRead], s.capabilities[operator.HostLeaseReadBatch] = true, batch
		p := &observationPeer{respond: func(req operator.Request) operator.Reply {
			c.Advance(100 * time.Millisecond)
			raw := []byte(`{"errorBoundNs":1,"rateErrorPpm":0,"measuredUnixNs":"1793930000000000000"}`)
			if !batch {
				return hostReply(t, raw)
			}
			out := operator.HostReadBatchOutput{}
			for _, slot := range inventory.Slots {
				r := hostReply(t, raw)
				r.ID = slot
				out.Replies = append(out.Replies, r)
			}
			return operator.Reply{Transport: operator.Succeeded, ExitCode: new(0), Payload: oracleJSON(out)}
		}}
		r, err := s.ReadLeases(context.Background(), p, time.Minute)
		require.NoError(t, err)
		for i, slot := range inventory.Slots {
			want := time.Duration(i) * 100 * time.Millisecond
			if batch {
				want = 0
			}
			require.Equal(t, want, r.Sent[slot], "MUTATION: lease-batch-send")
		}
	}
}

func TestLeaseGuardStore(t *testing.T) {
	c := &leaseTestClock{tick: time.Second}
	g, _ := leaseGuardFixture(t, c)
	g.leases, g.observations = map[string]clocks.Lease{}, map[string]clocks.LeaseObservation{}
	s, _ := fixtureSession(t)
	s.clock, s.Budget.clock, s.capabilities[operator.StoreFailover] = c, c, true
	p := &observationPeer{respond: func(req operator.Request) operator.Reply {
		r := reply(req, operator.Succeeded)
		r.Payload = json.RawMessage(`{"completed":true}`)
		return r
	}}
	store := SessionStoreCapability{Session: s, Peer: p}
	require.NoError(t, store.FailoverWithLeases(context.Background(), time.Minute, g))
	require.Equal(t, 1, p.sent)
	g.invalid = true
	require.EqualError(t, store.FailoverWithLeases(context.Background(), time.Minute, g), "clock-config-differ")
	require.Equal(t, 1, p.sent)
	require.False(t, s.Latched())
}

func TestLeaseReplyAging(t *testing.T) {
	for _, delay := range []time.Duration{0, 1500 * time.Millisecond} {
		r, p, _ := runtimeFixture(t)
		c := &leaseTestClock{tick: time.Second}
		r.Session.clock, r.Session.Budget.clock = c, c
		g, read := leaseGuardFixture(t, c)
		for _, s := range read.Evidence.Clocks {
			s.Lease.ErrorBoundNS = 58000
		}
		require.NoError(t, g.accept(read))
		respond := p.respond
		p.respond = func(req operator.Request) operator.Reply {
			if req.Action != "worker-down" {
				return respond(req)
			}
			c.advance(delay)
			// Leave room for the new pre-dispatch gap, while the additional
			// 1.5s reply delay still crosses the unchanged signal bound.
			return hostReply(t, []byte(`{"kind":"ok","started":"2026-11-06T01:53:20Z","ended":"2026-11-06T01:53:20.001Z","data":{"completed":true,"signalBeforeUnixNs":"1793930000000000000","signalAfterUnixNs":"1793930000000107000"}}`))
		}
		_, err := r.StepWithLeases(context.Background(), "worker-down", "w1", time.Minute, ProcessExtra{Worker: "w1"}, LeaseStep{Guard: g})
		if err != nil || r.Session.Latched() || len(r.Session.clockBrackets) != 1 || r.Session.clockBrackets[0].LeaseExcluded != (delay != 0) {
			t.Fatal("MUTATION: lease-reply-age", delay, err)
		}

		b := r.Session.clockBrackets[0]
		evidence := clocks.CoverageEvidence{Version: 2, FrontendSlot: "shared", ControlSlot: "shared", Bounds: clocks.FixedCoverageBounds(), KillBrackets: []observe.CoverageBracket{b}, Windows: []observe.CoverageWindow{{Kind: "kill", Worker: "w1", StartUnixNS: b.MidpointUnixNS, EndUnixNS: b.MidpointUnixNS}}}
		evidence.Evaluate()
		causes := evidence.Results[0].Causes
		require.Equal(t, delay != 0, slices.Contains(causes, "clock-coverage-kill-excluded"), "MUTATION: lease-reply-age")
		require.False(t, slices.Contains(causes, "clock-coverage-kill-bracket"), "MUTATION: lease-reply-age")
	}
}

func TestLeasePreflightErrors(t *testing.T) {
	for _, store := range []bool{false, true} {
		r, p, _ := runtimeFixture(t)
		c := &leaseTestClock{tick: time.Second}
		r.Session.clock, r.Session.Budget.clock = c, c
		g, _ := leaseGuardFixture(t, c)
		g.leases, g.observations = map[string]clocks.Lease{}, map[string]clocks.LeaseObservation{}
		g.read = func(context.Context, time.Duration) (LeaseRead, error) { return LeaseRead{}, context.Canceled }
		var err error
		if store {
			err = (SessionStoreCapability{Session: r.Session, Peer: p}).FailoverWithLeases(context.Background(), time.Minute, g)
		} else {
			_, err = r.StepWithLeases(context.Background(), "freeze", "shared", time.Minute, ProcessExtra{}, LeaseStep{Guard: g})
		}
		require.EqualError(t, err, "precondition-failed:clocks")
	}
}

// The first slow read reuses the expired reference. The second gets a new one.
// Both complete within the four seconds left in the parent deadline.
func TestLeaseAttemptRetryRoom(t *testing.T) {
	c := &leaseTestClock{tick: time.Second}
	g, r := leaseGuardFixture(t, c)
	c.advance(5 * time.Second)
	start := c.Elapsed()
	calls := 0
	g.pause = func(_ context.Context, d time.Duration) error { c.advance(d); return nil }
	g.read = func(context.Context, time.Duration) (LeaseRead, error) {
		calls++
		sent := c.Elapsed()
		c.advance(1700 * time.Millisecond)
		for slot, sample := range r.Evidence.Clocks {
			o := *sample.Lease
			if calls == 2 {
				o.ReferenceUnixNS = "1793930001000000000"
				o.MeasuredUnixNS = "1793930001100000000"
			}
			sample.Lease = &o
			r.Evidence.Clocks[slot] = sample
			r.Sent[slot] = sent
		}
		return r, nil
	}
	err := g.Refresh(context.Background(), start+4*time.Second)
	if err != nil || calls != 2 || c.Elapsed()-start > 4*time.Second || g.Check() != nil {
		t.Fatal("MUTATION: lease-attempt-retry-room", err, calls, c.Elapsed()-start)
	}
}

func TestLeaseGuardTenPollRefresh(t *testing.T) {
	for _, parentLimit := range []time.Duration{7 * time.Second, 2 * time.Second} {
		c := &leaseTestClock{tick: time.Second}
		g, r := leaseGuardFixture(t, c)
		c.advance(5 * time.Second)
		start := c.Elapsed()
		calls := 0
		g.pause = func(_ context.Context, d time.Duration) error { c.advance(d); return nil }
		g.read = func(context.Context, time.Duration) (LeaseRead, error) {
			calls++
			sent := c.Elapsed()
			c.advance(500 * time.Millisecond)
			for slot, s := range r.Evidence.Clocks {
				o := *s.Lease
				if c.Elapsed()-start >= 4500*time.Millisecond {
					o.ReferenceUnixNS = "1793930001000000000"
					o.MeasuredUnixNS = "1793930001100000000"
				}
				s.Lease = &o
				r.Evidence.Clocks[slot] = s
				r.Sent[slot] = sent
			}
			return r, nil
		}
		err := g.Refresh(context.Background(), start+parentLimit)
		if parentLimit > 4*time.Second {
			if err != nil || calls < 2 || c.Elapsed()-start > parentLimit {
				t.Fatal("MUTATION: lease-guard-refresh-window", err, calls, c.Elapsed()-start)
			}
		} else if err == nil || err.Error() != "clock-lease-expired" || c.Elapsed()-start > parentLimit {
			t.Fatal("MUTATION: lease-guard-refresh-window", err, c.Elapsed()-start)
		}
	}
}
