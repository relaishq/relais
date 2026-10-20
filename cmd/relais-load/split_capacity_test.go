package main

import (
	"testing"
	"time"

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
	step := judgeSplitCapacity("relay", r, true)
	require.Equal(t, "pass", step.Verdict, step.Causes)
	require.Equal(t, "valid", step.GeneratorOutcome)
	require.Len(t, step.ToleratedSlips, 1)
	require.Equal(t, r, step.Run, "raw audio and strict saturation evidence must remain intact")
}
