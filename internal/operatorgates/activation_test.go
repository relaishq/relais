package operatorgates

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"testing/synctest"
	"time"

	"github.com/relaishq/relais/internal/operatorcaller"
	"github.com/relaishq/relais/internal/operatorgates/bindingtest"
	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/relaishq/relais/internal/operatorgates/observe"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/operator"
	"github.com/stretchr/testify/require"
)

func bindFixture(t *testing.T, r *ProcessRuntime, p *observationPeer, run string) (ResourceConfig, bindingtest.Fixture) {
	t.Helper()
	if r.Session.clock.Wall().Year() < 2020 {
		c := r.Session.clock.(*FakeClock)
		c.Now = time.Unix(1793930000, 0)
		r.Session.Budget.armed = c.Wall().UnixMilli()
		r.Session.Budget.wallEnd = r.Session.Budget.armed + SessionMaximum.Milliseconds()
	}
	f, err := bindingtest.New(run, r.Session.clock.Wall())
	require.NoError(t, err)
	r.Session.scenario, err = operator.BuildScenarioIdentity(r.Session.scenario, f.Selection.SHA256)
	require.NoError(t, err)
	r.Session.digest = r.Session.scenario.Digest
	r.Session.clockArtifacts = f.Artifacts
	c := ResourceConfig{Parameters: r.Config.Parameters, Pins: r.Pins, Bounds: f.Selection.Receipt.Bounds, ClockPolicy: f.Policy, ClockLeapMode: "unsmeared", Endpoints: operatorcaller.Endpoints{RunID: run, FrontendControlBoundNS: f.Selection.Receipt.Bounds.FrontendControlBoundNS, FrontendWorkerBoundNS: f.Selection.Receipt.Bounds.FrontendWorkerBoundNS, DriftPPM: 100}}
	original := p.respond
	p.respond = func(q operator.Request) operator.Reply {
		if q.Action == "clock" {
			if q.Operation == operator.HostLeaseRead {
				r.Session.clock.(*FakeClock).Advance(20 * time.Millisecond)
			}
			return resourceTestReply(operator.HostOutput{Stdout: f.Wire(string(q.Script), r.Session.clock.Wall())})
		}
		return original(q)
	}
	r.Session.capabilities[operator.HostLeaseRead] = true
	return c, f
}
func TestAtomicBindingBeforeMedia(t *testing.T) {
	for _, mode := range []string{"valid", "missing", "mismatch", "digest", "later"} {
		t.Run(mode, func(t *testing.T) {
			r, p, _ := runtimeFixture(t)
			c, _ := bindFixture(t, r, p, "bound-run")
			if mode == "missing" {
				r.Session.scenario.ClockReceiptSHA256 = ""
			}
			if mode == "mismatch" {
				c.Endpoints.FrontendWorkerBoundNS++
			}
			if mode == "digest" {
				r.Session.scenario.Digest = "bad"
			}
			if mode == "later" {
				r.Session.clock.(*FakeClock).Advance(time.Hour)
			}
			err := r.Session.BindClock(context.Background(), p, c, r.Session.clock.Elapsed()+time.Minute)
			if (err == nil) != (mode == "valid" || mode == "later") {
				t.Fatal("MUTATION: binding-mandatory MUTATION: mismatch-accepted", mode, err)
			}
			if mode == "mismatch" && err.Error() != "gate-caller-bounds-differ" {
				t.Fatal("MUTATION: mismatch-accepted", err)
			}
			if mode == "valid" {
				g, err := r.Session.BoundLeaseGuard(p, func(_ context.Context, d time.Duration) error { r.Session.clock.(*FakeClock).Advance(d); return nil })
				require.NoError(t, err)
				r.Leases = g
				require.NoError(t, r.VerifyClocks(context.Background(), time.Minute))
				e := r.Session.coverageSnapshot(c.Bounds)
				require.Equal(t, r.Session.scenario.ClockReceiptSHA256, e.ClockReceiptSHA256)
				require.NoError(t, e.ValidateBinding(e.ClockReceiptSHA256))
			}
		})
	}
}
func TestActivatedBracketCertainty(t *testing.T) {
	for _, mode := range []string{"narrow", "wide", "malformed", "before", "zero", "range", "outside-start", "outside-end", "reversed", "missing"} {
		t.Run(mode, func(t *testing.T) {
			r, p, _ := runtimeFixture(t)
			clock := &leaseTestClock{tick: time.Second}
			r.Session.clock, r.Session.Budget.clock = clock, clock
			g, _ := leaseGuardFixture(t, clock)
			base := p.respond
			p.respond = func(q operator.Request) operator.Reply {
				if q.Action != "worker-down" {
					return base(q)
				}
				before, after := "1793930000000000000", "1793930000000584000"
				if mode == "narrow" {
					after = "1793930000000000100"
				}
				if mode == "before" {
					before = "bad"
				}
				if mode == "zero" {
					before = "0000000000000000000"
				}
				if mode == "range" {
					after = "9999999999999999999"
				}
				if mode == "malformed" {
					after = "bad"
				}
				if mode == "reversed" {
					after = "1793929999999999999"
				}
				data := map[string]any{"completed": true, "signalBeforeUnixNs": before, "signalAfterUnixNs": after}
				if mode == "missing" {
					delete(data, "signalAfterUnixNs")
				}
				start := time.Unix(0, 1793930000000000000)
				end := start.Add(time.Millisecond)
				if mode == "outside-start" {
					start = start.Add(10 * time.Microsecond)
				}
				if mode == "outside-end" {
					end = start.Add(10 * time.Microsecond)
				}
				return hostReply(t, oracleJSON(map[string]any{"kind": "ok", "started": start, "ended": end, "data": data}))
			}
			got, err := r.StepWithLeases(context.Background(), "worker-down", "w1", time.Minute, ProcessExtra{Worker: "w1"}, LeaseStep{Guard: g})
			wide := mode == "wide" || mode == "narrow"
			if (err == nil) != wide || r.Session.Latched() == wide || wide && got.Certainty != operator.Succeeded {
				t.Fatal("MUTATION: bracket-certainty", mode, err, r.Session.Latched())
			}
			// Only a well-formed wide bracket excludes its kill and owes a replacement.
			marked := len(r.Session.clockBrackets) > 0 && r.Session.clockBrackets[len(r.Session.clockBrackets)-1].LeaseExcluded
			if got.KillExcluded != (mode == "wide") || marked != (mode == "wide") {
				t.Fatal("MUTATION: malformed-bracket-excluded", mode, got.KillExcluded, marked)
			}
		})
	}
}
func TestUnboundProductionRefuses(t *testing.T) {
	r, p, _ := runtimeFixture(t)
	got := (ResourceRunner{Config: ResourceConfig{}}).Run(context.Background(), r.Session, p)
	if len(got) == 0 || got[0].Status == operator.Passed || p.sent != 0 {
		t.Fatal("MUTATION: unbound-pass", got, p.sent)
	}
}
func TestLeaseAttemptBudget(t *testing.T) {
	c := &leaseTestClock{tick: time.Second}
	g, _ := leaseGuardFixture(t, c)
	calls := 0
	g.read = func(context.Context, time.Duration) (LeaseRead, error) { calls++; return LeaseRead{}, nil }
	_, err := g.readAttempt(context.Background(), c.Elapsed()+leaseReadAttempt-time.Nanosecond)
	if err == nil || err.Error() != "clock-lease-expired" || calls != 0 {
		t.Fatal("MUTATION: lease-attempt-budget", err, calls)
	}
}

func TestActivatedFaultGuard(t *testing.T) {
	r, _, order := runtimeFixture(t)
	c := &leaseTestClock{tick: time.Second}
	r.Session.clock, r.Session.Budget.clock = c, c
	g, _ := leaseGuardFixture(t, c)
	r.Session.clockBinding = &observe.ClockBinding{}
	r.Leases = g
	r.BeforeMutation = func(context.Context) error { c.advance(5 * time.Second); return nil }
	_, err := r.Step(context.Background(), "freeze", "shared", time.Minute, ProcessExtra{})
	if err == nil || r.Session.Latched() {
		t.Fatal("MUTATION: fault-bypass", err)
	}
	for _, op := range *order {
		if op == "freeze:shared" {
			t.Fatal("MUTATION: fault-bypass", *order)
		}
	}
}
func TestRollbackUnleasedCounted(t *testing.T) {
	r, p, _ := runtimeFixture(t)
	c := &FakeClock{Now: time.Unix(1793930000, 0), Tick: time.Second}
	r.Session.clock, r.Session.Budget.clock = c, c
	g, _ := leaseGuardFixture(t, c)
	g.invalid = true
	f, err := bindingtest.New("rollback", c.Wall())
	require.NoError(t, err)
	e, err := (clocks.CoverageEvidence{FrontendSlot: "shared", ControlSlot: "shared"}).WithBinding(clocks.ReceiptBinding{ReceiptSHA256: f.Selection.SHA256, EvidenceSHA256: f.Selection.Receipt.EvidenceSHA256, Bounds: f.Selection.Receipt.Bounds}, f.Selection.Receipt)
	require.NoError(t, err)
	r.Session.clockBinding = e.Binding
	r.Config.Operation, r.Config.Target = "rollback", "from"
	before := p.respond
	p.respond = func(q operator.Request) operator.Reply {
		if q.Action != "drain" {
			return before(q)
		}
		move := controlplane.MoveResult{ID: "call", From: "w1", To: "w2", Kind: "move", Start: c.Wall(), End: c.Wall()}
		return hostReply(t, oracleJSON(map[string]any{"kind": "ok", "started": c.Wall(), "ended": c.Wall(), "data": controlplane.DrainResult{Moves: []controlplane.MoveResult{move}}}))
	}
	_, err = r.StepWithLeases(context.Background(), "drain", "shared", time.Minute, ProcessExtra{Worker: "w1"}, LeaseStep{Guard: g})
	require.NoError(t, err)
	rows := []clocks.TrackingRow{}
	for _, delta := range []int64{-2, 2} {
		rows = append(rows, clocks.TrackingRow{UnixSeconds: c.Wall().Unix() + delta, LeapStatus: "N", MaxErrorSeconds: 1e-9, MaxErrorDecimal: "1.000e-09"})
	}
	r.Session.retainClockAttempt(clocks.Evidence{Version: 2, Clocks: map[string]clocks.Sample{"shared": {TrackingLog: &clocks.TrackingLog{ReadComplete: true, Rows: rows}}}})
	evidence := r.Session.coverageSnapshot(f.Selection.Receipt.Bounds)
	stamp := strconv.FormatInt(c.Wall().UnixNano(), 10)
	evidence.Windows = []observe.CoverageWindow{{Kind: "move", StartUnixNS: stamp, EndUnixNS: stamp}}
	without := *evidence
	without.MissingIntervals = nil
	without.Evaluate()
	require.Empty(t, without.Results[0].Causes)
	evidence.Evaluate()
	if len(evidence.Results) != 1 || len(evidence.Results[0].Causes) == 0 {
		t.Fatal("MUTATION: rollback-unleased-counted", evidence.Results)
	}
}
func TestLeaseQueueAttemptBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, c := fixtureSession(t)
		s.capabilities[operator.HostLeaseRead] = true
		p := &observationPeer{respond: func(q operator.Request) operator.Reply { return reply(q, operator.Succeeded) }}
		s.exchange <- struct{}{}
		done := make(chan error, 1)
		go func() {
			_, err := s.Exchange(context.Background(), p, operator.HostLeaseRead, "w1", "clock", json.RawMessage(`{"reads":[{"target":"w1"}]}`), nil, 2*time.Second, func(operator.Reply) operator.Certainty { return operator.Succeeded })
			done <- err
		}()
		synctest.Wait()
		c.Advance(2*time.Second - leaseReadMinimum + time.Nanosecond)
		<-s.exchange
		err := <-done
		if err == nil || err.Error() != "clock-lease-expired" || p.sent != 0 || s.Latched() {
			t.Fatal("MUTATION: lease-queue-budget", err, p.sent, s.Latched())
		}

	})
}

func TestLeasePrepareRefusalDoesNotLatch(t *testing.T) {
	s, _ := fixtureSession(t)
	out := s.RunSequence(context.Background(), SequencePorts{Prepare: func(context.Context, operator.Gate, time.Duration, time.Duration) error {
		return fault("clock-lease-expired")
	}})
	if s.Latched() || len(out.Gates) != 1 || out.Gates[0].Reason != "clock-lease-expired" {
		t.Fatal("MUTATION: lease-refusal-latched", out, s.Latched())
	}
}

func TestLeaseQueuedTimeoutIsDefinite(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, _ := fixtureSession(t)
		s.capabilities[operator.HostLeaseRead] = true
		p := &observationPeer{}
		s.exchange <- struct{}{}
		ctx, cancel := context.WithTimeout(context.Background(), leaseReadAttempt)
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, err := s.Exchange(ctx, p, operator.HostLeaseRead, "w1", "clock", json.RawMessage(`{"reads":[{"target":"w1"}]}`), nil, 4*time.Second, func(operator.Reply) operator.Certainty { return operator.Succeeded })
			done <- err
		}()
		synctest.Wait()
		time.Sleep(leaseReadAttempt)
		err := <-done
		<-s.exchange
		if err == nil || err.Error() != "clock-lease-expired" || s.Latched() || p.sent != 0 {
			t.Fatal("MUTATION: lease-queue-timeout", err, s.Latched(), p.sent)
		}
	})
}
