package operatorgates

import (
	"context"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/operator"
)

// reserveClock finishes collection only when the telemetry loop checks its
// cutoff, after its non-blocking collection read. This pins the polling seam.
type reserveClock struct {
	*FakeClock
	cutoff time.Duration
	finish func()
}

func (c *reserveClock) Elapsed() time.Duration {
	elapsed := c.FakeClock.Elapsed()
	if elapsed >= c.cutoff && c.finish != nil {
		finish := c.finish
		c.finish = nil
		finish()
	}
	return elapsed
}

// The procedure may stop telemetry at its cutoff while collection retains its
// original reserve. Use a handshake, not wall-clock sleeps, to finish there.
func reservedMeasurement(t *testing.T, gate operator.Gate, mode string) MeasureResult {
	t.Helper()
	g, clock := testProcedure(t)
	var reads []time.Duration
	p := measureFixture(g, clock, 1, &reads)
	if gate == operator.Failover {
		// The production failover definition, including its trials, runs before
		// collection. Its completion is independent of the collector's schedule.
		w, ports := newRoundTripWorld(t, false)
		g = newProcedure(w.runtime.Config, ports, GateMaximum)
		clock = w.clock
		p = MeasurePorts{ProcedurePorts: ports, Calls: 1, Observe: w.runtime.Observe, Store: &roundTripStore{}, BeforeFault: func(context.Context, time.Duration) error { return nil }, MarkFault: func(context.Context) error { return nil }}
	}
	end := g.end
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.CollectionContext, p.CollectionEnd = parent, end+time.Minute
	done := make(chan MeasurementCollection, 1)
	finish := make(chan struct{})
	completed := make(chan struct{})
	collecting := false
	observe := p.Observe
	p.Observe = func(ctx context.Context, end time.Duration) (MeasurementObservation, error) {
		if collecting {
			if mode == "observation-deadline" {
				return MeasurementObservation{}, fault("gate-deadline")
			}
			return MeasurementObservation{At: clock.Wall()}, nil
		}
		return observe(ctx, end)
	}
	p.Collect = func(context.Context, time.Duration) <-chan MeasurementCollection {
		collecting = true
		clock.Advance(end - clock.Elapsed() - time.Second)
		go func() {
			<-finish
			done <- MeasurementCollection{Evidence: MeasurementEvidence{Media: &MediaSummary{Status: operator.Inconclusive}}}
			close(completed)
			close(done)
		}()
		return done
	}
	finishCollection := func() {
		clock.Advance(time.Second)
		close(finish)
		<-completed
	}
	if mode == "telemetry-cutoff" {
		p.Clock = &reserveClock{FakeClock: clock, cutoff: end, finish: finishCollection}
	}
	sleep := p.Sleep
	p.Sleep = func(ctx context.Context, d time.Duration) error {
		if clock.Elapsed() < end-time.Second {
			return sleep(ctx, d)
		}
		clock.Advance(d)
		if mode == "observation-deadline" {
			finishCollection()
		}
		return nil
	}
	out := RunMeasureGate(context.Background(), gate, g.c, p, end)
	if clock.Elapsed() >= p.CollectionEnd || out.Measurement.MissingSamples < 1 || out.Media == nil || out.Completed || out.Reason != "" {
		t.Fatalf("MUTATION: measurement-collection-reserve: mode=%s result=%+v elapsed=%v", mode, out, clock.Elapsed())
	}
	want := "measurement-telemetry-cutoff"
	if mode == "observation-deadline" {
		want = "measurement-observation-failed"
	}
	if out.Measurement.ErrorReason != want {
		t.Fatalf("MUTATION: measurement-collection-reserve: diagnostic=%q want=%q", out.Measurement.ErrorReason, want)
	}
	return out
}

func TestMeasurementCollectionReserveDoesNotStopSequence(t *testing.T) {
	for _, mode := range []string{"telemetry-cutoff", "observation-deadline"} {
		t.Run(mode, func(t *testing.T) {
			measured := reservedMeasurement(t, operator.Failover, mode)
			s, _ := fixtureSession(t)
			s.gates = []operator.Gate{operator.Failover, operator.RelayDeploy, operator.Load}
			ran := []operator.Gate{}
			seq := s.RunSequence(context.Background(), SequencePorts{Command: func(_ context.Context, gate operator.Gate, _ time.Duration) ProcedureResult {
				ran = append(ran, gate)
				if gate == operator.Failover {
					return measureProcedureResult(measured)
				}
				return ProcedureResult{Status: operator.Inconclusive, Completed: true}
			}})
			if s.Latched() || len(ran) != 3 || seq.Gates[2].Status == operator.NotRun || seq.Gates[2].Reason == "prior-gate-stopped" || s.Final(seq.Gates).Diagnostics[0].Procedure.Reason != measured.Measurement.ErrorReason {
				t.Fatalf("MUTATION: measurement-collection-reserve: ran=%v result=%+v", ran, seq)
			}
		})
	}
}

func TestAdmissionCollectionReserveRunsBoundary(t *testing.T) {
	for _, mode := range []string{"telemetry-cutoff", "observation-deadline"} {
		t.Run(mode, func(t *testing.T) {
			measured := reservedMeasurement(t, operator.Load, mode)
			boundary, stopped, idle := false, false, false
			out := RunAdmission(context.Background(), AdmissionPorts{
				Survivor:     func(context.Context, time.Duration) MeasureResult { return measured },
				StopSurvivor: func(context.Context, time.Duration) bool { stopped = true; return true },
				Idle:         func(context.Context, time.Duration) error { idle = true; return nil },
				Boundary: func(context.Context, time.Duration) MeasureResult {
					boundary = true
					return MeasureResult{Status: operator.Inconclusive, Completed: true}
				},
			}, GateMaximum)
			procedure := admissionProcedureResult(out, nil)
			if !boundary || !stopped || !idle || out.Reason != "" || procedure.ProcedureReason != measured.Measurement.ErrorReason {
				t.Fatalf("MUTATION: admission-collection-reserve: result=%+v boundary=%v stopped=%v idle=%v", out, boundary, stopped, idle)
			}
		})
	}
}

func TestMeasurementSkippedCollectionIsIncomplete(t *testing.T) {
	g, clock := testProcedure(t)
	var reads []time.Duration
	p := measureFixture(g, clock, 1, &reads)
	parent, cancel := context.WithCancel(context.Background())
	cancel()
	p.CollectionContext, p.CollectionEnd = parent, g.end+time.Minute
	collected := false
	p.Collect = func(context.Context, time.Duration) <-chan MeasurementCollection { collected = true; return nil }
	out := RunMeasureGate(context.Background(), operator.Load, g.c, p, g.end)
	// One missing telemetry sample was already counted by the summary; skipping
	// required collection must add one more and retain its own fixed diagnostic.
	if collected || out.Completed || out.Reason != "" || out.Measurement.ErrorReason != "measurement-collection-failed" || out.Measurement.MissingSamples != 2 {
		t.Fatalf("MUTATION: measurement-skipped-collection: collected=%v result=%+v", collected, out)
	}
}

func TestMeasurementEvidenceStopCodesAreDiagnosticOnly(t *testing.T) {
	for _, code := range []string{"gate-deadline", "command-result-unknown", "restore-failed", "interrupted"} {
		t.Run(code, func(t *testing.T) {
			g, clock := testProcedure(t)
			var reads []time.Duration
			p := measureFixture(g, clock, 1, &reads)
			p.Persist = func(context.Context, []MeasurementObservation) error { return fault(code) }
			out := RunMeasureGate(context.Background(), operator.Load, g.c, p, g.end)
			if out.Reason != "" || out.Completed || out.Measurement.ErrorReason != "measurement-persist-failed" {
				t.Fatalf("MUTATION: measurement-diagnostic-stop-code: %+v", out)
			}
		})
	}
}
