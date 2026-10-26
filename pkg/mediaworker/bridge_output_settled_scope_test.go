package mediaworker

import (
	"crypto/sha256"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Old-lineage evidence must not answer for a replacement lineage. Records keep
// their scope; do packed ranges?
func TestBridgeOutputPackedReceiptsRejectChangedScope(t *testing.T) {
	rig, scope := newBridgeRig(t, false)
	s := rig.s
	s.agent.v2.bridge.limits.AdmissionTimeout = time.Hour
	go func() {
		for {
			select {
			case <-s.ctx.Done():
				return
			case <-rig.wire.packets:
			}
		}
	}()
	for seq := uint64(1); seq <= 40; seq++ {
		r := bridgeRequest(scope, seq, time.Now())
		r.data = bridgeIdentityPacket(seq)
		r.digest = sha256.Sum256(r.data)
		require.Equal(t, "admitted", s.admitBridgePacket(r).Outcome)
		paceBridge(t, s)
	}
	s.mu.Lock()
	v := cloneBridgeState(s.agent.v2.bridge.state)
	v.Scope.Generation++
	v.Scope.LineageID = "replacement"
	s.agent.v2.bridge.state = v
	s.mu.Unlock()
	current := scope
	current.Generation++
	current.LineageID = "replacement"
	for _, seq := range []uint64{5, 40} { // 5 is packed, 40 is a recent full record
		st, err := s.worker.BridgeOutputStatus(current, seq)
		require.NoError(t, err)
		require.Equal(t, "unknown", st.Status)
		require.Nil(t, st.Receipt)
	}
}

func TestBridgeOutputPackedScopeCannotConflictWithFreshSequence(t *testing.T) {
	rig, scope := newBridgeRig(t, false)
	retainBridgeStream(t, rig.s, scope, 40, 160, rig.wire)
	s := rig.s
	s.mu.Lock()
	b := s.agent.v2.bridge
	v := cloneBridgeState(b.state)
	v.Scope.Generation++
	v.Scope.LineageID = "replacement"
	v.Through = 0
	v.Sent = 0
	v.Pruned = 0
	v.Turns[0].First = 0
	b.state = v
	s.mu.Unlock()
	current := BridgeOutputScope(v.Scope)
	for seq := uint64(1); seq <= 5; seq++ {
		r := bridgeRequest(current, seq, time.Now())
		r.data = bridgeIdentityPacket(seq + 100)
		r.digest = sha256.Sum256(r.data)
		got := s.admitBridgePacket(r)
		require.Equal(t, "admitted", got.Outcome, "old packed sequence %d must not conflict: %+v", seq, got)
	}
}

func TestBridgeOutputPackedChainBindsScope(t *testing.T) {
	rig, scope := newBridgeRig(t, false)
	retainBridgeStream(t, rig.s, scope, 40, 160, rig.wire)
	image := rig.s.exportBridgeLocked()
	for _, field := range []string{"session", "bot", "epoch", "generation", "lineage"} {
		t.Run(field, func(t *testing.T) {
			r := image.Settled[0]
			saved := cloneBridgeState(image.State)
			switch field {
			case "session":
				r.Scope.SessionID = "other"
			case "bot":
				r.Scope.BotInstance = "other"
			case "epoch":
				r.Scope.Epoch++
			case "generation":
				r.Scope.Generation++
			case "lineage":
				r.Scope.LineageID = "other"
			}
			saved.Scope = r.Scope
			_, err := validateBridgeSettled(&bridgeOutputMove{Version: bridgeOutputMoveVersion, Settled: []bridgeSettledRange{r}}, saved)
			require.Error(t, err, "matching replacement metadata cannot rebind the original chain")
		})
	}
}
