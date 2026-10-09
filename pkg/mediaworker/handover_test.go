package mediaworker

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/sdp/v3"
	"github.com/pion/srtp/v3"
	"github.com/stretchr/testify/require"
)

// Live handovers are tested through the call harness (pkg/callharness),
// where a Pion caller observes them. These tests cover the pieces a caller
// cannot isolate: restoring SRTP indexes across a sequence-number wrap, the
// socket's fence, and handovers that must fail without harming the call.

var testSRTPProfiles = []srtp.ProtectionProfile{
	srtp.ProtectionProfileAeadAes128Gcm,
	srtp.ProtectionProfileAes128CmHmacSha1_80,
}

// TestRestoreInboundIndexAcrossWrap resumes a receiving context at the
// highest index seen (sequence number 65535, rollover counter 0) and
// decrypts the next packet, which wrapped to sequence number 0 with
// rollover counter 1. Restoring only the rollover counter, all pion/srtp
// offers, fails on exactly that packet.
func TestRestoreInboundIndexAcrossWrap(t *testing.T) {
	for _, profile := range testSRTPProfiles {
		t.Run(profileName(profile), func(t *testing.T) {
			keys := testSessionKeys(t, profile)
			const ssrc = 1234
			sender := testContext(t, keys.RemoteMasterKey, keys.RemoteMasterSalt, profile)
			receiver := testContext(t, keys.RemoteMasterKey, keys.RemoteMasterSalt, profile)
			for seq := 65530; seq <= 65535; seq++ {
				_, err := receiver.DecryptRTP(nil, testEncrypt(t, sender, ssrc, uint16(seq)), nil)
				require.NoError(t, err)
			}
			wrapped := testEncrypt(t, sender, ssrc, 0) // rollover counter 1

			naive := testContext(t, keys.RemoteMasterKey, keys.RemoteMasterSalt, profile)
			naive.SetROC(ssrc, 0)
			_, err := naive.DecryptRTP(nil, append([]byte(nil), wrapped...), nil)
			require.Error(t, err, "SetROC alone decrypts the packet after the wrap")

			resumed := testContext(t, keys.RemoteMasterKey, keys.RemoteMasterSalt, profile)
			require.NoError(t, restoreInboundIndex(resumed, keys, profile, ssrc, 65535))
			_, err = resumed.DecryptRTP(nil, wrapped, nil)
			require.NoError(t, err, "restored context decrypts the packet after the wrap")
			roc, _ := resumed.ROC(ssrc)
			require.EqualValues(t, 1, roc, "rollover counter after the wrap")
		})
	}
}

// TestRestoreOutboundIndexAcrossWrap resumes a sending context after it sent
// sequence number 65535 and checks that the receiver, which never moved,
// decrypts what it sends next: sequence number 0 with rollover counter 1.
func TestRestoreOutboundIndexAcrossWrap(t *testing.T) {
	for _, profile := range testSRTPProfiles {
		t.Run(profileName(profile), func(t *testing.T) {
			keys := testSessionKeys(t, profile)
			const ssrc = 5678
			sender := testContext(t, keys.LocalMasterKey, keys.LocalMasterSalt, profile)
			receiver := testContext(t, keys.LocalMasterKey, keys.LocalMasterSalt, profile)
			for seq := 65530; seq <= 65535; seq++ {
				_, err := receiver.DecryptRTP(nil, testEncrypt(t, sender, ssrc, uint16(seq)), nil)
				require.NoError(t, err)
			}

			resumed := testContext(t, keys.LocalMasterKey, keys.LocalMasterSalt, profile)
			require.NoError(t, restoreOutboundIndex(resumed, ssrc, 65535))
			for _, seq := range []uint16{0, 1, 2} {
				_, err := receiver.DecryptRTP(nil, testEncrypt(t, resumed, ssrc, seq), nil)
				require.NoError(t, err, "receiver decrypts resumed sequence number %d", seq)
			}
		})
	}
}

// TestSocketFencesOldOwner checks that only a session's current owner can
// send to the session's caller through the shared socket.
func TestSocketFencesOldOwner(t *testing.T) {
	socket, workerA, workerB := newTestSocket(t, testConsentTimeout)
	portA, portB := workerA.conn.(*socketPort), workerB.conn.(*socketPort)

	caller, err := net.ListenUDP("udp4", net.UDPAddrFromAddrPort(netip.MustParseAddrPort("127.0.0.1:0")))
	require.NoError(t, err)
	t.Cleanup(func() { _ = caller.Close() })
	callerAddr := caller.LocalAddr().(*net.UDPAddr).AddrPort()

	socket.claim("session", portA)
	socket.mu.Lock()
	socket.flows[callerAddr] = "session"
	socket.mu.Unlock()

	received := func() string {
		require.NoError(t, caller.SetReadDeadline(time.Now().Add(200*time.Millisecond)))
		buf := make([]byte, 64)
		n, err := caller.Read(buf)
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return ""
		}
		require.NoError(t, err)

		return string(buf[:n])
	}

	_, err = portB.WriteToUDPAddrPort([]byte("from B"), callerAddr)
	require.NoError(t, err)
	_, err = portA.WriteToUDPAddrPort([]byte("from A"), callerAddr)
	require.NoError(t, err)
	require.Equal(t, "from A", received(), "only the owner reaches the caller")
	require.Empty(t, received())

	socket.route("session", portB)
	_, err = portA.WriteToUDPAddrPort([]byte("stale A"), callerAddr)
	require.NoError(t, err, "a fenced write fails silently, like a lost datagram")
	_, err = portB.WriteToUDPAddrPort([]byte("from B"), callerAddr)
	require.NoError(t, err)
	require.Equal(t, "from B", received(), "after the move only the new owner reaches the caller")
	require.Empty(t, received())
}

// TestHandoverOfUnestablishedSession checks that a session cannot be handed
// over before its DTLS handshake, and that trying leaves it running on its
// worker: its ICE checks are still answered there.
func TestHandoverOfUnestablishedSession(t *testing.T) {
	socket, workerA, workerB := newTestSocket(t, testConsentTimeout)
	call := dialTestSessionOn(t, workerA)
	nominated := call.endpoint(t)
	require.True(t, nominated.check(t, true), "nomination answered")

	_, err := socket.Handover(call.id, workerB, ResumeOptions{})
	require.ErrorIs(t, err, ErrNotEstablished)
	require.Same(t, workerA, socket.Owner(call.id), "owner after a failed handover")
	require.True(t, nominated.check(t, false), "check answered after a failed handover")

	_, err = socket.Handover(call.id, workerA, ResumeOptions{})
	require.ErrorIs(t, err, errSameOwner)
	_, err = socket.Handover("nosuchsession", workerB, ResumeOptions{})
	require.ErrorIs(t, err, ErrUnknownSession)
}

func TestResumeSessionRejectsBadState(t *testing.T) {
	worker := newTestWorker(t)

	_, err := worker.ResumeSession([]byte("not json"), ResumeOptions{})
	require.ErrorIs(t, err, errBadState)
	_, err = worker.ResumeSession([]byte(`{"Version":2}`), ResumeOptions{})
	require.ErrorIs(t, err, errStateVersion)
	_, err = worker.ResumeSession([]byte(`{"Version":5,"State":{"Version":5,"ID":"a","ICE":{"LocalUfrag":"a"}}}`),
		ResumeOptions{})
	require.ErrorIs(t, err, errBadState)
	_, err = worker.ResumeSession(nil, ResumeOptions{SequenceMargin: 1 << 15})
	require.Error(t, err, "margin of 2^15")
}

func newTestSocket(t *testing.T, consentTimeout time.Duration) (*Socket, *Worker, *Worker) {
	t.Helper()

	socket, err := ListenSocket(SocketConfig{})
	require.NoError(t, err)
	var workers []*Worker
	for range 2 {
		worker, err := socket.NewWorker(Config{consentTimeout: consentTimeout})
		require.NoError(t, err)
		workers = append(workers, worker)
	}
	t.Cleanup(func() {
		for _, worker := range workers {
			require.NoError(t, worker.Close())
		}
		require.NoError(t, socket.Close())
	})

	return socket, workers[0], workers[1]
}

// dialTestSessionOn is dialTestSession on a given worker.
func dialTestSessionOn(t *testing.T, worker *Worker) *testCall {
	t.Helper()

	id, answerSDP, err := worker.CreateSession(context.Background(), testOffer(testOfferAttrs{}))
	require.NoError(t, err)
	ufrag, pwd := answerCredentials(t, answerSDP)

	return &testCall{worker: worker, id: id, username: ufrag + ":" + testCallerUfrag, pwd: pwd}
}

// answerCredentials returns the worker's ICE ufrag and password from an
// answer.
func answerCredentials(t *testing.T, answerSDP string) (ufrag, pwd string) {
	t.Helper()

	var answer sdp.SessionDescription
	require.NoError(t, answer.UnmarshalString(answerSDP))
	for _, md := range answer.MediaDescriptions {
		if md.MediaName.Port.Value != 0 {
			ufrag, _ = md.Attribute("ice-ufrag")
			pwd, _ = md.Attribute("ice-pwd")
		}
	}
	require.NotEmpty(t, ufrag, "answer ice-ufrag")
	require.NotEmpty(t, pwd, "answer ice-pwd")

	return ufrag, pwd
}

func testSessionKeys(t *testing.T, profile srtp.ProtectionProfile) srtp.SessionKeys {
	t.Helper()

	keyLen, err := profile.KeyLen()
	require.NoError(t, err)
	saltLen, err := profile.SaltLen()
	require.NoError(t, err)
	fill := func(n int, b byte) []byte {
		out := make([]byte, n)
		for i := range out {
			out[i] = b + byte(i)
		}

		return out
	}

	return srtp.SessionKeys{
		LocalMasterKey:   fill(keyLen, 1),
		LocalMasterSalt:  fill(saltLen, 2),
		RemoteMasterKey:  fill(keyLen, 3),
		RemoteMasterSalt: fill(saltLen, 4),
	}
}

func testContext(t *testing.T, key, salt []byte, profile srtp.ProtectionProfile) *srtp.Context {
	t.Helper()

	ctx, err := srtp.CreateContext(key, salt, profile, srtp.SRTPReplayProtection(replayWindow))
	require.NoError(t, err)

	return ctx
}

func testEncrypt(t *testing.T, ctx *srtp.Context, ssrc uint32, seq uint16) []byte {
	t.Helper()

	raw, err := (&rtp.Packet{
		Header:  rtp.Header{Version: 2, PayloadType: 111, SequenceNumber: seq, SSRC: ssrc},
		Payload: []byte{1, 2, 3},
	}).Marshal()
	require.NoError(t, err)
	encrypted, err := ctx.EncryptRTP(nil, raw, nil)
	require.NoError(t, err)

	return encrypted
}

func profileName(profile srtp.ProtectionProfile) string {
	if profile == srtp.ProtectionProfileAeadAes128Gcm {
		return "AEAD_AES_128_GCM"
	}

	return "AES128_CM_HMAC_SHA1_80"
}
