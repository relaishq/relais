package mediaworker

import (
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/agenthost"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/stretchr/testify/require"
)

// The established DTLS fixture uses one owner tenure and an observed datagram
// sink. ResumeSession still owns registration, restore, memory admission,
// required checkpointing and adoption; this helper replaces only socket I/O.
func bridgeResumeWorker(cfg Config, owner netip.AddrPort) (*Worker, *dtlsWireConn) {
	wire := &dtlsWireConn{packets: make(chan []byte, 256)}
	return &Worker{cfg: cfg, conn: wire, localAddr: owner, sessions: map[string]*session{}, byAddr: map[netip.AddrPort]*session{}, byAddrConsent: map[netip.AddrPort]time.Time{}}, wire
}
func stopBridgeResumeWorker(w *Worker, id string) {
	if s := w.session(id); s != nil {
		s.fenced.Store(true)
		s.cancel()
		s.close()
	}
	w.running.Wait()
}
func TestBridgeOutputResumeReservesBridgeOnlyScratch(t *testing.T) {
	rig, scope := newBridgeRig(t, false)
	s := rig.s
	s.worker.cfg.Agent.CallbackTimeout = time.Hour
	s.worker.paused.Store(true)
	require.Equal(t, "admitted", s.admitBridgePacket(bridgeRequest(scope, 1, time.Now())).Outcome)
	// Source export uses a controlled clock in other tests. This test needs only
	// the real exact image, so capture the already accepted cut without pausing.
	image, err := s.saveV2MoveOnly(s.ctx)
	require.NoError(t, err)
	data, err := s.checkpoint.capture(checkpointExact, &movePayload{Bridge: image.bridge})
	require.NoError(t, err)
	var seen atomic.Int64
	cfg := s.worker.cfg
	cfg.SplitCheckpoints = true
	cfg.Relay.LeaseTTL = time.Hour
	target, _ := bridgeResumeWorker(cfg, s.worker.localAddr)
	target.paused.Store(true)
	t.Cleanup(func() { stopBridgeResumeWorker(target, s.id) })
	target.cfg.Agent.Factory = agenthost.Factory(agenthost.Registration{Name: "bridge", Factory: func(agent.FactoryArgs) agent.Agent {
		seen.Store(target.moveBytes.Load())
		return &acceptedEventAgent{}
	}})
	id, err := target.ResumeSession(data, ResumeOptions{Lease: s.lease})
	require.NoError(t, err)
	require.Equal(t, s.id, id)
	require.Equal(t, int64(bridgeRecordCharge+160), seen.Load(), "bridge-only move must reserve its restore copy before constructing the agent")
	dst := target.session(id)
	require.NotNil(t, dst.agent.v2.bridge)
	require.Empty(t, dst.agent.v2.tasks)
	require.Equal(t, int64(bridgeRecordCharge+160), dst.agent.v2.bridge.recordMemory.Load())
	dst.mu.Lock()
	require.Len(t, dst.agent.v2.playback.bridgeQueue, 1)
	dst.mu.Unlock()
}
