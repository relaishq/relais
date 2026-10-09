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

// TestDTLSWriteCannotOutlastExport races a DTLS record against an export:
// the record is written while the export holds the session lock, as a
// close_notify or retransmission could be. Once the export has fenced the
// session, the record must not reach the caller, or it could end the call
// the new owner has just resumed.
func TestDTLSWriteCannotOutlastExport(t *testing.T) {
	worker, err := New(Config{consentTimeout: 10 * time.Second})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, worker.Close()) })
	call := dialTestSessionOn(t, worker)
	caller := call.endpoint(t)
	require.True(t, caller.check(t, true), "caller nominates its address")
	sess := worker.session(call.id)
	require.NotNil(t, sess)

	record := []byte{21, 0xfe, 0xfd, 'x'} // looks like a DTLS alert; the content does not matter
	_, err = sess.dtlsEndpoint.WriteTo(record, nil)
	require.NoError(t, err)
	require.Equal(t, string(record), receive(t, caller.conn), "a record before the export reaches the caller")

	// The export's critical section: lock, fence, unlock. The record is
	// written while it holds the lock.
	sess.mu.Lock()
	written := make(chan error, 1)
	go func() {
		_, err := sess.dtlsEndpoint.WriteTo(record, nil)
		written <- err
	}()
	time.Sleep(100 * time.Millisecond) // the write is now waiting for the lock
	sess.fenced.Store(true)
	sess.mu.Unlock()

	require.NoError(t, <-written, "a fenced write fails silently")
	require.Empty(t, receive(t, caller.conn), "a record written during the export reached the caller")
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
