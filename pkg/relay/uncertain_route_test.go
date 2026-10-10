package relay

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestZeroFromRepairsUncertainSessionRoute(t *testing.T) {
	sys := startTestRelay(t, Config{})
	a, b := sys.worker(t, sessionA), sys.worker(t, sessionB)
	c := sys.worker(t, sessionC)
	caller, other := newTestCaller(t), newTestCaller(t)
	sys.connect(t, caller, a, sessionA)
	sys.connect(t, other, c, sessionC)
	transferTestLease(t, sys.cfg.Owners, sessionA, b.addr())
	require.NoError(t, sys.relay.MoveSession(sessionA, netip.AddrPort{}, b.addr()))
	caller.send(t, media, sys.relay.PublicAddr())
	b.expect(t, caller.addr(), media)
	other.send(t, media, sys.relay.PublicAddr())
	c.expect(t, other.addr(), media)
	a.sendTo(t, caller.addr(), dtlsReply, sys.relay.WorkerAddr())
	caller.expectNothing(t)
	b.sendTo(t, caller.addr(), dtlsReply, sys.relay.WorkerAddr())
	caller.expect(t, sys.relay.PublicAddr(), dtlsReply)
	require.Equal(t, 2, sys.relay.Stats().Flows)
}
