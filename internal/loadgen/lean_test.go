package loadgen

import (
	"testing"

	"github.com/relaishq/relais/pkg/callharness"
	"github.com/stretchr/testify/require"
)

func TestLeanPoolPreservesDistributedFailuresAndInputs(t *testing.T) {
	id := callharness.OperationIdentity{RunID: "run", OperationID: "kill", SessionID: "session", Worker: "worker-generation", Epoch: 2}
	receipt := &callharness.DistributedEvent{Identity: id, Kind: "takeover"}
	call := &callharness.Report{Distributed: &callharness.DistributedRecording{RunID: "run", ClockError: "jump", Armed: []callharness.OperationIdentity{id}}, Moves: []callharness.MoveReport{{Identity: &id, Receipt: receipt, Kind: "takeover", Distributed: &callharness.DistributedMeasurement{WindowOutcome: "fail", Causes: []string{"server loss"}}}}}
	event := EventResult{Event: Event{Kind: "kill"}, Affected: 1, Sessions: []string{"session"}, Identities: []callharness.OperationIdentity{id}}
	r := RunReport{Callers: 1, Processes: []ProcessReport{{Complete: true, Calls: []Message{{Session: "session", Call: call}}}}, Events: []EventResult{event}}
	got := PoolDistributedReports([]RunReport{r})
	require.Equal(t, "fail", got.Distributed.Outcome)
	require.Equal(t, 0, r.Events[0].Recorded, "pooled judgment must not mutate source events")
	require.Same(t, receipt, got.Processes[0].Calls[0].Call.Moves[0].Receipt)
	require.Equal(t, "jump", got.Processes[0].Calls[0].Call.Distributed.ClockError)
	require.Equal(t, 1, got.Callers, "record count is for completeness, not claimed concurrent occupancy")
}
