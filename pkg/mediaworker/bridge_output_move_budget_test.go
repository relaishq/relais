package mediaworker

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/stretchr/testify/require"
)

// Drain-to-fit may consume the whole flush deadline, leaving no callback
// time for SaveMoveOnly after pause; a late fit then closes the actor.
func TestBridgeOutputLateFitPreservesSaveBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rig, _ := newBridgeRig(t, true)
		s := rig.s
		s.worker.cfg.Agent.CallbackTimeout = 100 * time.Millisecond
		app := &v2UnitAgent{state: agent.State{Version: 1, HasMoveOnly: true, MoveOnlyLength: 3}, payload: []byte("app"), moveCallback: func(ctx context.Context) error {
			select {
			case <-time.After(20 * time.Millisecond):
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}}
		s.agent.v2.instance = app
		_, err := (acceptedEvents{s}).submit(agent.Event{Kind: agent.Started}, eventLive)
		require.NoError(t, err)
		b := s.agent.v2.bridge
		b.reservedPackets.Add(1) // one ingress packet still in flight
		deadline := time.Now().Add(400 * time.Millisecond)
		cleared := make(chan struct{})
		go func() {
			defer close(cleared)
			time.Sleep(time.Until(deadline) - 5*time.Millisecond)
			s.mu.Lock()
			b.reservedPackets.Add(-1)
			b.signalChange()
			s.mu.Unlock()
		}()
		request := agentFlush{done: make(chan error, 1), deadline: deadline, moveOnly: make(chan agentMoveExport, 1)}
		s.finishAgentFlush(request)
		err = <-request.done
		t.Logf("flush err=%v actorFailed=%v", err, s.agent.failed.Load())
		require.False(t, s.agent.failed.Load(), "a late fit must refuse before pause, not close the actor")
		require.ErrorIs(t, err, ErrExportNotFlushed)
		require.False(t, s.agent.paused)
		require.NoError(t, s.ctx.Err())
		<-cleared
	})
}

func TestBridgeOutputRefusedMoveRestoresCredit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rig, scope := newBridgeRig(t, false)
		s := rig.s
		s.worker.cfg.Agent.CallbackTimeout = 10 * time.Millisecond
		startBridgeActor(t, s)
		synctest.Wait()
		b := s.agent.v2.bridge
		b.reservedPackets.Add(1)
		_, _, err := s.export()
		require.ErrorIs(t, err, ErrExportNotFlushed)
		synctest.Wait()
		require.False(t, s.agent.paused)
		require.False(t, s.worker.draining, "planned move must not mark worker shutdown")
		b.reservedPackets.Add(-1)
		b.signalChange()
		credit, err := s.worker.BridgeOutputCredit(scope)
		require.NoError(t, err)
		require.Positive(t, credit.PacketSlots)
		receipt, err := s.worker.SubmitBridgeOutput(context.Background(), scope, 1, 1, bridgePacket(160))
		require.NoError(t, err)
		require.Equal(t, "admitted", receipt.Outcome)
		require.False(t, s.agent.failed.Load())
	})
}
