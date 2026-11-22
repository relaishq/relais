package operatorgates

import (
	"context"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/operator"
)

func trialReadyProcedure(t *testing.T, readyAfter int) (*procedure, *FakeClock, *int) {
	t.Helper()
	g, clock := testProcedure(t)
	moved, polls := false, 0
	g.p.Step = func(_ context.Context, op, _ string, _ time.Duration, _ ProcessExtra) (StepResult, error) {
		if op == "move" {
			moved = true
			m := controlplane.MoveResult{ID: "original", From: "w1", To: "w2", Kind: "move", Start: clock.Wall(), End: clock.Wall()}
			return StepResult{Certainty: operator.Succeeded, Move: m, Raw: oracleJSON(m)}, nil
		}
		s := liveStatus(g, controlplane.CallStatus{ID: "original", Owner: "w1", Epoch: 1})
		if moved {
			polls++
			s.Calls[0].Owner, s.Calls[0].Epoch = "w2", 2
			s.Workers[0].Recovering = readyAfter == 0 || polls < readyAfter
		}
		return StepResult{Certainty: operator.Succeeded, LossKnown: true, Status: s}, nil
	}
	return g, clock, &polls
}

func TestTrialReadyWithLiveCaller(t *testing.T) {
	for _, ready := range []bool{true, false} {
		name, readyAfter := "deadline", 0
		if ready {
			name, readyAfter = "success", 3
		}
		t.Run(name, func(t *testing.T) {
			g, clock, polls := trialReadyProcedure(t, readyAfter)
			err := WorkerTrials(context.Background(), g.c, g.p, g.end, TrialTarget{Moves: 1}, "stable")
			if ready {
				if err != nil || *polls != 3 || clock.Elapsed() != ConsentWindow+time.Second {
					t.Fatalf("live caller readiness changed: err %v polls %d elapsed %v", err, *polls, clock.Elapsed())
				}
			} else if code(err) != "gate-deadline" || clock.Elapsed() != g.end || *polls < 2 {
				t.Fatalf("live caller deadline changed: err %v polls %d elapsed %v end %v", err, *polls, clock.Elapsed(), g.end)
			}
		})
	}
}

type stoppingGateBroker struct {
	*testGateBroker
	stopped func() bool
}

func (b stoppingGateBroker) CheckStopped(context.Context) error {
	if b.stopped() {
		return fault("precondition-failed:caller")
	}
	return nil
}

func TestTrialReadyStopsWithCaller(t *testing.T) {
	g, clock, polls := trialReadyProcedure(t, 0)
	g.p.Broker = stoppingGateBroker{g.p.Broker.(*testGateBroker), func() bool { return *polls >= 2 }}
	err := g.trials(context.Background(), TrialTarget{Moves: 1}, "stable")
	if code(err) != "precondition-failed:caller" || *polls != 2 || clock.Elapsed() != ConsentWindow+time.Second {
		t.Fatalf("stopped caller polled to deadline: err %v polls %d elapsed %v", err, *polls, clock.Elapsed())
	}
	g.p.Media = func(context.Context) (*MediaSummary, error) { return &MediaSummary{Status: operator.Passed}, nil }
	r := g.finish(context.Background(), err, false)
	if r.Completed || r.AwaitingPool || r.Status == operator.Passed || r.Reason != "precondition-failed" || r.ProcedureReason != "precondition-failed:caller" || r.Category != "caller" {
		t.Fatalf("caller abort qualified or lost its cause: %+v", r)
	}
}
