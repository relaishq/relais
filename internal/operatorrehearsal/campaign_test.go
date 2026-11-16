package operatorrehearsal

import (
	"context"
	"reflect"
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

// The timing model uses the measured worst setup (8m20s) and 111 seconds
// between each media phase and the next, including caller preparation.
func TestCampaignsFitAfterCalibrationAndCallerPreparation(t *testing.T) {
	base := time.UnixMilli(1_000_000)
	clock := &campaignClock{FakeClock: operatorgates.FakeClock{Now: base}}
	r := &rehearsal{clock: clock, boundInit: operator.Init{Scenario: operator.Scenario{SchemaVersion: 1, Digest: strings.Repeat("a", 64), Gates: CoveredGates}, ArmedAtUnixMS: strconv.FormatInt(base.UnixMilli(), 10), EndUnixMS: strconv.FormatInt(base.Add(operatorgates.SessionMaximum).UnixMilli(), 10)}}
	clock.Advance(8*time.Minute + 20*time.Second)
	init := r.pinnedInit()
	session, err := operatorgates.NewSession("campaign", init, clock)
	if err != nil {
		t.Fatal(err)
	}
	commands := 0
	var mutationMargin time.Duration
	sequence := session.RunSequence(context.Background(), operatorgates.SequencePorts{
		Minimum: campaignMinimum,
		Prepare: func(_ context.Context, gate operator.Gate, end, gateEnd time.Duration) error {
			now := clock.Elapsed()
			callerEnd, fits := callerWindow(gate, now, gateEnd)
			if !fits || callerEnd-now != min(operatorcaller.CallerMaximum, gateEnd-now) {
				t.Fatal("MUTATION: rehearsal-fresh-arm: caller does not fit", gate)
			}
			clock.Advance(100 * time.Second)
			if clock.Elapsed() >= end || clock.Elapsed()+time.Duration(duration(gate))*time.Second+4*time.Minute > callerEnd {
				t.Fatal("MUTATION: rehearsal-fresh-arm: caller preparation or media overran", gate)
			}
			return nil
		},
		Command: func(_ context.Context, gate operator.Gate, end time.Duration) operatorgates.ProcedureResult {
			commands++
			now := clock.Elapsed()
			if end-now > operatorgates.GateMaximum || end > session.Budget.MutationEnd() {
				t.Fatal("gate window exceeds safety cutoff")
			}
			media := 600 * time.Second
			if gate != operator.RelayDeploy {
				media = (1020 + 480) * time.Second
			}
			if time.Duration(duration(gate))*time.Second != media {
				t.Fatal("media duration differs", gate)
			}
			clock.Advance(media)
			return operatorgates.ProcedureResult{Status: operator.Passed, Media: &operatorgates.MediaSummary{Status: operator.Passed}}
		},
		ReleaseCallers: func(context.Context, string, time.Duration) bool { clock.Advance(11 * time.Second); return true },
		Finalize: func(_ context.Context, end time.Duration) (*operatorgates.MediaSummary, error) {
			mutationMargin = session.Budget.MutationEnd() - clock.Elapsed()
			if end-clock.Elapsed() > operatorgates.PoolReserve || end-clock.Elapsed() < operatorgates.PoolReserve-time.Second {
				t.Fatal("pool reserve changed")
			}
			clock.Advance(operatorgates.PoolReserve - time.Second)
			return &operatorgates.MediaSummary{Status: operator.Passed}, nil
		},
	})
	if commands != 4 || sequence.Status != operator.Passed || mutationMargin < 10*time.Minute+35*time.Second || session.Budget.Remaining() < 10*time.Minute+35*time.Second {
		t.Fatalf("MUTATION: rehearsal-fresh-arm: commands=%d mutation margin=%s session remaining=%s result=%+v", commands, mutationMargin, session.Budget.Remaining(), sequence)
	}
}

func TestPinnedInitExcludesSetup(t *testing.T) {
	base := time.UnixMilli(1_000_000)
	clock := &operatorgates.FakeClock{Now: base}
	original := operator.Init{ClockBinding: &operator.ClockBindingArtifacts{}, Scenario: operator.Scenario{SchemaVersion: 1, Digest: strings.Repeat("a", 64), Gates: CoveredGates}, ArmedAtUnixMS: strconv.FormatInt(base.UnixMilli(), 10), EndUnixMS: strconv.FormatInt(base.Add(operatorgates.SessionMaximum).UnixMilli(), 10)}
	r := &rehearsal{clock: clock, boundInit: original}
	clock.Advance(8*time.Minute + 20*time.Second)
	init := r.pinnedInit()
	if init.ArmedAtUnixMS != strconv.FormatInt(clock.Wall().UnixMilli(), 10) || init.EndUnixMS != strconv.FormatInt(clock.Wall().Add(operatorgates.SessionMaximum).UnixMilli(), 10) || init.ClockBinding != original.ClockBinding || !reflect.DeepEqual(init.Scenario, original.Scenario) || r.boundInit.ArmedAtUnixMS != original.ArmedAtUnixMS || r.boundInit.EndUnixMS != original.EndUnixMS {
		t.Fatal("MUTATION: rehearsal-fresh-arm: setup consumed the gate window or binding changed", init)
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

func TestPinnedSequencePreservesSetupTrustLoss(t *testing.T) {
	for _, uncertain := range []bool{false, true} {
		session := newTestSession(t)
		if uncertain {
			session.RecordUncertainty(operator.Uncertainty{Kind: "host.execute", Rule: "lost-reply"})
		} else {
			session.Halt("command-result-unknown")
		}
		r := &rehearsal{session: session}
		// No resource fields are supplied: a rejected setup must start no sequence.
		if err := r.runPinnedSequence(context.Background()); err == nil || err.Error() != "command-result-unknown" {
			t.Fatal("setup trust loss was reset", err)
		}
	}
}
