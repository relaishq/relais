package mediaworker

import (
	"context"
	"net"
	"testing"
	"testing/synctest"
	"time"

	"github.com/pion/srtp/v3"
	legacy "github.com/relaishq/relais/pkg/agent"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/stretchr/testify/require"
)

// This uses the production pacer with a controlled store clock, not wall sleeps.
func TestBridgePacerLateWakesPreserveQueuedPackets(t *testing.T) {
	s, _, result := audioUnitSession(t, 150)
	installUnitPlayback(t, s, result)
	profile := srtp.ProtectionProfileAeadAes128Gcm
	keys := testSessionKeys(t, profile)
	s.srtpOut = testContext(t, keys.LocalMasterKey, keys.LocalMasterSalt, profile)
	sink := listenTestUDP(t)
	s.state.ICE.RemoteAddr = sink.LocalAddr().(*net.UDPAddr).AddrPort()
	base := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	s.agent.v2.playback.nextAt, s.agent.v2.playback.initialized = base, true
	synctest.Test(t, func(t *testing.T) {
		for i := 1; i <= 50; i++ {
			s.storeClock, s.storeClockAt = base.Add(time.Duration(i)*40*time.Millisecond), time.Now()
			delay, err := s.paceOne()
			require.NoError(t, err)
			require.Equal(t, 20*time.Millisecond, delay)
		}
	})
	require.Len(t, s.agent.v2.playback.queue, 100, "late wakes send one packet, not a catch-up burst")
	require.Equal(t, 2*time.Second, s.agent.v2.playback.duration, "these retained bytes are not lost")
	require.GreaterOrEqual(t, bridgePlaybackDrainBudget(146*20*time.Millisecond), 146*20*time.Millisecond, "two seconds cannot drain 146 twenty-ms packets even with no contention")
}

// The production timeout is shared by Handle, Save and host admission. The
// test chooses 20 ms and releases the host lock on context expiry, so it does
// not rely on a callback completing inside a 100 ms wall-time race assertion.
type bridgeAdmissionBlockedAfterSave struct {
	*bridgeThroughputAgent
	session      *session
	armed        bool
	saveReturned chan struct{}
}

func (a *bridgeAdmissionBlockedAfterSave) Save(ctx context.Context) (agent.State, error) {
	state, err := a.bridgeThroughputAgent.Save(ctx)
	if !a.armed {
		return state, err
	}
	a.armed = false
	locked := make(chan struct{})
	go func() {
		a.session.mu.Lock()
		close(locked)
		<-ctx.Done()
		a.session.mu.Unlock()
	}()
	<-locked
	close(a.saveReturned)
	return state, err
}
func TestBridgeStreamingHostAdmissionSharesCallbackDeadline(t *testing.T) {
	s, a, run := bridgeOutputRig(t)
	blocked := &bridgeAdmissionBlockedAfterSave{bridgeThroughputAgent: a, session: s, armed: true, saveReturned: make(chan struct{})}
	s.agent.v2.instance = blocked
	s.worker.cfg.Agent.CallbackTimeout = 20 * time.Millisecond
	result := agent.TaskResult{TaskID: run.ID(), Sequence: 1, Turn: 1, Outcome: agent.OK, Chunk: agent.Chunk{Audio: &agent.EncodedAudio{Format: "audio/opus", Packets: [][]byte{{0xf8, 0, 0, 0, 1}}}}}
	err := s.acceptPrototypeOutput(prototypeOutputRequest{ctx: s.ctx, result: result})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.ErrorIs(t, err, legacy.ErrSave)
	select {
	case <-blocked.saveReturned:
	default:
		t.Fatal("Save did not return before blocked host admission")
	}
	require.Empty(t, s.agent.v2.playback.queue, "deadline rejection must not admit audio")
}

func bridgePlaybackDrainBudget(queued time.Duration) time.Duration {
	return max(2*time.Second, 2*queued+time.Second)
}
