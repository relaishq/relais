package mediaworker

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/stretchr/testify/require"
)

func TestBridgeOutputReceiptDisposition(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		change       func(*session, *bridgeOutputRequest)
	}{
		{"moving", "moving", func(s *session, _ *bridgeOutputRequest) { s.agent.paused = true }},
		{"unbound", "unbound", func(s *session, _ *bridgeOutputRequest) { s.lease.Epoch = 2 }},
		{"uncertain", "uncertain", func(s *session, _ *bridgeOutputRequest) { s.agent.v2.bridge.provenUnadmitted = false }},
		{"replaced", "stale_scope", func(_ *session, r *bridgeOutputRequest) { r.scope.Generation++ }},
		{"other_epoch", "unbound", func(_ *session, r *bridgeOutputRequest) { r.scope.Epoch++ }},
		{"order", "out_of_order", func(_ *session, r *bridgeOutputRequest) { r.seq = 2 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig, scope := newBridgeRig(t, false)
			r := bridgeRequest(scope, 1, time.Now())
			tc.change(rig.s, &r)
			got := rig.s.admitBridgePacket(r)
			require.Equal(t, tc.reason, got.Reason)
			data, err := json.Marshal(got)
			require.NoError(t, err)
			require.Contains(t, string(data), `"consumed":false`)
			require.Zero(t, rig.s.agent.v2.bridge.state.Through)
		})
	}
	rig, scope := newBridgeRig(t, false)
	late := rig.s.admitBridgePacket(bridgeRequest(scope, 1, time.Now().Add(-time.Second)))
	data, err := json.Marshal(late)
	require.NoError(t, err)
	require.Contains(t, string(data), `"consumed":true`)
	rig.s.agent.v2.bridge.limits.AudioBytes = 160
	require.Equal(t, "admitted", rig.s.admitBridgePacket(bridgeRequest(scope, 2, time.Now())).Outcome)
	pressure := rig.s.admitBridgePacket(bridgeRequest(scope, 3, time.Now()))
	data, err = json.Marshal(pressure)
	require.NoError(t, err)
	require.Contains(t, string(data), `"consumed":true`)
}
func TestBridgeOutputInterruptionPreservesFinishedOutcome(t *testing.T) {
	rig, scope := newBridgeRig(t, true)
	s := rig.s
	submitBridge(t, s, scope, 1, 1)
	require.NoError(t, s.worker.EndBridgeOutput(scope, 1, 1))
	paceBridge(t, s)
	require.NoError(t, s.deliverClockEvent())
	before, err := s.worker.BridgePlaybackOutcome(scope, 1)
	require.NoError(t, err)
	_, err = s.worker.InterruptBridgeOutput(context.Background(), scope, "next", 2, true)
	require.NoError(t, err)
	after, err := s.worker.BridgePlaybackOutcome(scope, 1)
	require.NoError(t, err)
	require.Equal(t, before, after)
}
func TestBridgeOutputBeginWhilePacketsPending(t *testing.T) {
	rig, scope := newBridgeRig(t, false)
	b := rig.s.agent.v2.bridge
	b.reservedPackets.Store(1)
	defer b.reservedPackets.Store(0)
	stream, err := rig.s.worker.BeginBridgeOutput(scope, agent.Turn(2))
	require.NoError(t, err)
	require.EqualValues(t, 2, stream)
	first := rig.s.admitBridgePacket(bridgeRequest(scope, 1, time.Now()))
	require.Equal(t, "admitted", first.Outcome)
	next := bridgeRequest(scope, 2, time.Now())
	next.turn = 2
	require.Equal(t, "admitted", rig.s.admitBridgePacket(next).Outcome)
	require.EqualValues(t, 2, b.state.Turns[1].First, "turn start binds to its first packet, not an older pending packet")
}

func TestBridgeOutputInterruptionWhileActorAndPacketLaneBusy(t *testing.T) {
	rig, scope := newBridgeRig(t, false)
	s := rig.s
	b := s.agent.v2.bridge
	entered, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	rig.app.handle = func(context.Context, agent.Event) (agent.Effects, error) {
		close(entered)
		<-release
		return agent.Effects{}, nil
	}
	actor := make(chan error, 1)
	go func() {
		_, err := (acceptedEvents{s}).submit(agent.Event{Kind: agent.Started}, eventLive)
		actor <- err
	}()
	<-entered
	replies := make(chan error, b.limits.PendingPackets)
	for i := 1; i <= b.limits.PendingPackets; i++ {
		seq := uint64(i)
		go func() {
			_, err := s.worker.SubmitBridgeOutput(context.Background(), scope, 1, seq, bridgePacket(160))
			replies <- err
		}()
		require.Eventually(t, func() bool { return len(b.ingress) == i }, time.Second, time.Millisecond)
	}
	ready := time.Now()
	request := bridgePriorityRequest{scope: scope, requestID: "cancel", floor: 2, cancel: true, clear: true, done: make(chan bridgePriorityReply, 1)}
	b.priority <- request
	startBridgeLoop(t, s)
	reply := <-request.done
	elapsed := time.Since(ready)
	// The CI assertion is a scheduling margin, not a changed 50 ms target.
	require.Less(t, elapsed, 250*time.Millisecond)
	require.NoError(t, reply.err)
	require.EqualValues(t, 2, reply.receipt.CancelFloor)
	t.Logf("request enqueue to observed cancellation=%s target=50ms CI margin=250ms", elapsed)
	for range b.limits.PendingPackets {
		require.NoError(t, <-replies)
	}
	close(release)
	require.NoError(t, <-actor)
	require.False(t, s.agent.failed.Load())
}

func TestBridgeOutputPrunedIdentityIsSettled(t *testing.T) {
	rig, scope := newBridgeRig(t, true)
	s := rig.s
	submitBridge(t, s, scope, 1, 1)
	paceBridge(t, s)
	require.NoError(t, s.worker.AdvanceBridgeReplayHorizon(scope, 1))
	s.mu.Lock()
	s.bridgeCheckpointedLocked(cloneAgentState(s.state.Agent), 1)
	s.mu.Unlock()
	receipt := submitBridge(t, s, scope, 1, 1)
	require.Equal(t, "settled", receipt.Status)
	require.Empty(t, receipt.Outcome)
	require.True(t, receipt.Consumed)
	require.Empty(t, s.agent.v2.playback.bridgeQueue)
}
func TestBridgeOutputPartialFinalFramePlayback(t *testing.T) {
	rig, scope := newBridgeRig(t, true)
	s := rig.s
	submitBridge(t, s, scope, 1, 1)
	tail := bridgePacket(160)
	tail[0] = 0xf0
	receipt, err := s.worker.SubmitBridgeOutput(context.Background(), scope, 1, 2, tail)
	require.NoError(t, err)
	require.Equal(t, "admitted", receipt.Outcome)
	require.Equal(t, 30*time.Millisecond, s.agent.v2.playback.duration)
	require.NoError(t, s.worker.EndBridgeOutput(scope, 1, 2))
	paceBridge(t, s)
	timestamp := s.agent.v2.playback.nextTS
	paceBridge(t, s)
	require.Equal(t, timestamp+480, s.agent.v2.playback.nextTS)
	require.Zero(t, s.agent.v2.playback.duration)
	require.NoError(t, s.deliverClockEvent())
	outcome, err := s.worker.BridgePlaybackOutcome(scope, 1)
	require.NoError(t, err)
	require.Equal(t, "complete", outcome.Outcome)
}

func TestBridgeOutputMemoryRefusalPreservesIdentity(t *testing.T) {
	rig, scope := newBridgeRig(t, true)
	s := rig.s
	b := s.agent.v2.bridge
	b.limits.ActorReserveBytes = (1 << 20) - 512
	before := b.state.Revision
	receipt := submitBridge(t, s, scope, 1, 1)
	require.Equal(t, "backpressure", receipt.Reason)
	require.False(t, receipt.Consumed)
	require.Equal(t, before, receipt.Revision, "an uncommitted candidate cannot advertise a receipt revision")
	status, err := s.worker.BridgeOutputStatus(scope, 1)
	require.NoError(t, err)
	require.Equal(t, "unadmitted", status.Status)
	b.limits.ActorReserveBytes = BridgeOutputLimits{}.Defaults().ActorReserveBytes
	require.Equal(t, "admitted", submitBridge(t, s, scope, 1, 1).Outcome)
}

func TestBridgeOutputClosedTurnConsumesIdentity(t *testing.T) {
	for _, turn := range []agent.Turn{1, 99} {
		t.Run(fmt.Sprint(turn), func(t *testing.T) {
			rig, scope := newBridgeRig(t, true)
			s := rig.s
			require.NoError(t, s.worker.EndBridgeOutput(scope, 1, 0))
			require.NoError(t, s.deliverClockEvent())
			packet := bridgeRequest(scope, 1, time.Now())
			packet.turn = turn
			receipt := s.admitBridgePacket(packet)
			require.Equal(t, "closed_turn", receipt.Reason)
			require.True(t, receipt.Consumed)
			require.EqualValues(t, 1, s.agent.v2.bridge.state.Through)
			require.Equal(t, receipt, s.admitBridgePacket(packet), "retry returns the settled terminal receipt")
			_, err := s.worker.BeginBridgeOutput(scope, 2)
			require.NoError(t, err)
			next := bridgeRequest(scope, 2, time.Now())
			next.turn = 2
			require.Equal(t, "admitted", s.admitBridgePacket(next).Outcome)
			require.Len(t, s.agent.v2.playback.bridgeQueue, 1, "closed turn cannot add media to a finished turn")
			image := s.exportBridgeLocked()
			target, _ := newBridgeRig(t, false)
			target.s.state.Agent = cloneAgentState(s.state.Agent)
			target.s.agent.latest = cloneAgentState(s.state.Agent)
			require.NoError(t, target.s.restoreBridgeOutput(&movePayload{Bridge: image}))
		})
	}
}
