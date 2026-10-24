package main

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/callharness"
	"github.com/stretchr/testify/require"
)

func leanPricedFixture(t *testing.T) (leanManifest, leanReport) {
	t.Helper()
	m := leanFixture(t)
	r := judgeLean(m)
	p := &leanPrices{Currency: "TEST", Region: m.Region, Date: "2026-10-23", Source: "synthetic tariff; no real prices"}
	for _, category := range leanCostCategories {
		q, h := 1.0, 1.0
		if category == "workers" || category == "store_nodes" {
			h = 2
		}
		if category == "public_egress" {
			q = r.Cost.PublicGiB
			h = r.Phases[0].PublicBytesPerCallSecond * 3600 / (1 << 30)
		}
		p.Lines = append(p.Lines, leanPriceLine{Category: category, Resource: category, Meter: "test-unit", Quantity: leanPtr(q), Rate: leanPtr(2.0), SteadyQuantityPerHour: leanPtr(h)})
	}
	m.Prices = p
	r = judgeLean(m)
	return m, r
}
func TestLeanAllocatedCost(t *testing.T) {
	m, r := leanPricedFixture(t)
	require.Equal(t, "pass", r.Outcome, leanCauses(r))
	require.Empty(t, r.Cost.Unknown)
	require.Contains(t, r.Cost.Status, "VALIDATED")
	// All 32 recovery phases contain one call, the boundary contains four.
	require.InDelta(t, 36*(30.0-.001)/3600, r.Cost.CallHours, 1e-10)
	want := 26 + 2*r.Cost.PublicGiB
	require.InDelta(t, want, *r.Cost.Total, 1e-9)
	require.InDelta(t, want/r.Cost.CallHours, *r.Cost.PerCallHour, 1e-9)
	m.Prices.Lines[0].Rate = leanPtr(0.0)
	m.Prices.Lines[0].ZeroReason = "explicit synthetic free allocation"
	require.Contains(t, judgeLean(m).Cost.Status, "VALIDATED")
	for _, tc := range []struct {
		name, reason string
		change       func(*leanManifest)
	}{
		{"missing price", "missing", func(m *leanManifest) { m.Prices.Lines[0].Rate = nil }},
		{"missing ledger category", "disk", func(m *leanManifest) { m.Prices.Lines = append(m.Prices.Lines[:9], m.Prices.Lines[10:]...) }},
		{"duplicate host", "duplicate", func(m *leanManifest) { m.Prices.Lines = append(m.Prices.Lines, m.Prices.Lines[2]) }},
		{"wire versus billable egress", "egress quantity", func(m *leanManifest) { m.Prices.Lines[5].Quantity = leanPtr(999.0) }},
		{"negative", "negative", func(m *leanManifest) { m.Prices.Lines[0].Rate = leanPtr(-1.0) }},
		{"nonfinite", "nonfinite", func(m *leanManifest) { m.Prices.Lines[0].Rate = leanPtr(math.NaN()) }},
		{"undated", "date", func(m *leanManifest) { m.Prices.Date = "" }},
		{"no zero reason", "zero", func(m *leanManifest) { m.Prices.Lines[0].Rate = leanPtr(0.0) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := leanPricedFixture(t)
			tc.change(&m)
			r := judgeLean(m)
			require.Equal(t, "NOT-RUN", r.Cost.Status)
			require.Nil(t, r.Cost.Total)
			require.Contains(t, strings.Join(r.Cost.Unknown, ";"), tc.reason)
			require.Equal(t, "pass", r.Outcome, leanCauses(r))
		})
	}
}
func TestLeanRejectsScalarAndAmbiguousEvidence(t *testing.T) {
	m := leanFixture(t)
	// Large meaningless scalar estimates must not replace interval evidence.
	c := m.Phases[0].Run.Processes[0].Calls[0].Call
	c.Moves[0].Start = -time.Hour
	c.Moves[0].End = time.Hour
	c.Moves[0].Tracks = []callharness.MoveTrackReport{{Kind: "audio", Gap: time.Hour}}
	r := judgeLean(m)
	require.Equal(t, "pass", r.Outcome, leanCauses(r))
	// The same report becomes inconclusive when the interval straddles 2s.
	c.Moves[0].Distributed.Gaps[0].EventGap = callharness.DurationInterval{Lower: 1900 * time.Millisecond, Upper: 2100 * time.Millisecond}
	r = judgeLean(m)
	require.Equal(t, "inconclusive", r.Outcome, leanCauses(r))
	c.Moves[0].Receipt.Result.Lost = true
	r = judgeLean(m)
	require.Equal(t, "fail", r.Outcome, leanCauses(r))
}
func TestLeanRejectsMalformedManifest(t *testing.T) {
	for _, change := range []func(*leanManifest){
		func(m *leanManifest) { m.Clock = nil }, func(m *leanManifest) { m.Version = 0 }, func(m *leanManifest) { m.StoreTLS = false }, func(m *leanManifest) { m.Signaling = "https://secret@example.test/calls" }, func(m *leanManifest) { m.Phases[0].Run.Limits.CPUPercent = 99 }, func(m *leanManifest) { m.Phases[0].Kind = "ramp" }, func(m *leanManifest) { m.Phases[0].CallerIDs = []string{"unknown"} }, func(m *leanManifest) { m.Phases[0].WindowEvidence = "" },
	} {
		m := leanFixture(t)
		change(&m)
		require.Error(t, validateLeanManifest(m))
		require.NotEqual(t, "pass", judgeLean(m).Outcome)
	}
}

func TestLeanMeasuredClockInputsAndSamplingJitter(t *testing.T) {
	var clock leanClock
	require.NoError(t, json.Unmarshal([]byte(`{"frontend_control_bound_ns":0,"drift_ppm":0,"independent_measurements":"measured"}`), &clock))
	require.Negative(t, clock.WorkerBound, "omission must not silently mean perfect agreement")
	require.Error(t, json.Unmarshal([]byte(`{"unknown":0}`), &clock))
	m := leanFixture(t)
	for phase := range m.Phases {
		for process := range m.Phases[phase].Run.Processes {
			samples := m.Phases[phase].Run.Processes[process].Samples
			for i := range samples {
				samples[i].At = samples[i].At.Add(time.Millisecond)
				if i == 0 {
					samples[i].Interval += time.Millisecond
				}
			}
		}
	}
	r := judgeLean(m)
	require.Equal(t, "pass", r.Outcome, leanCauses(r))
}
func TestLeanClockAlignmentAndIndependentFailure(t *testing.T) {
	m := leanFixture(t)
	// A wide supplied alignment bound must conservatively increase the budget
	// rate, rather than turning a nominally passing average into approval.
	m.Phases[0].WindowError = 10 * time.Second
	p := &m.Phases[0].Run.Telemetry.Processes[3]
	for key := range p.Last {
		if strings.HasPrefix(key, "relais_store_commands_total") {
			p.Last[key] = 1500
		}
	}
	r := judgeLean(m)
	require.Equal(t, "inconclusive", r.Outcome, leanCauses(r))
	require.Contains(t, leanCauses(r), "budget not proven across alignment bounds")
	require.Less(t, r.Phases[0].CommandsLowerPerCallSecond, 100.0)
	require.Greater(t, r.Phases[0].CommandsPerCallSecond, 100.0)
	m = leanFixture(t)
	m.Phases[0].Run.Processes[0].Samples[0].CPUPerGOMAXPROCS = 95
	m.Phases[0].Run.Telemetry.Processes[0].UnexpectedExit = true
	r = judgeLean(m)
	require.Equal(t, "fail", r.Outcome, leanCauses(r))
}

func TestLeanMissingWindowStillWritesAnInconclusiveReport(t *testing.T) {
	m := leanFixture(t)
	m.Phases[0].Run.Telemetry.WindowSeconds = 0
	r := judgeLean(m)
	require.Equal(t, "inconclusive", r.Outcome, leanCauses(r))
	_, err := json.Marshal(r)
	require.NoError(t, err, "missing telemetry must not create an infinite derived rate")
	require.Contains(t, leanCauses(r), "telemetry window")
}

func TestLeanSerialPhasesNeedAClockBoundedCompletion(t *testing.T) {
	for _, change := range []func(*leanManifest){
		func(m *leanManifest) { m.Phases[0].CompletedAt = m.Phases[0].Run.StartedAt.Add(29 * time.Second) },
		func(m *leanManifest) { m.Phases[1].Before.At = m.Phases[0].CompletedAt.Add(-time.Second) },
		func(m *leanManifest) { m.Phases[0].Before.At = m.Phases[0].Run.StartedAt },
	} {
		m := leanFixture(t)
		change(&m)
		r := judgeLean(m)
		require.Equal(t, "inconclusive", r.Outcome, leanCauses(r))
	}
	m := leanFixture(t)
	m.Phases[0].Run.Processes = append(m.Phases[0].Run.Processes, m.Phases[0].Run.Processes[0])
	m.Phases[0].CallerIDs = append(m.Phases[0].CallerIDs, "1")
	require.Error(t, validateLeanManifest(m), "one phase cannot silently join raw clocks from different runners")
}

func TestLeanFaultTrialsKeepCallersLiveThroughRestore(t *testing.T) {
	plan := leanPlan()
	require.Equal(t, 120, plan.FaultTrialSeconds)
	require.Equal(t, plan.ExpectedSeconds, plan.Moves*plan.TrialSeconds+(plan.Takeovers+plan.Drains)*plan.FaultTrialSeconds+plan.AdmissionSeconds+plan.SetupCleanupSeconds)
	m := leanFixture(t)
	r := judgeLean(m)
	require.Equal(t, 2400, r.Schedule.ExpectedSeconds)
	// A claimed restoration after the caller hung up is never a live-call pass.
	m.Phases[20].Restored.At = m.Phases[20].Run.StartedAt.Add(30500 * time.Millisecond)
	r = judgeLean(m)
	require.Equal(t, "inconclusive", r.Outcome, leanCauses(r))
	require.Contains(t, leanCauses(r), "restore not proven before caller ended")
}

func TestLeanActualEventClockBoundsRetainWarmupAndTail(t *testing.T) {
	for _, which := range []string{"warmup", "tail"} {
		t.Run(which, func(t *testing.T) {
			m := leanFixture(t)
			p := &m.Phases[0]
			c := p.Run.Processes[0].Calls[0].Call
			move := &c.Moves[0]
			tracker, err := callharness.NewClockTracker(0)
			require.NoError(t, err)
			for _, probe := range c.Distributed.Probes {
				require.NoError(t, tracker.Add(probe))
			}
			if which == "warmup" {
				move.Receipt.Start = p.Run.StartedAt.Add(2 * time.Second)
				move.Receipt.Result.Start = move.Receipt.Start
				move.Distributed.Start, err = tracker.Map(move.Receipt.Start, c.Distributed.ControlOffset)
			} else {
				move.Receipt.End = p.Run.StartedAt.Add(25 * time.Second)
				move.Receipt.Result.End = move.Receipt.End
				move.Distributed.End, err = tracker.Map(move.Receipt.End, c.Distributed.ControlOffset)
				p.Timings[0].ReceiptSentAt = p.Run.StartedAt.Add(26 * time.Second)
				p.After.At = p.Run.StartedAt.Add(27 * time.Second)
			}
			require.NoError(t, err)
			r := judgeLean(m)
			require.Equal(t, "inconclusive", r.Outcome, leanCauses(r))
			require.Contains(t, leanCauses(r), "actual distributed event lacks warmup or recovery tail")
		})
	}
}

func TestLeanEgressAllocationPreservesObservedBytesAcrossWindowBounds(t *testing.T) {
	m := leanFixture(t)
	before := judgeLean(m)
	m.Phases[0].WindowError = time.Second
	m.Phases[0].Run.Telemetry.WindowSeconds = 31
	after := judgeLean(m)
	require.Equal(t, "pass", after.Outcome, leanCauses(after))
	require.InDelta(t, before.Cost.PublicGiB, after.Cost.PublicGiB, 1e-12, "an aligned observation window must not discard observed billable bytes")
}

func TestLeanFixedNamesKeepReplacementGenerationsSeparate(t *testing.T) {
	m := leanFixture(t)
	require.Equal(t, m.Phases[20].Before.Workers[0].Name, m.Phases[20].Restored.Workers[0].Name)
	require.NotEqual(t, m.Phases[20].Before.Workers[0].Generation, m.Phases[20].Restored.Workers[0].Generation)
	r := judgeLean(m)
	require.Equal(t, "pass", r.Outcome, leanCauses(r))
	m.Phases[20].Restored.Workers[0].Generation = m.Phases[20].Before.Workers[0].Generation
	r = judgeLean(m)
	require.Equal(t, "inconclusive", r.Outcome)
	require.Contains(t, leanCauses(r), "generation was not replaced")
	m = leanFixture(t)
	m.Phases[1].Before.Workers[0].Sessions = m.Phases[0].Before.Workers[0].Sessions
	r = judgeLean(m)
	require.NotEqual(t, "pass", r.Outcome)
	require.Contains(t, leanCauses(r), "require fresh session identities")
}
