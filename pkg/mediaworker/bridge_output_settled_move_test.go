package mediaworker

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func bridgeIdentityPacket(seq uint64) []byte {
	p := bridgePacket(160)
	binary.BigEndian.PutUint64(p[1:9], seq)
	return p
}

// Compact ~300 sent identities, partially prune, exact-move, and check that the
// destination keeps per-identity duplicate/conflict evidence and status proof.
func TestBridgeOutputPackedPartialPruneExactMove(t *testing.T) {
	rig, scope := newBridgeRig(t, true)
	s := rig.s
	go func() {
		for {
			select {
			case <-s.ctx.Done():
				return
			case <-rig.wire.packets:
			}
		}
	}()
	const n = 300
	s.agent.v2.bridge.limits.AdmissionTimeout = time.Hour
	revisions := map[uint64]uint64{}
	for seq := uint64(1); seq <= n; seq++ {
		r := bridgeRequest(scope, seq, time.Now())
		r.data = bridgeIdentityPacket(seq)
		r.digest = sha256.Sum256(r.data)
		require.Equal(t, "admitted", s.admitBridgePacket(r).Outcome)
		paceBridge(t, s)
		s.mu.Lock()
		if rec, ok := s.agent.v2.bridge.records[seq]; ok {
			revisions[seq] = rec.Receipt.Revision
		}
		s.mu.Unlock()
	}
	s.mu.Lock()
	b := s.agent.v2.bridge
	t.Logf("before prune: records=%d ranges=%d sent=%d", len(b.records), len(b.settled), b.state.Sent)
	s.bridgeCheckpointedLocked(cloneAgentState(s.state.Agent), 3)
	s.mu.Unlock()
	require.NoError(t, s.worker.AdvanceBridgeReplayHorizon(scope, 100))
	srcStatus := map[uint64]BridgeOutputStatus{}
	for _, seq := range []uint64{50, 100, 101, 150, 256, 257, 284, 285, 299, 300} {
		st, err := s.worker.BridgeOutputStatus(scope, seq)
		require.NoError(t, err)
		srcStatus[seq] = st
	}
	s.mu.Lock()
	t.Logf("after prune: pruned=%d records=%d ranges=%d first=%d", b.state.Pruned, len(b.records), len(b.settled), b.settled[0].First)
	saved := cloneAgentState(s.state.Agent)
	image := s.exportBridgeLocked()
	s.mu.Unlock()
	target, _ := newBridgeRig(t, true)
	dst := target.s
	dst.agent.latest = cloneAgentState(saved)
	dst.state.Agent = cloneAgentState(saved)
	dst.agent.v2.bridge.state = nil
	require.NoError(t, dst.restoreBridgeOutput(&movePayload{Bridge: image}))
	require.NoError(t, dst.restorePlayback(&movePayload{Bridge: image}))
	for seq, want := range srcStatus {
		got, err := dst.worker.BridgeOutputStatus(scope, seq)
		require.NoError(t, err)
		require.Equal(t, want, got, "status for %d must survive the move", seq)
	}
	// Duplicate identical bytes: retained outcome, no replay. Conflicting bytes: conflict.
	for _, seq := range []uint64{150, 285, 299} {
		r, err := dst.worker.SubmitBridgeOutput(context.Background(), scope, 1, seq, bridgeIdentityPacket(seq))
		require.NoError(t, err)
		require.Equal(t, "sent", r.Outcome, "seq %d duplicate", seq)
		require.Equal(t, revisions[seq], r.Revision, "original receipt revision must survive packing and move")
		bad := bridgeIdentityPacket(seq)
		bad[20] ^= 1
		r, err = dst.worker.SubmitBridgeOutput(context.Background(), scope, 1, seq, bad)
		require.NoError(t, err)
		require.Equal(t, "conflict", r.Reason, "seq %d conflict", seq)
	}
	r, err := dst.worker.SubmitBridgeOutput(context.Background(), scope, 1, 50, bridgeIdentityPacket(50))
	require.NoError(t, err)
	t.Logf("pruned duplicate: outcome=%q status=%q reason=%q consumed=%v", r.Outcome, r.Status, r.Reason, r.Consumed)
	require.Empty(t, dst.agent.v2.playback.bridgeQueue, "no replay of settled identities")
	// The next fresh identity is admitted.
	r, err = dst.worker.SubmitBridgeOutput(context.Background(), scope, 1, n+1, bridgeIdentityPacket(n+1))
	require.NoError(t, err)
	require.Equal(t, "admitted", r.Outcome)
	// Tampering with one packed digest must be refused at restore.
	bad := *image
	bad.Settled = append([]bridgeSettledRange(nil), image.Settled...)
	data := append([]byte(nil), bad.Settled[0].Data...)
	data[5] ^= 1
	bad.Settled[0].Data = data
	other, _ := newBridgeRig(t, false)
	other.s.agent.latest = cloneAgentState(saved)
	other.s.state.Agent = cloneAgentState(saved)
	require.Error(t, other.s.restoreBridgeOutput(&movePayload{Bridge: &bad}))
}
