package relay

import (
	"github.com/stretchr/testify/require"
	"sync/atomic"
	"testing"
)

func TestSelfFenceStopsBothPacketPathsAndCounters(t *testing.T) {
	var permitted atomic.Bool
	permitted.Store(true)
	sys := startTestRelay(t, Config{ForwardingAllowed: permitted.Load, InstanceID: "lease-owner"})
	worker := sys.worker(t, sessionA)
	caller := newTestCaller(t)
	sys.connect(t, caller, worker, sessionA)
	caller.send(t, media, sys.relay.PublicAddr())
	worker.expect(t, caller.addr(), media)
	worker.sendTo(t, caller.addr(), dtlsReply, sys.relay.WorkerAddr())
	caller.expect(t, sys.relay.PublicAddr(), dtlsReply)
	before := sys.relay.Stats()
	permitted.Store(false)
	caller.send(t, media, sys.relay.PublicAddr())
	worker.expectNothing(t)
	worker.sendTo(t, caller.addr(), dtlsReply, sys.relay.WorkerAddr())
	caller.expectNothing(t)
	require.Equal(t, before.CallerPackets, sys.relay.Stats().CallerPackets)
	require.Equal(t, before.WorkerPackets, sys.relay.Stats().WorkerPackets)
	sys.relay.Fence()
	caller.send(t, media, sys.relay.PublicAddr())
	worker.expectNothing(t)
	worker.sendTo(t, caller.addr(), dtlsReply, sys.relay.WorkerAddr())
	caller.expectNothing(t)
	after := sys.relay.Stats()
	require.Equal(t, before.CallerPackets, after.CallerPackets)
	require.Equal(t, before.WorkerPackets, after.WorkerPackets)
	require.EqualValues(t, 1, after.SelfFences)
}
