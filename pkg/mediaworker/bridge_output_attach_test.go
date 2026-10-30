package mediaworker

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBridgeExactMoveReattachesSameBot(t *testing.T) {
	rig, scope := newBridgeRig(t, true)
	s := rig.s
	submitBridge(t, s, scope, 1, 1)
	s.mu.Lock()
	s.lease.Epoch = scope.Epoch + 1
	s.mu.Unlock()
	api, ok := any(s.worker).(interface {
		ReattachBridgeOutput(BridgeOutputScope, uint64) error
	})
	require.True(t, ok, "exact moves require authenticated epoch scope transfer")
	next := scope
	next.Epoch++
	require.NoError(t, api.ReattachBridgeOutput(next, 1))
	_, err := s.worker.BridgeOutputCredit(scope)
	require.Error(t, err)
	r, err := s.worker.BridgeOutputResume(next)
	require.NoError(t, err)
	require.EqualValues(t, 2, r.NextSequence)
	require.NoError(t, api.ReattachBridgeOutput(next, 1))
	bad := next
	bad.LineageID = "other"
	require.Error(t, api.ReattachBridgeOutput(bad, 1))
	require.Error(t, api.ReattachBridgeOutput(next, 0))
}

func TestBridgeEpochTransferPreservesPackedConflictEvidence(t *testing.T) {
	rig, scope := newBridgeRig(t, true)
	s := rig.s
	for seq := uint64(1); seq <= 8; seq++ {
		submitBridge(t, s, scope, 1, seq)
		paceBridge(t, s)
	}
	s.mu.Lock()
	frozen := s.exportBridgeLocked()
	before, _ := json.Marshal(frozen)
	require.NotEmpty(t, s.agent.v2.bridge.settled)
	s.lease.Epoch = scope.Epoch + 1
	s.mu.Unlock()
	next := scope
	next.Epoch++
	require.NoError(t, s.worker.ReattachBridgeOutput(next, 8))
	after, _ := json.Marshal(frozen)
	require.Equal(t, before, after, "scope transfer cannot mutate an exact frozen image")
	for _, seq := range []uint64{1, 8} {
		receipt, err := s.worker.SubmitBridgeOutput(context.Background(), next, 1, seq, bridgePacket(160))
		require.NoError(t, err)
		require.Equal(t, "sent", receipt.Outcome)
		bad := bridgePacket(160)
		bad[10] ^= 1
		receipt, err = s.worker.SubmitBridgeOutput(context.Background(), next, 1, seq, bad)
		require.NoError(t, err)
		require.Equal(t, "conflict", receipt.Reason)
	}
	s.mu.Lock()
	saved := cloneAgentState(s.state.Agent)
	image := s.exportBridgeLocked()
	s.mu.Unlock()
	dst, _ := newBridgeRig(t, true)
	dst.s.agent.latest = cloneAgentState(saved)
	dst.s.state.Agent = cloneAgentState(saved)
	dst.s.agent.v2.bridge.state = nil
	require.NoError(t, dst.s.restoreBridgeOutput(&movePayload{Bridge: image}))
}
