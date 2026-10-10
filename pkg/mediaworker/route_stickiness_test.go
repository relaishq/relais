package mediaworker

import (
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The test owns A's socket so it can send B's valid credentials from A's
// exact address. On the wire this is the source-spoofing attack in #20.
func TestAuthenticatedSpoofCannotMoveSharedSocketRoute(t *testing.T) {
	for _, sameWorker := range []bool{false, true} {
		t.Run(map[bool]string{false: "different workers", true: "same worker"}[sameWorker], func(t *testing.T) {
			_, a, b := newTestSocket(t, 10*time.Second)
			if sameWorker {
				b = a
			}
			victim := dialTestSessionOn(t, a).endpoint(t)
			require.True(t, victim.check(t, true))
			attackerCall := dialTestSessionOn(t, b)
			attacker := attackerCall.endpoint(t)
			require.True(t, attacker.check(t, true), "positive control: B has valid credentials")
			spoof := &testEndpoint{call: attackerCall, conn: victim.conn}
			require.False(t, spoof.check(t, true), "B cannot nominate A's active address")
			require.Equal(t, "still A", sendAndReceive(t, a.conn.(*socketPort), victim, "still A"))
			require.Equal(t, victim.call.id, a.sessionAt(victim.conn.LocalAddr().(*net.UDPAddr).AddrPort()).id)
			require.True(t, victim.check(t, false), "incumbent consent still works")
			require.True(t, attacker.check(t, false), "B's own address still works")
		})
	}
}

func TestSharedSocketQuietAddressCanMove(t *testing.T) {
	for _, hangup := range []bool{false, true} {
		t.Run(map[bool]string{false: "consent goes quiet", true: "hangup"}[hangup], func(t *testing.T) {
			socket, a, b := newTestSocket(t, 10*time.Second)
			victim := dialTestSessionOn(t, a).endpoint(t)
			require.True(t, victim.check(t, true))
			addr := victim.conn.LocalAddr().(*net.UDPAddr).AddrPort()
			if hangup {
				require.NoError(t, socket.EndSession(victim.call.id))
				socket.mu.RLock()
				_, retained := socket.flowConsent[addr]
				socket.mu.RUnlock()
				require.False(t, retained, "hangup prunes consent evidence")
			} else {
				// Advance only the address's authenticated activity clock. The
				// worker's session is still live; media must not renew stickiness.
				socket.mu.Lock()
				socket.flowConsent[addr] = time.Now().Add(-socket.stickinessWindow)
				socket.mu.Unlock()
				_, err := victim.conn.WriteToUDPAddrPort([]byte("unauthenticated media"), socket.LocalAddr())
				require.NoError(t, err)
				require.NoError(t, a.conn.(*socketPort).drain(time.Second))
			}
			attackerCall := dialTestSessionOn(t, b)
			spoof := &testEndpoint{call: attackerCall, conn: victim.conn}
			require.True(t, spoof.check(t, true), "quiet/released address can join B")
			require.Equal(t, "now B", sendAndReceive(t, b.conn.(*socketPort), victim, "now B"))
		})
	}
}

// TestWorkerLocalRouteStickiness covers a worker on its own socket, where
// the worker's address map is the only route: B's valid credentials cannot
// take A's active address, and can once A's window has lapsed.
func TestWorkerLocalRouteStickiness(t *testing.T) {
	const window = 10 * time.Second

	w, err := New(Config{consentTimeout: window, RouteStickinessWindow: window})
	require.NoError(t, err)
	t.Cleanup(func() { _ = w.Close() })
	victim := dialTestSessionOn(t, w).endpoint(t)
	require.True(t, victim.check(t, true))
	attacker := dialTestSessionOn(t, w)
	addr := victim.conn.LocalAddr().(*net.UDPAddr).AddrPort()
	spoof := &testEndpoint{call: attacker, conn: victim.conn}
	require.False(t, spoof.check(t, true), "B cannot take A's active address")
	require.Equal(t, victim.call.id, w.sessionAt(addr).id)

	// Age only A's last authenticated check; A's session stays live.
	w.mu.Lock()
	w.byAddrConsent[addr] = time.Now().Add(-window)
	w.mu.Unlock()
	require.True(t, spoof.check(t, true), "B can take the address once A's window has lapsed")
	require.Equal(t, attacker.id, w.sessionAt(addr).id)
}
