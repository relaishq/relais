package relay

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
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
	// The relay counts a packet after forwarding it, so wait for both counts:
	// binding and media from the caller, binding success and reply from the worker.
	require.Eventually(t, func() bool {
		stats := sys.relay.Stats()
		return stats.CallerPackets == 2 && stats.WorkerPackets == 2
	}, time.Second, time.Millisecond)
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
