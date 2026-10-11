package main

import (
	"encoding/json"
	"fmt"
	"github.com/relais/internal/capacity"
	"github.com/relais/internal/loadgen"
	"github.com/relais/pkg/callharness"
	"github.com/stretchr/testify/require"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func capacityFixture() loadgen.RunReport {
	r := loadgen.RunReport{Callers: 2, Outcome: "valid", Yardstick: []loadgen.MetricRow{{Scenario: "move", Metric: "resume", Outcome: "pass"}}, Telemetry: capacity.Telemetry{Processes: []capacity.Process{{Name: "relay", RatesPerCallSecond: map[string]float64{"relais_relay_caller_packets_total": 80, "relais_relay_worker_packets_total": 80, `relais_transport_bytes_total{direction="out",transport="redis"}`: 1000, `relais_store_commands_total{store="session"}`: 40}}}}, Processes: []loadgen.ProcessReport{{Calls: []loadgen.Message{{Call: &callharness.Report{Moves: []callharness.MoveReport{{Kind: "move", Tracks: []callharness.MoveTrackReport{{Kind: "audio", PacketsAfter: 100, Gap: 40 * time.Millisecond, BaselineGapSamples: 50, BaselineGapP95: 20 * time.Millisecond}}}}}}}}}}
	r.Telemetry.Processes = append(r.Telemetry.Processes, capacity.Process{Name: "control", First: map[string]float64{"relais_control_takeovers_total": 0}, Last: map[string]float64{"relais_control_takeovers_total": 0}})
	return r
}
func TestCapacityRejectsSaturatedGeneratorEvenWithGreenCallerRows(t *testing.T) {
	r := capacityFixture()
	r.Outcome = "inconclusive"
	r.Causes = []string{"saturated interval"}
	step := judgeCapacity("worker", r, 100, 128<<10)
	require.Equal(t, "inconclusive", step.Verdict)
}
func TestCapacityRequiresAllServiceAndResourceEvidence(t *testing.T) {
	r := capacityFixture()
	require.Equal(t, "pass", judgeCapacity("worker", r, 100, 128<<10).Verdict)
	r.Telemetry.Errors = []string{"missing"}
	require.Equal(t, "inconclusive", judgeCapacity("worker", r, 100, 128<<10).Verdict)
	r = capacityFixture()
	r.Yardstick[0].Outcome = "fail"
	require.Equal(t, "fail", judgeCapacity("worker", r, 100, 128<<10).Verdict)
	r = capacityFixture()
	r.Processes[0].Calls[0].Call.Moves[0].Tracks[0].Gap = 200 * time.Millisecond
	require.Equal(t, "fail", judgeCapacity("worker", r, 100, 128<<10).Verdict)
	r = capacityFixture()
	r.Processes[0].Calls[0].Call.Moves[0].Tracks[0].BaselineGapSamples = 2
	require.Equal(t, "inconclusive", judgeCapacity("worker", r, 100, 128<<10).Verdict)
	require.Equal(t, "fail", judgeCapacity("worker", capacityFixture(), 20, 128<<10).Verdict)
}
func TestNPlusOneReservesMoveSlotAndRelayCapacity(t *testing.T) {
	require.Equal(t, 19, plannedLoad(3, 10, 100))
	require.Equal(t, 12, plannedLoad(3, 10, 12))
	require.Zero(t, plannedLoad(3, 0, 100))
	require.LessOrEqual(t, (plannedLoad(3, 10, 100)+1)/2, 10)
}
func TestCostUsesInputPricesAndMeasuredTransfer(t *testing.T) {
	p := priceInput{Currency: "test", Worker: machinePrice{Hourly: 2}, Relay: machinePrice{Hourly: 3}, Control: machinePrice{Hourly: 4}, Store: machinePrice{Hourly: 5}, PublicEgressPerGiB: 6}
	c := calculateCost(p, 3, 9, 1<<20)
	require.Equal(t, 2.0, c.MachinePerCallHour)
	require.Equal(t, 21.09375, c.TransferPerCallHour)
	require.Equal(t, 23.09375, c.TotalPerCallHour)
}

func TestPriceInputsRequireEveryExplicitPrice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prices.json")
	valid := `{"currency":"TEST","worker":{"hourly":0},"relay":{"hourly":2},"control":{"hourly":3},"store":{"hourly":4},"public_egress_per_gib":5}`
	require.NoError(t, os.WriteFile(path, []byte(valid), 0600))
	p, err := readPrices(path)
	require.NoError(t, err)
	require.Zero(t, p.Worker.Hourly)
	for _, invalid := range []string{strings.Replace(valid, `"worker":{"hourly":0}`, `"worker":{}`, 1), strings.Replace(valid, `"public_egress_per_gib":5`, `"extra":5`, 1), strings.Replace(valid, `"hourly":2`, `"hourly":-2`, 1), valid + ` {}`} {
		require.NoError(t, os.WriteFile(path, []byte(invalid), 0600))
		_, err := readPrices(path)
		require.Error(t, err)
	}
}

func TestCapacityRejectsUnexpectedTakeover(t *testing.T) {
	r := capacityFixture()
	r.Telemetry.Processes[1].Last["relais_control_takeovers_total"] = 1
	require.Equal(t, "fail", judgeCapacity("worker", r, 100, 128<<10).Verdict)
}

func TestCapacityRejectsWholeCallGapAwayFromScheduledEvents(t *testing.T) {
	r := capacityFixture()
	r.Processes[0].Calls[0].Call.Tracks = []callharness.TrackReport{{Kind: "audio", MediaGap: 400 * time.Millisecond, MediaGapEndedAt: 15 * time.Second}}
	require.Equal(t, "fail", judgeCapacity("worker", r, 100, 128<<10).Verdict)
}

func TestCapacityCallerStallMakesOutsideEventGapInconclusive(t *testing.T) {
	for _, arrivals := range []bool{false, true} {
		for _, tc := range []struct {
			name     string
			lateness time.Duration
			want     string
		}{
			{"below excess", 249 * time.Millisecond, "fail"},
			{"equal excess", 250 * time.Millisecond, "inconclusive"},
			{"above excess", 400 * time.Millisecond, "inconclusive"},
		} {
			t.Run(fmt.Sprintf("%s/arrivals=%t", tc.name, arrivals), func(t *testing.T) {
				r := capacityFixture()
				r.Processes[0].Lateness.Max = tc.lateness
				track := callharness.TrackReport{Kind: "audio", MediaGap: 400 * time.Millisecond, MediaGapEndedAt: 15 * time.Second}
				if arrivals {
					track.MediaGap = 0
					track.Arrivals = []time.Duration{14600 * time.Millisecond, 15 * time.Second}
				}
				r.Processes[0].Calls[0].Call.Tracks = []callharness.TrackReport{track}
				// A different process's stall cannot explain this call's gap.
				r.Processes = append(r.Processes, loadgen.ProcessReport{Lateness: loadgen.Lateness{Max: time.Second}})
				step := judgeCapacity("worker", r, 100, 128<<10)
				require.Equal(t, tc.want, step.Verdict)
				if tc.want == "inconclusive" {
					require.Contains(t, strings.Join(step.Causes, ";"), "generator maximum send lateness")
				}
			})
		}
	}
}

func TestCapacityTakeoverAllowsLaterVideoWithinTakeoverBound(t *testing.T) {
	at := time.Now()
	r := capacityFixture()
	r.Events = []loadgen.EventResult{{Event: loadgen.Event{Kind: "kill", Worker: 0}, At: at.Add(6 * time.Second), Sessions: []string{"affected"}, Affected: 1}}
	r.Telemetry.Processes[1].Last["relais_control_takeovers_total"] = 1
	r.Processes[0].Calls[0] = loadgen.Message{Session: "affected", Call: &callharness.Report{
		StartedAt: at,
		Moves: []callharness.MoveReport{{Kind: "takeover", From: 0, Start: 6 * time.Second, End: 6200 * time.Millisecond,
			Measurement: callharness.EventMeasurement{MediaResumedAt: 6150 * time.Millisecond},
			Tracks:      []callharness.MoveTrackReport{{Kind: "video", Gap: 800 * time.Millisecond, PacketsAfter: 20}}}},
		Tracks: []callharness.TrackReport{{Kind: "video", MediaGap: 800 * time.Millisecond, MediaGapEndedAt: 6900 * time.Millisecond}},
	}}
	require.Equal(t, "pass", judgeCapacity("n-plus-one", r, 100, 128<<10).Verdict)
	r.Processes[0].Calls[0].Call.Tracks[0].MediaGapEndedAt = 9 * time.Second
	require.Equal(t, "fail", judgeCapacity("n-plus-one", r, 100, 128<<10).Verdict)
}

func TestCapacityTakeoverCountMatchesScheduledAffectedCalls(t *testing.T) {
	r := capacityFixture()
	r.Events = []loadgen.EventResult{{Event: loadgen.Event{Kind: "kill"}, Affected: 2}}
	r.Telemetry.Processes[1].Last["relais_control_takeovers_total"] = 2
	require.Equal(t, "pass", judgeCapacity("n-plus-one", r, 100, 128<<10).Verdict)
	r.Telemetry.Processes[1].Last["relais_control_takeovers_total"] = 1
	require.Equal(t, "fail", judgeCapacity("n-plus-one", r, 100, 128<<10).Verdict)
	r.Telemetry.Processes[1].Last = nil
	require.Equal(t, "inconclusive", judgeCapacity("n-plus-one", r, 100, 128<<10).Verdict)
}

func TestCapacityScheduledGapIsSpecificToCallAndWindow(t *testing.T) {
	at := time.Now()
	report := &callharness.Report{StartedAt: at, Moves: []callharness.MoveReport{{Kind: "takeover", From: 0, Start: 6 * time.Second, End: 6500 * time.Millisecond}}}
	events := []loadgen.EventResult{{Event: loadgen.Event{Kind: "kill", Worker: 0}, At: at.Add(6 * time.Second), Sessions: []string{"affected"}}}
	require.False(t, gapOutsideScheduledEvent(events, "affected", report, 6400*time.Millisecond, 400*time.Millisecond))
	require.True(t, gapOutsideScheduledEvent(events, "survivor", report, 6400*time.Millisecond, 400*time.Millisecond))
	require.True(t, gapOutsideScheduledEvent(events, "affected", report, 15*time.Second, 400*time.Millisecond))
	// A larger scheduled gap must not mask a later excessive arrival gap.
	r := capacityFixture()
	r.Events = events
	r.Processes[0].Calls[0] = loadgen.Message{Session: "affected", Call: report}
	report.Tracks = []callharness.TrackReport{{Kind: "audio", MediaGap: 400 * time.Millisecond, MediaGapEndedAt: 6400 * time.Millisecond, Arrivals: []time.Duration{10 * time.Second, 10300 * time.Millisecond}}}
	require.Equal(t, "fail", judgeCapacity("worker", r, 100, 128<<10).Verdict)
}

func TestCapacityUnexpectedExitIsKnownFailure(t *testing.T) {
	r := capacityFixture()
	r.Telemetry.Processes[0].UnexpectedExit = true
	r.Telemetry.Processes[0].Errors = []string{"exit status 2"}
	r.Telemetry.Errors = []string{"process exited"}
	step := judgeCapacity("worker", r, 100, 128<<10)
	require.Equal(t, "fail", step.Verdict)
	require.Contains(t, strings.Join(step.Causes, ";"), "exit status 2")
}

func TestCapacityUnexpectedExitFailsWithInvalidGenerator(t *testing.T) {
	r := capacityFixture()
	r.Outcome = "inconclusive"
	r.Causes = []string{"saturated interval"}
	r.Processes[0].Saturated = true
	r.Telemetry.Processes[0].UnexpectedExit = true
	r.Telemetry.Processes[0].Errors = []string{"process exited with status 2"}
	r.Telemetry.Errors = []string{"process exited"}
	step := judgeCapacity("relay", r, 100, 128<<10)
	require.Equal(t, "fail", step.Verdict)
	require.Contains(t, strings.Join(step.Causes, ";"), "generator invalid: saturated interval")
	// A relay placement override also must not hide a confirmed crash.
	step.Run.WorkerPeaks = map[string]int{"worker-0": 9}
	dir := t.TempDir()
	step, err := recordCapacityStep(dir, filepath.Join(dir, "top.json"), &capacityReport{WorkerCapacityLowerBound: 8}, step)
	require.NoError(t, err)
	require.Equal(t, "fail", step.Verdict)
}

func TestPeakOverrideIsWrittenToBothReportsIncludingDrain(t *testing.T) {
	for _, kind := range []string{"n-plus-one", "drain"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			output := filepath.Join(dir, "top.json")
			result := capacityReport{WorkerCapacityLowerBound: 8}
			r := capacityFixture()
			r.WorkerPeaks = map[string]int{"worker-1": 9}
			step, err := recordCapacityStep(dir, output, &result, judgeCapacity(kind, r, 100, 128<<10))
			require.NoError(t, err)
			require.Equal(t, "fail", step.Verdict)
			var diskStep capacityStep
			var diskResult capacityReport
			data, err := os.ReadFile(filepath.Join(dir, "report.json"))
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(data, &diskStep))
			data, err = os.ReadFile(output)
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(data, &diskResult))
			require.Equal(t, diskStep, diskResult.Steps[0])
			require.Equal(t, "fail", diskStep.Verdict)
		})
	}
}

func TestCapacityCostReportsEveryMissingPrerequisite(t *testing.T) {
	prices := priceInput{Currency: "TEST", Worker: machinePrice{Hourly: 1}}
	result := capacityReport{Workers: 3, PlannedCallers: 8, PlannedValidated: true, Steps: []capacityStep{{Kind: "relay", Verdict: "pass", PublicEgressBytesPerCallSecond: 1000}}}
	setCapacityCost(&result, nil)
	require.Equal(t, "NOT-RUN(no input prices)", result.CostStatus)
	require.Nil(t, result.Cost)
	result.PlannedValidated = false
	setCapacityCost(&result, &prices)
	require.Equal(t, "NOT-RUN(planned load not validated)", result.CostStatus)
	require.Nil(t, result.Cost)
	result.Steps = nil
	setCapacityCost(&result, &prices)
	require.Equal(t, "NOT-RUN(no passing relay step)", result.CostStatus)
	result.PlannedValidated = true
	result.Steps = []capacityStep{{Kind: "relay", Verdict: "pass"}}
	setCapacityCost(&result, &prices)
	require.True(t, result.Cost.Validated)
	require.Contains(t, result.CostStatus, "VALIDATED(")
}

func TestCapacityRecordsRelayBufferConfiguration(t *testing.T) {
	for _, value := range []float64{0, 1} {
		r := capacityFixture()
		r.Telemetry.Processes[0].Last = map[string]float64{"relais_relay_buffer_enabled": value}
		step := judgeCapacity("worker", r, 100, 128<<10)
		require.NotNil(t, step.RelayBufferEnabled)
		require.Equal(t, value == 1, *step.RelayBufferEnabled)
	}
}

func TestCapacityBudgetCannotRelaxObjectiveOrUseNaN(t *testing.T) {
	c := config{Capacity: true, Callers: 2, Processes: 1, Workers: 3, GOMAXPROCS: 1, Duration: 20 * time.Second, SampleEvery: time.Second, Limits: loadgen.DefaultLimits(), CapacityStart: 2, CapacityMax: 2, CallersPerProcess: 2, StoreOpsBudget: 100, StoreBytesBudget: 128 << 10}
	require.NoError(t, c.validate())
	for _, value := range []float64{101, math.NaN(), math.Inf(1), 0} {
		bad := c
		bad.StoreOpsBudget = value
		require.Error(t, bad.validate())
	}
	for _, value := range []float64{129 << 10, math.NaN(), math.Inf(1), 0} {
		bad := c
		bad.StoreBytesBudget = value
		require.Error(t, bad.validate())
	}
	c.StoreOpsBudget = 90
	c.StoreBytesBudget = 120 << 10
	require.NoError(t, c.validate())
}
