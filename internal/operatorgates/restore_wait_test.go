package operatorgates

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/operator"
)

// restoreCase runs a gate's restoration of stopped worker w1 with a control
// plane that never reports the wanted state.
type restoreCase struct {
	name      string
	left      time.Duration // restoration budget left when finish starts
	status    func(*procedure, controlplane.CallStatus) controlplane.Status
	interval  time.Duration
	procedure string
	purpose   string
	last      []string
}

func restoreCases() []restoreCase {
	stalled := func(g *procedure, call controlplane.CallStatus) controlplane.Status {
		s := liveStatus(g, call)
		s.Workers[0].Calls = 0
		return s
	}
	unstarted := func(g *procedure, call controlplane.CallStatus) controlplane.Status {
		s := stalled(g, call)
		s.Workers[0].Dead = true
		return s
	}
	return []restoreCase{
		{"settle-window", 10 * time.Minute, stalled, 500 * time.Millisecond, "restore-unsettled", "restore-worker-settled", []string{"control-ready", "calls-one", "call-owner-w2", "w1-live", "w2-live"}},
		{"alive-window", 10 * time.Minute, unstarted, 500 * time.Millisecond, "restore-unconfirmed", "restore-worker-alive", []string{"control-ready", "calls-one", "call-owner-w2", "w1-dead", "w2-live"}},
		{"settle-budget", 40 * time.Second, stalled, 500 * time.Millisecond, "restore-unsettled", "restore-worker-settled", []string{"control-ready", "calls-one", "call-owner-w2", "w1-live", "w2-live"}},
		{"settle-floor", 29 * time.Second, stalled, 500 * time.Millisecond, "precondition-failed:deadline-before-submission", "restore-worker-settled", []string{"unobserved"}},
	}
}

func runRestoreCase(t *testing.T, tc restoreCase) (*procedure, ProcedureResult, []time.Duration) {
	t.Helper()
	g, clock := testProcedure(t)
	call := controlplane.CallStatus{ID: "original", Owner: "w2", Epoch: 1}
	g.restoreEnd = clock.Elapsed() + tc.left
	g.end = g.restoreEnd - RestoreReserve
	g.down, g.stopped = []string{"w1"}, []string{"w1"}
	var polls []time.Duration
	g.p.Step = floorStep(clock, &polls, func(string) controlplane.Status { return tc.status(g, call) })
	g.p.Media = func(context.Context) (*MediaSummary, error) { return &MediaSummary{Status: operator.Passed}, nil }
	return g, g.finish(context.Background(), fault("missing-live-call"), false), polls
}

// Restoration waits keep their own window, poll interval and codes, and a
// failed restoration still latches the session as restore-failed.
func TestRestorationWaitKeepsResult(t *testing.T) {
	for _, tc := range restoreCases() {
		t.Run(tc.name, func(t *testing.T) {
			g, r, polls := runRestoreCase(t, tc)
			if r.Status != operator.Inconclusive || r.Reason != "restore-failed" || r.Category != "" || r.ProcedureReason != diagnosticCode(tc.procedure) || r.Completed || g.unknown {
				t.Fatal("restoration result changed", r)
			}
			for i := 1; i < len(polls); i++ {
				if polls[i]-polls[i-1] != tc.interval {
					t.Fatal("restoration poll interval changed", polls[i-1], polls[i])
				}
			}
			s, _ := fixtureSession(t)
			s.CompleteGate(r.GateResult(operator.ControlRestart), s.clock.Elapsed()+time.Minute)
			if !s.Latched() {
				t.Fatal("failed restoration no longer latches")
			}
		})
	}
}

// A restoration wait that runs out its window, the gate budget or the
// submission floor names itself; the restoration result is unchanged.
func TestRestorationWaitNamesExpiredWait(t *testing.T) {
	for _, tc := range restoreCases() {
		t.Run(tc.name, func(t *testing.T) {
			_, r, polls := runRestoreCase(t, tc)
			got := r.Process.ExpiredWait
			if r.Reason != "restore-failed" || r.ProcedureReason != diagnosticCode(tc.procedure) || got == nil || got.Purpose != tc.purpose || !reflect.DeepEqual(got.Last, tc.last) {
				t.Fatalf("MUTATION: restore-expired-wait %s %+v", r.Reason, got)
			}
			if tc.purpose == "restore-worker-settled" && got.Polls != len(polls) {
				t.Fatal("restoration polls miscounted", got.Polls, len(polls))
			}
		})
	}
}

// WorkerEmpty keeps its one-second retry and precondition code; as a gate
// wait it records the window or budget that ended it.
func TestWorkerEmptyNamesExpiredWait(t *testing.T) {
	for _, tc := range []struct {
		name string
		left time.Duration
		code string
		last []string
	}{
		{"window", 10 * time.Minute, "precondition-failed:not-draining", []string{"control-ready", "calls-one", "call-owner-w2", "w1-live", "w2-live"}},
		{"budget", time.Minute, "gate-deadline", []string{"unobserved"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, clock := testProcedure(t)
			g.end = clock.Elapsed() + tc.left
			call := controlplane.CallStatus{ID: "original", Owner: "w2", Epoch: 1}
			var polls []time.Duration
			g.p.Step = floorStep(clock, &polls, func(string) controlplane.Status { return liveStatus(g, call) })
			err := g.workerEmpty(context.Background(), "w1")
			for i := 1; i < len(polls); i++ {
				if polls[i]-polls[i-1] != time.Second {
					t.Fatal("worker-empty retry interval changed", polls[i-1], polls[i])
				}
			}
			got := g.evidence.ExpiredWait
			if code(err) != tc.code || got == nil || got.Purpose != "worker-empty" || got.Polls != len(polls) || !reflect.DeepEqual(got.Last, tc.last) {
				t.Fatalf("MUTATION: worker-empty-expired-wait %v %+v", err, got)
			}
		})
	}
}
