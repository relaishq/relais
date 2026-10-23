package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/capacity"
	"github.com/relaishq/relais/internal/loadgen"
	legacy "github.com/relaishq/relais/pkg/agent"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/stretchr/testify/require"
)

func TestMatchedWorkerArguments(t *testing.T) {
	before := studyWorker("bin", "before", "steady")
	after := studyWorker("bin", "after", "steady")
	require.Equal(t, before.Binary, after.Binary)
	require.Equal(t, before.Agent, after.Agent)
	require.Equal(t, []string{"-agent-split-checkpoints"}, after.Args)
	require.Equal(t, []string{"-agent-max-snapshot-bytes=24576", "-agent-snapshot-bytes-per-second=270336", "-agent-snapshot-burst-bytes=98304"}, before.Args)
	require.Empty(t, studyWorker("bin", "before", "echo").Args)
	require.Equal(t, "history-churn/v1", studyWorker("bin", "after", "history-churn").Agent)
}
func TestSplitModeRejectsUnmatchedOrRelaxedInputs(t *testing.T) {
	for _, arg := range []string{"-capacity", "-video=false", "-store-ops-budget=101", "-soak", "-calibrate", "-workers=2", "-callers=17", "-topology=netns", "-duration=61s", "-duration=19s", "-cpu-limit=99", "-worker-arg=-agent-max-snapshot-bytes=20000"} {
		require.ErrorContains(t, runArgs([]string{"-split-capacity", arg}), "split-capacity needs")
	}
}

func TestSplitCustomKeepsAsyncSemanticsAfterGeneratorNormalization(t *testing.T) {
	samples := capacitySlipSamples()
	capacitySlip(&samples[0][15], 25*time.Millisecond, 25*time.Millisecond)
	r := capacitySlipRun(t, "move", samples)
	r.Processes[0].Calls[0].Call.Moves[0].Measurement.Audio.Verdict.Trusted = false
	r.Judge()
	r.Telemetry.Processes[0].RatesPerCallSecond[`relais_store_wire_commands_total{store="session"}`] = 40
	step := judgeSplitCapacity("relay", r, true, nil)
	require.Equal(t, "pass", step.Verdict, step.Causes)
	require.Equal(t, "valid", step.GeneratorOutcome)
	require.Len(t, step.ToleratedSlips, 1)
	require.Equal(t, r, step.Run, "raw audio and strict saturation evidence must remain intact")
}

func TestSplitChurnPhaseOverridesPassingWindow(t *testing.T) {
	r := capacitySlipRun(t, "move", capacitySlipSamples())
	r.Judge()
	r.Telemetry.Processes[0].RatesPerCallSecond[`relais_store_wire_commands_total{store="session"}`] = 40
	for _, field := range []string{"bytes", "commands", "missing", "short"} {
		t.Run(field, func(t *testing.T) {
			window := newWireWindow()
			window.Commands = int64(r.Callers) * 70
			window.RequestBytes = int64(r.Callers) * 100000
			o := &splitObservation{Scenario: "history-churn", Store: storeTrace{Phases: map[string]*wireWindow{"churn": window}, PhaseSeconds: map[string]float64{"churn": 1}}}
			// Keep enough observation time for the fixture's complete media window.
			r.Duration = 10 * time.Second
			switch field {
			case "bytes":
				window.RequestBytes = int64(r.Callers) * 131073
			case "commands":
				window.Commands = int64(r.Callers) * 101
			case "missing":
				delete(o.Store.Phases, "churn")
			case "short":
				r.Duration = 30 * time.Second
			}
			step := judgeSplitCapacity("relay", r, true, o)
			require.Less(t, step.StoreBytesPerCallSecond, float64(131072))
			require.Equal(t, "fail", step.Verdict)
			require.Contains(t, step.Causes[len(step.Causes)-1], "churn")
		})
	}
}

func TestStudyUsesFullSizeFixtureOnlyForHistoryProcesses(t *testing.T) {
	for _, arm := range []string{"before", "after"} {
		for _, scenario := range []string{"steady", "history-churn", "rotation", "n-plus-one", "drain"} {
			worker := studyWorker("bin", arm, scenario)
			require.Equal(t, []string{"RELAIS_HISTORY_FULL_SIZE=1"}, studyWorkerEnvironment(worker, true))
			require.Empty(t, studyWorkerEnvironment(worker, false))
		}
		require.Empty(t, studyWorkerEnvironment(studyWorker("bin", arm, "echo"), true))
	}
}

func TestWorkerBudgetRecordMatchesDefaultsAndOverrides(t *testing.T) {
	for _, tc := range []struct {
		args             []string
		cap, rate, burst int
	}{
		{nil, 7168, 81920, 28672},
		{[]string{"-agent-split-checkpoints"}, 6144, 70000, 40960},
		{[]string{"-agent-split-checkpoints", "-agent-max-snapshot-bytes=9000", "-agent-snapshot-bytes-per-second=120000", "-agent-snapshot-burst-bytes=60000"}, 9000, 120000, 60000},
		{studyWorker("bin", "before", "steady").Args, 24576, 270336, 98304},
	} {
		budgets, err := workerOperatorBudgets(tc.args)
		require.NoError(t, err)
		require.Equal(t, tc.cap, budgets["max_snapshot_bytes"])
		require.Equal(t, tc.rate, budgets["snapshot_bytes_per_second"])
		require.Equal(t, tc.burst, budgets["snapshot_burst_bytes"])
		require.Equal(t, 1048576, budgets["move_bytes_per_session"])
		require.Equal(t, 67108864, budgets["move_bytes_total"])
	}
	_, err := workerOperatorBudgets([]string{"-unknown-budget=1"})
	require.Error(t, err)
}

func TestFixtureSizeEvidenceUsesAcceptedAndRestoredBytes(t *testing.T) {
	r := loadgen.RunReport{Callers: 1, Duration: time.Second}
	r.Telemetry.Agents.Samples = []capacity.AgentSample{{Status: mediaworker.AgentSessionStatus{
		ID: "call", State: legacy.State{Bytes: make([]byte, 3072)}, HistoryBytes: 8192,
		App: json.RawMessage(`{"inputs":400,"restored_working_bytes":3072,"restored_history_bytes":8192}`),
	}}}
	observation := &splitObservation{}
	collectFixtureEvidence(r, observation)
	require.Equal(t, map[string]studyFixtureSize{"call": {Working: 3072, History: 8192, RestoredWorking: 3072, RestoredHistory: 8192}}, observation.FixtureSizes)
	// Configured limits cannot supply missing runtime/Restore witnesses.
	r.Telemetry.Agents.Samples[0].Status.App = json.RawMessage(`{"inputs":1}`)
	r.Telemetry.Agents.Samples[0].Status.State.Bytes = make([]byte, 2871)
	r.Telemetry.Agents.Samples[0].Status.HistoryBytes = 4205
	collectFixtureEvidence(r, observation)
	require.Equal(t, studyFixtureSize{Working: 2871, History: 4205}, observation.FixtureSizes["call"])
}

func TestUnknownWorkerFlagsKeepRunAndMarkBudgetEvidenceUnknown(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "custom-worker")
	require.NoError(t, os.WriteFile(binary, []byte("custom worker build"), 0600))
	c := config{}
	c.Worker.Binary = binary
	c.Worker.Args = []string{"-custom-delay=300ms"}
	build, err := customBuild(c)
	require.NoError(t, err, "unknown custom-worker flags must not refuse the run")
	require.NotEmpty(t, build.SHA256)
	require.Nil(t, build.OperatorBudgets)
	require.Contains(t, build.OperatorBudgetError, "custom-delay")
}
