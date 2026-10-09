package mediaworker

import (
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// These tests pin down who may reach a caller, and when: only a session's
// current owner, only addresses that authenticated, and nothing from a
// session after its export.

// silence is how long a test waits to be sure nothing arrives.
const silence = 300 * time.Millisecond

// TestUnauthenticatedCheckMovesNoFlow sends binding requests that fail
// authentication, naming a session the sender does not hold the password
// of. Neither may change the socket's flows: the established caller's
// address still belongs to its own session, so its owner still reaches it,
// and an address that never authenticated is not reachable at all. Once an
// address does authenticate, it becomes a flow.
func TestUnauthenticatedCheckMovesNoFlow(t *testing.T) {
	_, workerA, workerB := newTestSocket(t, 10*time.Second)
	portA := workerA.conn.(*socketPort)

	callA := dialTestSessionOn(t, workerA)
	callerA := callA.endpoint(t)
	require.True(t, callerA.check(t, true), "caller A nominates its address")
	callB := dialTestSessionOn(t, workerB)
	require.True(t, callB.endpoint(t).check(t, true), "caller B nominates its address")
	require.Equal(t, "to A", sendAndReceive(t, portA, callerA, "to A"), "worker A reaches caller A")

	// From caller A's own address, a request for session B with the wrong
	// password, as a spoofer could send.
	forged := &testEndpoint{call: &testCall{worker: workerB, id: callB.id, username: callB.username, pwd: "notthepassword"}, conn: callerA.conn}
	require.False(t, forged.check(t, false), "forged request for session B answered")
	require.Equal(t, "still to A", sendAndReceive(t, portA, callerA, "still to A"),
		"after the forged request, worker A still reaches caller A")

	// From a new address, a request for session A with the wrong password.
	stranger := callA.endpoint(t)
	forged = &testEndpoint{call: &testCall{worker: workerA, id: callA.id, username: callA.username, pwd: "notthepassword"}, conn: stranger.conn}
	require.False(t, forged.check(t, false), "forged request for session A answered")
	require.Empty(t, sendAndReceive(t, portA, stranger, "to stranger"), "an address that never authenticated is a flow")

	// The same address authenticates: now it is a flow of session A.
	require.True(t, stranger.check(t, false), "valid check from the new address answered")
	require.Equal(t, "now to stranger", sendAndReceive(t, portA, stranger, "now to stranger"))
}

// sendAndReceive writes msg to the endpoint's address through port, and
// returns what the endpoint receives ("" when nothing arrives).
func sendAndReceive(t *testing.T, port *socketPort, to *testEndpoint, msg string) string {
	t.Helper()

	addr := to.conn.LocalAddr().(*net.UDPAddr).AddrPort()
	_, err := port.WriteToUDPAddrPort([]byte(msg), netip.AddrPortFrom(addr.Addr().Unmap(), addr.Port()))
	require.NoError(t, err)

	return receive(t, to.conn)
}

// receive returns the next datagram on conn, or "" if none arrives within
// silence.
func receive(t *testing.T, conn *net.UDPConn) string {
	t.Helper()

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(silence)))
	buf := make([]byte, receiveMTU)
	n, err := conn.Read(buf)
	if isTimeout(err) {
		return ""
	}
	require.NoError(t, err)

	return string(buf[:n])
}

func isTimeout(err error) bool {
	var netErr net.Error

	return errors.As(err, &netErr) && netErr.Timeout()
}
