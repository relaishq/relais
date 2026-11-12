package operatorgates

import (
	"context"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/operator"
)

type stuckStore struct {
	clock *FakeClock
	reads []time.Duration
}

func (s *stuckStore) Observe(context.Context, time.Duration) (StoreObservation, error) {
	s.reads = append(s.reads, s.clock.Elapsed())
	return StoreObservation{Primary: "primary-a", Available: true, Durability: "sync"}, nil
}
func (s *stuckStore) Failover(context.Context, time.Duration) error { return nil }

// measureFixture observes one live call on w1, or calls extra calls.
func measureFixture(g *procedure, clock *FakeClock, calls int, reads *[]time.Duration) MeasurePorts {
	p := MeasurePorts{ProcedurePorts: g.p, Calls: 1}
	p.Observe = func(context.Context, time.Duration) (MeasurementObservation, error) {
		*reads = append(*reads, clock.Elapsed())
		s := liveStatus(g, controlplane.CallStatus{ID: "original", Owner: "w1", Epoch: 1})
		for i := 1; i < calls; i++ {
			s.Calls = append(s.Calls, controlplane.CallStatus{ID: "extra", Owner: "w2", Epoch: 1})
		}
		return MeasurementObservation{Status: &s, Local: map[string]mediaworker.WorkerStatus{"w1": {InventoryKnown: true, Sessions: 1, Inventory: []mediaworker.InstalledSession{{ID: "original", Epoch: 1, Owner: g.c.Parameters.Workers["w1"].Media}}}, "w2": {InventoryKnown: true, Inventory: []mediaworker.InstalledSession{}}}}, nil
	}
	return p
}

func intervals(t *testing.T, reads []time.Duration, want time.Duration) {
	t.Helper()
	if len(reads) < 3 {
		t.Fatal("wait did not poll", reads)
	}
	for i := 2; i < len(reads); i++ {
		if reads[i]-reads[i-1] != want {
			t.Fatal("poll interval changed", reads[i-1], reads[i])
		}
	}
}

// measurementScenarios run measurement waits that never see their condition.
// Each checks the characterized outcome, interval and window and returns the
// recorded expired wait.
func measurementScenarios() map[string]func(*testing.T) *WaitDiagnostic {
	return map[string]func(*testing.T) *WaitDiagnostic{
		"calls-settled": func(t *testing.T) *WaitDiagnostic {
			g, clock := testProcedure(t)
			var reads []time.Duration
			start := clock.Elapsed()
			r := RunMeasureGate(context.Background(), operator.Load, g.c, measureFixture(g, clock, 2, &reads), g.end)
			intervals(t, reads, 5*time.Second)
			if r.Status != operator.Inconclusive || r.Reason != "" || r.Measurement.MissingSamples != 14 || reads[len(reads)-1]-start != time.Minute {
				t.Fatalf("calls-settled result changed %+v %v", r, reads[len(reads)-1]-start)
			}
			return r.ExpiredWait
		},
		"failover-primary-recovered": func(t *testing.T) *WaitDiagnostic {
			g, clock := testProcedure(t)
			store := &stuckStore{clock: clock}
			var reads []time.Duration
			p := measureFixture(g, clock, 1, &reads)
			p.Store, p.BeforeFault, p.MarkFault = store, func(context.Context, time.Duration) error { return nil }, func(context.Context) error { return nil }
			r := RunMeasureGate(context.Background(), operator.Failover, g.c, p, g.end)
			intervals(t, store.reads[1:], 5*time.Second)
			if r.Status != operator.Inconclusive || r.Reason != "gate-deadline" || r.Measurement.RecoveredMS != nil {
				t.Fatalf("failover result changed %+v", r)
			}
			return r.ExpiredWait
		},
		"collection-complete": func(t *testing.T) *WaitDiagnostic {
			g, clock := testProcedure(t)
			var reads []time.Duration
			p := measureFixture(g, clock, 1, &reads)
			p.Collect = func(ctx context.Context, _ time.Duration) <-chan MeasurementCollection {
				ch := make(chan MeasurementCollection)
				go func() { <-ctx.Done(); close(ch) }()
				return ch
			}
			r := RunMeasureGate(context.Background(), operator.Load, g.c, p, g.end)
			intervals(t, reads[1:], 15*time.Second)
			if r.Status != operator.Inconclusive || r.Reason != "gate-deadline" || r.Measurement.MissingSamples != 83 {
				t.Fatalf("collection result changed %+v", r)
			}
			return r.ExpiredWait
		},
		"boundary-idle": func(t *testing.T) *WaitDiagnostic {
			g, clock := testProcedure(t)
			var reads []time.Duration
			g.p.Step = func(_ context.Context, op, _ string, _ time.Duration, _ ProcessExtra) (StepResult, error) {
				if op == "status" {
					reads = append(reads, clock.Elapsed())
					return StepResult{Status: liveStatus(g, controlplane.CallStatus{ID: "original", Owner: "w1", Epoch: 1})}, nil
				}
				return StepResult{}, nil
			}
			end := clock.Elapsed() + 10*time.Second
			var expired *WaitDiagnostic
			err := WaitBoundaryIdle(context.Background(), g.p, end, &expired)
			intervals(t, reads, 500*time.Millisecond)
			if code(err) != "gate-deadline" || reads[len(reads)-1] != end-500*time.Millisecond {
				t.Fatal("boundary idle result changed", err, reads[len(reads)-1])
			}
			return expired
		},
	}
}

// Measurement waits keep their outcomes, intervals and windows.
func TestMeasurementWaitsKeepResult(t *testing.T) {
	for name, run := range measurementScenarios() {
		t.Run(name, func(t *testing.T) { run(t) })
	}
}

// Each measurement wait that its window or the gate budget ends names itself
// with the last observed state.
func TestMeasurementWaitsNameExpiredWait(t *testing.T) {
	live := []string{"control-ready", "calls-one", "call-owner-w1", "w1-live", "w2-live"}
	want := map[string][]string{
		"calls-settled":              {"control-ready", "calls-many", "w1-live", "w2-live"},
		"failover-primary-recovered": append(slices.Clone(live), "store-available", "store-primary-same", "store-sync"),
		"collection-complete":        live,
		"boundary-idle":              {"control-ready", "calls-one", "call-owner-w1", "w1-live", "w2-live", "w1-local-unknown", "w2-local-unknown", "store-ownership-unknown"},
	}
	for name, run := range measurementScenarios() {
		t.Run(name, func(t *testing.T) {
			got := run(t)
			if got == nil || got.Purpose != name || got.Polls < 2 || got.WaitedMS <= 0 || !reflect.DeepEqual(got.Last, want[name]) || !got.valid() {
				t.Fatalf("MUTATION: measurement-expired-wait %+v", got)
			}
		})
	}
}
