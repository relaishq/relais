package operatorrehearsal

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/operatorcaller"
	"github.com/relaishq/relais/internal/operatorgates"
	"github.com/relaishq/relais/pkg/operator"
)

// Exercise real campaign admission with a clock that moves between reads,
// after controls/calibration and with the full caller preparation allowance.
type campaignClock struct{ operatorgates.FakeClock }

func (c *campaignClock) Elapsed() time.Duration {
	c.Advance(time.Microsecond)
	return c.Tick
}
func TestCampaignsFitAfterCalibrationAndCallerPreparation(t *testing.T) {
	for _, setup := range []time.Duration{5*time.Minute - time.Second, 5 * time.Minute} {
		t.Run(setup.String(), func(t *testing.T) {
			base := time.UnixMilli(1_000_000)
			clock := &campaignClock{FakeClock: operatorgates.FakeClock{Now: base}}
			init := operator.Init{Scenario: operator.Scenario{SchemaVersion: 1, Digest: strings.Repeat("a", 64), Gates: CoveredGates}, ArmedAtUnixMS: strconv.FormatInt(base.UnixMilli(), 10), EndUnixMS: strconv.FormatInt(base.Add(operatorgates.SessionMaximum).UnixMilli(), 10)}
			session, err := operatorgates.NewSession("campaign", init, clock)
			if err != nil {
				t.Fatal(err)
			}
			// The runner spent about five minutes in controls and calibration before
			// the first gate. None of that consumes its fresh caller window.
			clock.Advance(setup)
			commands := 0
			sequence := session.RunSequence(context.Background(), operatorgates.SequencePorts{
				Minimum: campaignMinimum,
				Prepare: func(_ context.Context, gate operator.Gate, end, gateEnd time.Duration) error {
					now := clock.Elapsed()
					callerEnd, fits := callerWindow(gate, now, gateEnd)
					if !fits || callerEnd-now != min(operatorcaller.CallerMaximum, gateEnd-now) {
						t.Fatalf("caller does not fit: %s end=%s", gate, callerEnd)
					}
					clock.Advance(calibrationPreparation)
					// The bridge runtime includes setup, unlike the procedure window. The
					// media must finish inside its original deadline without renewing it.
					if clock.Elapsed()+time.Duration(duration(gate))*time.Second+4*time.Minute > callerEnd {
						t.Fatal("caller media cannot finish in immutable runtime")
					}
					if clock.Elapsed() >= end {
						if setup == 5*time.Minute && gate == operator.RelayDeploy {
							return &operatorgates.ProcedureError{Code: "gate-deadline"}
						}
						t.Fatal("caller preparation overran its budget")
					}
					return nil
				},
				Command: func(_ context.Context, gate operator.Gate, end time.Duration) operatorgates.ProcedureResult {
					commands++
					now := clock.Elapsed()
					if end-now > operatorgates.GateMaximum || end-now < time.Duration(duration(gate))*time.Second || end > session.Budget.MutationEnd() {
						t.Fatalf("gate window changed: %s end=%s now=%s", gate, end, now)
					}
					clock.Advance(time.Duration(duration(gate)) * time.Second)
					return operatorgates.ProcedureResult{Status: operator.Passed, Media: &operatorgates.MediaSummary{Status: operator.Passed}}
				},
				ReleaseCallers: func(context.Context, string, time.Duration) bool { return true },
				Finalize: func(context.Context, time.Duration) (*operatorgates.MediaSummary, error) {
					return &operatorgates.MediaSummary{Status: operator.Passed}, nil
				},
			})
			if setup == 5*time.Minute {
				if commands != 3 || sequence.Status != operator.Inconclusive || sequence.Gates[3].Reason != "gate-deadline" {
					t.Fatalf("strict preparation cutoff changed: commands=%d result=%+v", commands, sequence)
				}
			} else if commands != len(CoveredGates) || sequence.Status != operator.Passed {
				t.Fatalf("campaigns skipped after real preparation allowance: commands=%d result=%+v", commands, sequence)
			}
		})
	}
}

func TestCallerWindowPreservesCampaignFitReserve(t *testing.T) {
	for _, gate := range CoveredGates {
		media := time.Duration(duration(gate)) * time.Second
		for _, remaining := range []time.Duration{34 * time.Minute, 33 * time.Minute, 31 * time.Minute, media + 300*time.Second, media + 300*time.Second - time.Nanosecond, media + 120*time.Second} {
			now := 5 * time.Minute
			end, fits := callerWindow(gate, now, now+remaining)
			if end != now+min(operatorcaller.CallerMaximum, remaining) || fits != (remaining >= media+300*time.Second) {
				t.Fatalf("caller window changed: gate=%s remaining=%s end=%s fits=%v", gate, remaining, end, fits)
			}
		}
	}
}
