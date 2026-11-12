package operatorgates

import (
	"context"
	"encoding/json"
	"net/netip"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/operatorgates/observe"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/operator"
	"github.com/relaishq/relais/pkg/relay"
)

// floorStep refuses a host request locally once less than the host-submission
// floor remains, as Session.Exchange does, and records each status read time.
func floorStep(clock *FakeClock, polls *[]time.Duration, status func(string) controlplane.Status) StepFunc {
	return func(_ context.Context, op, _ string, end time.Duration, _ ProcessExtra) (StepResult, error) {
		if end-clock.Elapsed() < operator.HostSubmissionMinimum {
			return StepResult{Certainty: operator.Failed}, fault("precondition-failed:deadline-before-submission")
		}
		if op == "status" {
			*polls = append(*polls, clock.Elapsed())
			return StepResult{LossKnown: true, Status: status(op)}, nil
		}
		ready := int64(100)
		return StepResult{ReadyMS: &ready}, nil
	}
}

func liveStatus(g *procedure, call controlplane.CallStatus) controlplane.Status {
	return controlplane.Status{Ready: true, Pool: controlplane.PoolSample{Cap: 2}, Calls: []controlplane.CallStatus{call}, Workers: []controlplane.WorkerStatus{
		{Name: "w1", Address: g.c.Parameters.Workers["w1"].Media, Calls: 1},
		{Name: "w2", Address: g.c.Parameters.Workers["w2"].Media},
	}}
}

// A control restart whose control plane never reconstructs the call polls
// every 500 ms until the host-submission floor refuses the next read. The
// gate keeps that refusal as its category and does not latch the session.
func TestBudgetEndedWaitKeepsCategory(t *testing.T) {
	g, clock := testProcedure(t)
	g.end -= RestoreReserve
	end := g.end
	call := controlplane.CallStatus{ID: "original", Owner: "w1", Epoch: 1}
	var polls []time.Duration
	reads := 0
	s, _ := fixtureSession(t)
	g.p.Wait = s.recordWait
	g.p.Step = floorStep(clock, &polls, func(string) controlplane.Status {
		reads++
		s := liveStatus(g, call)
		if reads > 1 {
			s.Ready, s.Calls = false, nil
		}
		return s
	})
	r := runProcessGate(context.Background(), operator.ControlRestart, g)
	if r.Status != operator.Inconclusive || r.Reason != "precondition-failed" || r.Category != "deadline-before-submission" || r.Completed || g.unknown || r.Process.Restarts != 0 {
		t.Fatal("budget-ended wait changed its result", r)
	}
	if len(polls) < 3 {
		t.Fatal("wait did not poll", polls)
	}
	for i := 2; i < len(polls); i++ {
		if polls[i]-polls[i-1] != 500*time.Millisecond {
			t.Fatal("poll interval changed", polls[i-1], polls[i])
		}
	}
	if left := end - polls[len(polls)-1]; left < operator.HostSubmissionMinimum || left >= operator.HostSubmissionMinimum+500*time.Millisecond {
		t.Fatal("wait stopped before the submission floor", left)
	}
	want := WaitDiagnostic{Purpose: "control-reconstructed", WaitedMS: (polls[len(polls)-1] + 500*time.Millisecond - polls[0]).Milliseconds(), Polls: len(polls) - 1, Last: []string{"control-not-ready", "calls-none", "w1-live", "w2-live"}}
	if got := r.Process.ExpiredWait; got == nil || !reflect.DeepEqual(*got, want) {
		t.Fatalf("MUTATION: expired-wait-diagnostic got %+v want %+v", got, want)
	}
	waits := 0
	for _, e := range s.Final(nil).Trace {
		if e.Event == "wait" && e.WaitPurpose == "control-reconstructed" {
			waits++
		}
	}
	if waits != len(polls) {
		t.Fatal("control wait purpose missing from the trace", waits, len(polls))
	}
	s.CompleteGate(r.GateResult(operator.ControlRestart), s.clock.Elapsed()+time.Minute)
	if s.Latched() {
		t.Fatal("budget-ended wait latched the session")
	}
	s.CompleteGate(operator.GateResult{Gate: operator.ControlRestart, Status: operator.Inconclusive, Reason: "gate-deadline"}, s.clock.Elapsed()+time.Minute)
	if !s.Latched() {
		t.Fatal("gate-deadline no longer latches")
	}
}

// Each kind of gate wait names itself when the budget ends it, whether the
// gate end passed or the submission floor refused the next read. A wait that
// succeeds or ends on another error records nothing.
func TestGateWaitsNameExpiredWait(t *testing.T) {
	call := controlplane.CallStatus{ID: "original", Owner: "w1", Epoch: 1}
	inspected := func(instance string, epoch uint64) StepResult {
		status := relay.RelayStatus{Instance: instance, Public: netip.MustParseAddrPort("192.0.2.2:3478"), Private: netip.MustParseAddrPort("192.0.2.2:24702"), Continuity: &relay.ContinuityStatus{Epoch: epoch}}
		status.Stats.Workers, status.Stats.RoutesRestored, status.Stats.WorkerPackets = 2, 1, 10
		return StepResult{Inspection: observe.Inspection{Active: "relay-a", Status: status}}
	}
	for _, tc := range []struct {
		name string
		run  func(*procedure, *FakeClock) error
		code string
		want *WaitDiagnostic
	}{
		{"trial-ready-gate-end", func(g *procedure, clock *FakeClock) error {
			moved := false
			g.p.Step = func(_ context.Context, op, _ string, _ time.Duration, _ ProcessExtra) (StepResult, error) {
				if op == "move" {
					m := controlplane.MoveResult{ID: call.ID, From: "w1", To: "w2", Kind: "move", Start: clock.Wall(), End: clock.Wall()}
					moved = true
					return StepResult{Move: m, Raw: oracleJSON(m)}, nil
				}
				s := liveStatus(g, call)
				s.Workers[0].Recovering = moved
				if moved {
					s.Calls[0].Owner, s.Calls[0].Epoch = "w2", 2
				}
				return StepResult{LossKnown: true, Status: s}, nil
			}
			return g.trials(context.Background(), TrialTarget{Moves: 1}, "stable")
		}, "gate-deadline", &WaitDiagnostic{Purpose: "trial-ready", Last: []string{"control-ready", "calls-one", "call-owner-w2", "w1-recovering", "w2-live"}}},
		{"release-call-restored-floor", func(g *procedure, clock *FakeClock) error {
			var polls []time.Duration
			restarted := false
			step := floorStep(clock, &polls, func(string) controlplane.Status {
				s := liveStatus(g, call)
				if restarted {
					s.Calls[0].Epoch = 2
				}
				return s
			})
			g.p.Step = func(ctx context.Context, op, slot string, end time.Duration, extra ProcessExtra) (StepResult, error) {
				restarted = restarted || op == "control-start"
				return step(ctx, op, slot, end, extra)
			}
			return g.releaseControl(context.Background())
		}, "precondition-failed:deadline-before-submission", &WaitDiagnostic{Purpose: "release-call-restored", Last: []string{"control-ready", "calls-one", "call-owner-w1", "w1-live", "w2-live"}}},
		{"relay-continuity-floor", func(g *procedure, clock *FakeClock) error {
			var polls []time.Duration
			step := floorStep(clock, &polls, nil)
			g.p.Step = func(ctx context.Context, op, slot string, end time.Duration, extra ProcessExtra) (StepResult, error) {
				if op == "relay-inspect" {
					if _, err := step(ctx, op, slot, end, extra); err != nil {
						return StepResult{}, err
					}
					return inspected("relay-a", 1), nil
				}
				return step(ctx, op, slot, end, extra)
			}
			return g.relay(context.Background(), true, 1)
		}, "gate-deadline", &WaitDiagnostic{Purpose: "relay-continuity", Last: []string{"relay-same-instance", "relay-epoch-same", "relay-workers-ready"}}},
		{"worker-alive-satisfied", func(g *procedure, _ *FakeClock) error {
			g.p.Step = func(context.Context, string, string, time.Duration, ProcessExtra) (StepResult, error) {
				return StepResult{Status: liveStatus(g, call)}, nil
			}
			_, err := g.wait(context.Background(), "worker-alive", func(s controlplane.Status) bool { return alive(s, "w1", g.c.Parameters.Workers) })
			return err
		}, "", nil},
		{"restart-takeover-unknown", func(g *procedure, _ *FakeClock) error {
			g.p.Step = func(context.Context, string, string, time.Duration, ProcessExtra) (StepResult, error) {
				return StepResult{}, fault("command-result-unknown")
			}
			_, err := g.waitStep(context.Background(), "restart-takeover", func(StepResult) bool { return false })
			return err
		}, "command-result-unknown", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, clock := testProcedure(t)
			started := clock.Elapsed()
			err := tc.run(g, clock)
			got := g.evidence.ExpiredWait
			if code(err) != tc.code || (got == nil) != (tc.want == nil) {
				t.Fatalf("MUTATION: expired-wait-scope err %v diagnostic %+v", err, got)
			}
			if got == nil {
				return
			}
			if got.Purpose != tc.want.Purpose || !reflect.DeepEqual(got.Last, tc.want.Last) || got.Polls < 1 || got.WaitedMS <= 0 || got.WaitedMS > (clock.Elapsed()-started).Milliseconds() {
				t.Fatalf("MUTATION: expired-wait-scope got %+v want %+v", got, tc.want)
			}
		})
	}
}

// The public projection keeps only fixed codes and counts.
func TestExpiredWaitPublicRefusesFreeText(t *testing.T) {
	w := WaitDiagnostic{Purpose: "control-reconstructed http://192.0.2.1", WaitedMS: -1, Polls: -2, Last: []string{"w1-live", "worker 192.0.2.3:24711 stuck", "call-owner-w1", ""}}.Public()
	want := WaitDiagnostic{Purpose: "unavailable", Last: []string{"w1-live", "unavailable", "call-owner-w1", "unavailable"}}
	if !reflect.DeepEqual(w, want) {
		t.Fatalf("MUTATION: expired-wait-free-text got %+v", w)
	}
	for _, purpose := range waitPurposes {
		if (WaitDiagnostic{Purpose: purpose}).Public().Purpose != purpose {
			t.Fatal("fixed purpose refused", purpose)
		}
	}
	raw := json.RawMessage(`{"completedSteps":1,"workerRollouts":0,"restarts":0,"expiredWait":{"purpose":"free text","waitedMs":5,"polls":2,"last":["control-ready","192.0.2.3"]}}`)
	e, err := DecodeProcessEvidence(raw)
	if err != nil || e.ExpiredWait == nil || e.ExpiredWait.Purpose != "unavailable" || !reflect.DeepEqual(e.ExpiredWait.Last, []string{"control-ready", "unavailable"}) || e.ExpiredWait.Polls != 2 {
		t.Fatalf("MUTATION: expired-wait-free-text projection %+v %v", e.ExpiredWait, err)
	}
	encoded, _ := json.Marshal(e)
	if strings.Contains(string(encoded), "192.0.2") || strings.Contains(string(encoded), "free text") {
		t.Fatal("free text escaped the projection", string(encoded))
	}
}
