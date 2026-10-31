package operatorgates

import (
	"context"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/operator"
)

func TestResourceSequencePreparationConsumesCallerBudget(t *testing.T) {
	for _, exhausted := range []bool{false, true} {
		t.Run(map[bool]string{false: "delivery-delays", true: "preparation-exhausted"}[exhausted], func(t *testing.T) {
			s, clock := fixtureSession(t)
			caller := &CallerRuntime{End: 20 * time.Minute}
			if exhausted {
				caller.End = 3 * time.Minute
			}
			cutoff := caller.End
			var collected, stopped bool
			commandCalls := 0
			result := s.RunSequence(context.Background(), SequencePorts{
				Prepare: func(context.Context, operator.Gate, time.Duration, time.Duration) error {
					clock.Advance(115 * time.Second)
					return nil
				},
				Command: func(ctx context.Context, _ operator.Gate, end time.Duration) ProcedureResult {
					p := ProcedurePorts{Clock: clock, Media: func(ctx context.Context) (*MediaSummary, error) {
						collected = true
						if clock.Elapsed() > cutoff-2*time.Minute || caller.End > cutoff-time.Minute {
							t.Fatal("collection reserve lost or caller cutoff renewed")
						}
						if ctx.Err() != nil {
							t.Fatal("collection inherited expired procedure context")
						}
						clock.Advance(30 * time.Second)
						return &MediaSummary{Status: operator.Inconclusive}, nil
					}}
					return runResourceCommand(ctx, clock, caller, end, p, func(ctx context.Context, p ProcedurePorts, end time.Duration) ProcedureResult {
						commandCalls++
						if end > cutoff-2*time.Minute {
							t.Fatal("procedure/restoration renewed the immutable caller cutoff")
						}
						deadline, ok := ctx.Deadline()
						if !ok || time.Until(deadline) > end-clock.Elapsed()+time.Second {
							t.Fatal("procedure context exceeds its end")
						}
						// Repeated delivery delays consume the same window. Restoration uses
						// the real procedure fence and cannot submit once its end is reached.
						g := newProcedure(&ProcessConfig{ArmedAt: time.UnixMilli(s.Budget.armed)}, p, end)
						g.frozen = true
						g.p.Step = func(ctx context.Context, _ string, _ string, stepEnd time.Duration, _ ProcessExtra) (StepResult, error) {
							if stepEnd > end || clock.Elapsed() >= end || ctx.Err() != nil {
								t.Fatal("late restoration effect")
							}
							clock.Advance(500 * time.Millisecond)
							return StepResult{}, nil
						}
						clock.Advance(end - clock.Elapsed() - time.Second)
						if err := g.restoreGate(ctx); err != nil {
							t.Fatal(err)
						}
						clock.Advance(end - clock.Elapsed())
						g.frozen = true
						if code(g.restoreGate(ctx)) != "gate-deadline" {
							t.Fatal("restoration escaped cutoff")
						}
						cancelled, cancel := context.WithCancel(ctx)
						cancel()
						media, err := p.Media(cancelled)
						if err != nil {
							t.Fatal(err)
						}
						return ProcedureResult{Status: operator.Inconclusive, Media: media}
					}, func(ctx context.Context, end time.Duration) bool {
						stopped = true
						if end > cutoff || clock.Elapsed() >= cutoff || ctx.Err() != nil {
							t.Fatal("stop reserve lost")
						}
						return true
					})
				},
			})
			if exhausted {
				if commandCalls != 0 {
					t.Fatal("procedure started without collection/stop reserves")
				}
			} else if commandCalls != 1 || !collected || !stopped || len(result.Gates) != 1 {
				t.Fatal("lifecycle not exercised")
			}
			if caller.End > cutoff || clock.Elapsed() >= cutoff {
				t.Fatal("caller deadline renewed or exceeded")
			}
		})
	}
}

func TestResourceCollectionHasItsOwnWindow(t *testing.T) {
	s, clock := fixtureSession(t)
	caller := &CallerRuntime{End: 20 * time.Minute}
	lost := false
	p := ProcedurePorts{Clock: clock, Step: func(ctx context.Context, _ string, _ string, end time.Duration, _ ProcessExtra) (StepResult, error) {
		if ctx.Err() != nil || end != 19*time.Minute || clock.Elapsed() >= end {
			t.Fatal("post-collection status lost its window")
		}
		row := StepResult{}
		if lost {
			row.Status.LostCount = 1
		}
		return row, nil
	}, Media: func(ctx context.Context) (*MediaSummary, error) {
		if ctx.Err() != nil {
			t.Fatal("collection inherited procedure cancellation")
		}
		clock.Advance(30 * time.Second)
		return &MediaSummary{Status: operator.Inconclusive}, nil
	}}
	out := runResourceCommand(context.Background(), clock, caller, 21*time.Minute, p, func(ctx context.Context, p ProcedurePorts, end time.Duration) ProcedureResult {
		clock.Advance(end - clock.Elapsed() - time.Second)
		g := newProcedure(&ProcessConfig{ArmedAt: time.UnixMilli(s.Budget.armed), Pool: true}, p, end)
		result := g.finish(ctx, nil, false)
		if !result.Completed || !result.AwaitingPool || result.Reason != "" {
			t.Fatal("reserved collection time counted as procedure lateness")
		}
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := p.PostMediaStatus(cancelled); err != nil {
			t.Fatal(err)
		}
		lost = true
		if _, err := p.PostMediaStatus(cancelled); code(err) != "known-call-loss" {
			t.Fatal("post-media status hid known call loss")
		}

		return result
	}, func(ctx context.Context, end time.Duration) bool { return ctx.Err() == nil && end <= 20*time.Minute })
	if !out.Completed || out.Reason != "" {
		t.Fatal("normal collection changed procedure completion")
	}
}

func TestResourceMeasurementCollectionReserve(t *testing.T) {
	s, clock := fixtureSession(t)
	caller := &CallerRuntime{End: 20 * time.Minute}
	p := ProcedurePorts{Clock: clock, Broker: &testGateBroker{reg: Registration{Near: true}}}
	runResourceCommand(context.Background(), clock, caller, 21*time.Minute, p, func(ctx context.Context, p ProcedurePorts, end time.Duration) ProcedureResult {
		runtime := &ProcessRuntime{commandPorts: &p}
		// Bind the real resource measurement ports, including the boundary caller.
		runtime.Session = s
		ports := runtime.measurePorts(caller, 1)
		if ports.CollectionContext != p.CollectionContext || ports.CollectionEnd != 19*time.Minute {
			t.Fatal("resource measurement lost its collection window")
		}
		ports.Broker = p.Broker
		ports.Observe = func(context.Context, time.Duration) (MeasurementObservation, error) {
			return MeasurementObservation{}, fault("missing-live-inventory")
		}
		ports.Collect = func(ctx context.Context, end time.Duration) <-chan MeasurementCollection {
			if end != 19*time.Minute || ctx.Err() != nil {
				t.Fatal("measurement collection uses procedure deadline")
			}
			clock.Advance(18*time.Minute + 30*time.Second)
			ch := make(chan MeasurementCollection, 1)
			ch <- MeasurementCollection{Evidence: MeasurementEvidence{Media: &MediaSummary{Status: operator.Inconclusive}}}
			close(ch)
			return ch
		}
		out := RunMeasureGate(ctx, operator.Load, &ProcessConfig{ArmedAt: time.UnixMilli(s.Budget.armed)}, ports, end)
		if out.Media == nil || out.Reason == "gate-deadline" {
			t.Fatal("measurement discarded reserved collection")
		}
		return ProcedureResult{Status: out.Status, Media: out.Media}
	}, func(context.Context, time.Duration) bool { return true })
}

func TestExpiredCallerWindowKeepsGateDeadline(t *testing.T) {
	_, clock := fixtureSession(t)
	clock.Advance(21 * time.Minute)
	stops := 0
	result := runResourceCommand(context.Background(), clock, &CallerRuntime{End: 20 * time.Minute}, 40*time.Minute, ProcedurePorts{}, func(context.Context, ProcedurePorts, time.Duration) ProcedureResult {
		t.Fatal("expired procedure ran")
		return ProcedureResult{}
	}, func(context.Context, time.Duration) bool { stops++; return false })
	if result.Reason != "gate-deadline" || result.Status != operator.Inconclusive || stops != 0 {
		t.Fatal("expired caller window became an unknown stop")
	}
}

func TestSequenceStopDeadlineClassification(t *testing.T) {
	for _, mode := range []string{"unknown-stop", "expired-before-stop", "unknown-after-submission"} {
		t.Run(mode, func(t *testing.T) {
			s, clock := fixtureSession(t)
			result := s.RunSequence(context.Background(), SequencePorts{Command: func(context.Context, operator.Gate, time.Duration) ProcedureResult {
				if mode == "expired-before-stop" {
					clock.Advance(s.Budget.end)
				}
				return ProcedureResult{Status: operator.Inconclusive, Reason: "gate-deadline"}
			}, ReleaseCallers: func(_ context.Context, _ string, end time.Duration) bool {
				if mode == "unknown-after-submission" {
					clock.Advance(end - clock.Elapsed())
				}
				return false
			}})
			want := "command-result-unknown"
			if mode == "expired-before-stop" {
				want = "gate-deadline"
			}
			if result.Gates[0].Reason != want || result.Gates[0].Status != operator.Inconclusive || !s.Latched() {
				t.Fatal("stop certainty classification", result.Gates)
			}
		})
	}
}

func TestUnconfirmedStopDoesNotInheritProcedureDeadline(t *testing.T) {
	got := Stopped(false, ProcedureResult{Status: operator.Inconclusive, Reason: "gate-deadline"})
	if got.Reason != "command-result-unknown" {
		t.Fatal("procedure deadline is not proof of an expired stop", got.Reason)
	}
}

func TestResourceUnconfirmedStopRetainsUnknownOutcome(t *testing.T) {
	for _, timesOut := range []bool{false, true} {
		t.Run(map[bool]string{false: "unknown-stop", true: "timeout-after-submission"}[timesOut], func(t *testing.T) {
			_, clock := fixtureSession(t)
			out := runResourceCommand(context.Background(), clock, &CallerRuntime{End: 20 * time.Minute}, 20*time.Minute, ProcedurePorts{}, func(context.Context, ProcedurePorts, time.Duration) ProcedureResult {
				return ProcedureResult{Status: operator.Inconclusive, Reason: "gate-deadline"}
			}, func(_ context.Context, end time.Duration) bool {
				if timesOut {
					clock.Advance(end - clock.Elapsed())
				}
				return false
			})
			if out.Reason != "command-result-unknown" || out.StopDeadlineExpired {
				t.Fatal("submitted stop lost its unknown outcome", out.Reason)
			}
		})
	}
}

func TestSequenceRetainsExpiredResourceStop(t *testing.T) {
	s, clock := fixtureSession(t)
	result := s.RunSequence(context.Background(), SequencePorts{Command: func(ctx context.Context, _ operator.Gate, end time.Duration) ProcedureResult {
		clock.Advance(21 * time.Minute)
		return runResourceCommand(ctx, clock, &CallerRuntime{End: 20 * time.Minute}, end, ProcedurePorts{}, func(context.Context, ProcedurePorts, time.Duration) ProcedureResult {
			t.Fatal("expired procedure ran")
			return ProcedureResult{}
		}, func(context.Context, time.Duration) bool { t.Fatal("expired stop submitted"); return false })
	}, ReleaseCallers: func(context.Context, string, time.Duration) bool { return false }})
	if result.Gates[0].Reason != "gate-deadline" || !s.Latched() {
		t.Fatal("expired resource stop classification", result.Gates)
	}
}
