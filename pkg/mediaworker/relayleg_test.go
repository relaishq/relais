package mediaworker

import (
	"context"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/pion/stun/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/relais/pkg/relay"
	"github.com/relais/pkg/sessionstore"
)

// TestWorkerBehindRelay drives a worker behind a relay from the relay's side
// of the relay leg: the test plays the relay. The worker claims each session
// for its own socket, advertises the relay's public address, answers a
// caller's ICE check through the relay with the caller's own address, ignores
// datagrams from anywhere but the relay, and releases the session when it
// ends.
func TestWorkerBehindRelay(t *testing.T) {
	owners := sessionstore.NewMemory()
	relayLeg := listenTestUDP(t)
	public := netip.MustParseAddrPort("192.0.2.10:3478")
	worker, err := New(Config{
		consentTimeout: testConsentTimeout,
		Relay: &RelayConfig{
			Addr:       relayLeg.LocalAddr().(*net.UDPAddr).AddrPort(),
			PublicAddr: public,
			Owners:     owners,
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, worker.Close()) })

	require.Equal(t, public, worker.MediaAddr(), "media address")
	require.NotEqual(t, public, worker.LocalAddr(), "the worker's own socket")

	id, answerSDP, err := worker.CreateSession(context.Background(), testOffer(testOfferAttrs{}))
	require.NoError(t, err)
	owner, err := owners.Owner(context.Background(), id)
	require.NoError(t, err, "session claimed before the answer is returned")
	require.Equal(t, worker.LocalAddr(), owner, "session owner")
	require.Contains(t, answerSDP, "a=candidate:"+hostCandidate(public), "answer advertises the relay")

	call := &testCall{worker: worker, id: id, username: id + ":" + testCallerUfrag, pwd: answerPwd(t, answerSDP)}
	caller := netip.MustParseAddrPort("198.51.100.7:50000")
	request := call.bindingRequest(t)

	// A check from anywhere but the relay is ignored.
	stranger := listenTestUDP(t)
	_, err = stranger.WriteToUDPAddrPort(append(relay.AppendHeader(nil, caller), request.Raw...), worker.LocalAddr())
	require.NoError(t, err)

	// Through the relay leg, the check is answered back through it, to the
	// caller named in the header, with the caller's address as mapped.
	_, err = relayLeg.WriteToUDPAddrPort(append(relay.AppendHeader(nil, caller), request.Raw...), worker.LocalAddr())
	require.NoError(t, err)
	require.NoError(t, relayLeg.SetReadDeadline(time.Now().Add(testCheckTimeout)))
	buf := make([]byte, receiveMTU)
	n, err := relayLeg.Read(buf)
	require.NoError(t, err)
	to, pkt, err := relay.ParseHeader(buf[:n])
	require.NoError(t, err)
	assert.Equal(t, caller, to, "response goes to the caller")

	response := &stun.Message{Raw: pkt}
	require.NoError(t, response.Decode())
	assert.Equal(t, stun.BindingSuccess, response.Type)
	assert.Equal(t, request.TransactionID, response.TransactionID)
	var mapped stun.XORMappedAddress
	require.NoError(t, mapped.GetFrom(response))
	assert.Equal(t, caller.Addr().AsSlice(), []byte(mapped.IP.To4()), "mapped address")
	assert.Equal(t, int(caller.Port()), mapped.Port, "mapped port")

	require.NoError(t, stranger.SetReadDeadline(time.Now().Add(100*time.Millisecond)))
	_, err = stranger.Read(buf)
	require.Error(t, err, "nothing goes back to a stranger")

	require.NoError(t, worker.EndSession(id))
	_, err = owners.Owner(context.Background(), id)
	require.ErrorIs(t, err, sessionstore.ErrNotFound, "session released when it ends")
}

// TestRelayLegCarriesFullSizePackets sends a caller's ICE check as large as
// the worker's read buffer (receiveMTU) over the relay leg. The relay-leg
// header must not eat into it: a truncated check fails its integrity check
// and goes unanswered.
func TestRelayLegCarriesFullSizePackets(t *testing.T) {
	relayLeg := listenTestUDP(t)
	worker, err := New(Config{
		consentTimeout: testConsentTimeout,
		Relay: &RelayConfig{
			Addr:       relayLeg.LocalAddr().(*net.UDPAddr).AddrPort(),
			PublicAddr: netip.MustParseAddrPort("192.0.2.10:3478"),
			Owners:     sessionstore.NewMemory(),
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, worker.Close()) })

	id, answerSDP, err := worker.CreateSession(context.Background(), testOffer(testOfferAttrs{}))
	require.NoError(t, err)
	call := &testCall{worker: worker, id: id, username: id + ":" + testCallerUfrag, pwd: answerPwd(t, answerSDP)}
	caller := netip.MustParseAddrPort("[2001:db8::7]:50000") // the longer IPv6 header

	request := call.paddedBindingRequest(t, receiveMTU)
	require.Len(t, request.Raw, receiveMTU)
	_, err = relayLeg.WriteToUDPAddrPort(append(relay.AppendHeader(nil, caller), request.Raw...), worker.LocalAddr())
	require.NoError(t, err)

	require.NoError(t, relayLeg.SetReadDeadline(time.Now().Add(testCheckTimeout)))
	buf := make([]byte, 2*receiveMTU)
	n, err := relayLeg.Read(buf)
	require.NoError(t, err, "a full-size check is answered")
	_, pkt, err := relay.ParseHeader(buf[:n])
	require.NoError(t, err)
	response := &stun.Message{Raw: pkt}
	require.NoError(t, response.Decode())
	assert.Equal(t, stun.BindingSuccess, response.Type)
	assert.Equal(t, request.TransactionID, response.TransactionID)
}

func TestRelayConfigIsValidated(t *testing.T) {
	owners := sessionstore.NewMemory()
	addr := netip.MustParseAddrPort("127.0.0.1:9")
	for name, cfg := range map[string]RelayConfig{
		"no relay address":      {PublicAddr: addr, Owners: owners},
		"no public address":     {Addr: addr, Owners: owners},
		"unspecified public IP": {Addr: addr, PublicAddr: netip.MustParseAddrPort("0.0.0.0:9"), Owners: owners},
		"no store":              {Addr: addr, PublicAddr: addr},
	} {
		_, err := New(Config{Relay: &cfg})
		assert.Error(t, err, name)
	}
}

// bindingRequest is an authenticated, nominating ICE check for the call.
func (c *testCall) bindingRequest(t *testing.T) *stun.Message {
	t.Helper()

	request, err := stun.Build(
		stun.BindingRequest,
		stun.NewTransactionIDSetter(stun.NewTransactionID()),
		stun.NewUsername(c.username),
		stun.RawAttribute{Type: stun.AttrUseCandidate},
		stun.NewShortTermIntegrity(c.pwd),
		stun.Fingerprint,
	)
	require.NoError(t, err)

	return request
}

// paddedBindingRequest is bindingRequest padded with a comprehension-optional
// attribute to exactly size bytes.
func (c *testCall) paddedBindingRequest(t *testing.T, size int) *stun.Message {
	t.Helper()

	build := func(padding int) *stun.Message {
		request, err := stun.Build(
			stun.BindingRequest,
			stun.NewTransactionIDSetter(stun.NewTransactionID()),
			stun.NewUsername(c.username),
			stun.RawAttribute{Type: stun.AttrUseCandidate},
			stun.RawAttribute{Type: 0x8070, Value: make([]byte, padding)},
			stun.NewShortTermIntegrity(c.pwd),
			stun.Fingerprint,
		)
		require.NoError(t, err)

		return request
	}

	return build(size - len(build(0).Raw))
}

func answerPwd(t *testing.T, answerSDP string) string {
	t.Helper()

	for _, line := range strings.Split(answerSDP, "\r\n") {
		if pwd, ok := strings.CutPrefix(line, "a=ice-pwd:"); ok {
			return pwd
		}
	}
	t.Fatal("answer has no a=ice-pwd")

	return ""
}

func listenTestUDP(t *testing.T) *net.UDPConn {
	t.Helper()

	conn, err := net.ListenUDP("udp4", net.UDPAddrFromAddrPort(netip.MustParseAddrPort("127.0.0.1:0")))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	return conn
}
