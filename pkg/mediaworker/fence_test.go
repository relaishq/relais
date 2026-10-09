package mediaworker

import (
	"context"
	"crypto"
	"crypto/x509"
	"errors"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/pion/dtls/v3"
	"github.com/pion/dtls/v3/pkg/crypto/fingerprint"
	"github.com/pion/dtls/v3/pkg/crypto/selfsign"
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

// TestHangupDuringHandoverEndsTheCall hangs a call up while a handover has
// it between owners. The hangup must wait for the handover and end the call
// on the worker that now owns it; the caller sees that worker's
// close_notify. The test also checks that a completed handover sends the
// caller nothing at all, though the old owner closed its DTLS connection.
func TestHangupDuringHandoverEndsTheCall(t *testing.T) {
	socket, workerA, workerB := newTestSocket(t, 30*time.Second)
	call, client := dialDTLSCaller(t, workerA)

	_, err := socket.Handover(call.id, workerB, ResumeOptions{})
	require.NoError(t, err, "move A -> B")
	require.Same(t, workerB, socket.Owner(call.id))
	require.NoError(t, client.SetReadDeadline(time.Now().Add(silence)))
	_, err = client.Read(make([]byte, receiveMTU))
	require.True(t, isTimeout(err), "the caller received something from the move (a close_notify?): %v", err)

	hungUp := make(chan error, 1)
	socket.afterExport = func() {
		go func() { hungUp <- socket.EndSession(call.id) }()
		time.Sleep(100 * time.Millisecond) // the hangup arrives between owners
	}
	_, err = socket.Handover(call.id, workerA, ResumeOptions{})
	require.NoError(t, err, "move B -> A")
	require.NoError(t, <-hungUp, "hangup during the handover")

	require.Nil(t, workerA.session(call.id), "session still on worker A after the hangup")
	require.Nil(t, workerB.session(call.id), "session still on worker B after the hangup")
	require.Nil(t, socket.Owner(call.id), "session still routed after the hangup")
	require.ErrorIs(t, socket.EndSession(call.id), ErrUnknownSession)

	require.NoError(t, client.SetReadDeadline(time.Now().Add(2*time.Second)))
	_, err = client.Read(make([]byte, receiveMTU))
	require.Error(t, err, "the caller's DTLS connection is still open")
	require.False(t, isTimeout(err), "the caller saw no close_notify from the resumed worker")
}

// TestOverlappingHandoverIsRefused starts a second handover of a session
// while the first has it between owners. The second must be refused at
// once, not queued to run as another move when the first is done; a hangup
// during a move still waits for it and ends the call.
func TestOverlappingHandoverIsRefused(t *testing.T) {
	socket, workerA, workerB := newTestSocket(t, 30*time.Second)
	workerC, err := socket.NewWorker(Config{consentTimeout: 30 * time.Second})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, workerC.Close()) })
	call, client := dialDTLSCaller(t, workerA)

	overlapping := make(chan error, 1)
	socket.afterExport = func() {
		go func() {
			_, err := socket.Handover(call.id, workerC, ResumeOptions{})
			overlapping <- err
		}()
		select {
		case err := <-overlapping:
			overlapping <- err // answered while the first move is in flight
		case <-time.After(silence):
		}
	}
	_, err = socket.Handover(call.id, workerB, ResumeOptions{})
	require.NoError(t, err, "first move A -> B")
	socket.afterExport = nil
	require.ErrorIs(t, <-overlapping, ErrHandoverInProgress, "overlapping move to C")
	require.Same(t, workerB, socket.Owner(call.id), "owner after the moves")
	require.Nil(t, workerC.session(call.id), "the overlapping move reached worker C")

	hungUp := make(chan error, 1)
	socket.afterExport = func() {
		go func() { hungUp <- socket.EndSession(call.id) }()
		time.Sleep(100 * time.Millisecond)
	}
	_, err = socket.Handover(call.id, workerC, ResumeOptions{})
	require.NoError(t, err, "move B -> C")
	require.NoError(t, <-hungUp, "hangup during the move")
	require.Nil(t, socket.Owner(call.id), "session still routed after the hangup")
	require.Nil(t, workerC.session(call.id), "session still on worker C after the hangup")

	require.NoError(t, client.SetReadDeadline(time.Now().Add(2*time.Second)))
	_, err = client.Read(make([]byte, receiveMTU))
	require.Error(t, err, "the caller's DTLS connection is still open")
	require.False(t, isTimeout(err), "the caller saw no close_notify after the hangup")
}

// dialDTLSCaller sets up an established session on worker: a caller that
// nominates its address and completes the DTLS handshake as the client,
// with the certificate its offer's fingerprint names.
func dialDTLSCaller(t *testing.T, worker *Worker) (*testCall, *dtls.Conn) {
	t.Helper()

	cert, err := selfsign.GenerateSelfSigned()
	require.NoError(t, err)
	parsed, err := x509.ParseCertificate(cert.Certificate[0])
	require.NoError(t, err)
	callerFingerprint, err := fingerprint.Fingerprint(parsed, crypto.SHA256)
	require.NoError(t, err)
	offer := strings.ReplaceAll(testOffer(testOfferAttrs{}),
		strings.TrimSuffix(strings.Repeat("AB:", 32), ":"), callerFingerprint)

	id, answerSDP, err := worker.CreateSession(context.Background(), offer)
	require.NoError(t, err)
	ufrag, pwd := answerCredentials(t, answerSDP)
	call := &testCall{worker: worker, id: id, username: ufrag + ":" + testCallerUfrag, pwd: pwd}
	caller := call.endpoint(t)
	require.True(t, caller.check(t, true), "caller nominates its address")

	client, err := dtls.ClientWithOptions(caller.conn, net.UDPAddrFromAddrPort(worker.MediaAddr()),
		dtls.WithCertificates(cert),
		dtls.WithSRTPProtectionProfiles(dtls.SRTP_AEAD_AES_128_GCM),
		dtls.WithExtendedMasterSecret(dtls.RequireExtendedMasterSecret),
		dtls.WithInsecureSkipVerify(true))
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, client.HandshakeContext(ctx))

	sess := worker.session(id)
	require.NotNil(t, sess)
	require.Eventually(t, func() bool {
		sess.mu.Lock()
		defer sess.mu.Unlock()

		return sess.srtpIn != nil
	}, 5*time.Second, 10*time.Millisecond, "worker established the session")

	return call, client
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
