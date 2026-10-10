package callharness

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestExternalCallerSocketFactory(t *testing.T) {
	h := &Harness{}
	socket, err := h.newCallerSocket(newRecorder())
	require.NoError(t, err)
	defer socket.close()
	require.True(t, socket.acceptsCandidate(net.ParseIP("127.0.0.2")))
	require.False(t, socket.acceptsCandidate(net.ParseIP("10.1.0.2")))
	var created net.PacketConn
	h.external = &ExternalTopology{CallerSocket: func() (net.PacketConn, error) {
		conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
		created = conn
		return conn, err
	}}
	configured, err := h.newCallerSocket(newRecorder())
	require.NoError(t, err)
	require.True(t, configured.acceptsCandidate(net.ParseIP("127.0.0.1")))
	require.False(t, configured.acceptsCandidate(net.ParseIP("127.0.0.2")))
	require.NoError(t, configured.close())
	_, err = created.WriteTo([]byte("closed"), created.LocalAddr())
	require.Error(t, err)
	failure := errors.New("socket failed")
	leaked, listenErr := net.ListenPacket("udp4", "127.0.0.1:0")
	require.NoError(t, listenErr)
	h.external.CallerSocket = func() (net.PacketConn, error) { return leaked, failure }
	_, err = h.newCallerSocket(newRecorder())
	require.ErrorIs(t, err, failure)
	_, err = leaked.WriteTo([]byte("closed"), leaked.LocalAddr())
	require.Error(t, err)
	h.external.CallerSocket = func() (net.PacketConn, error) { return nil, failure }
	_, err = h.newCallerSocket(newRecorder())
	require.ErrorIs(t, err, failure)
	h.external.CallerSocket = func() (net.PacketConn, error) { return net.ListenPacket("udp4", "0.0.0.0:0") }
	_, err = h.newCallerSocket(newRecorder())
	require.ErrorContains(t, err, "specific IPv4")
	h.external.CallerSocket = func() (net.PacketConn, error) { return nil, nil }
	_, err = h.newCallerSocket(newRecorder())
	require.Error(t, err)
	require.True(t, (&callerSocket{candidateIP: net.ParseIP("10.1.0.2")}).acceptsCandidate(net.ParseIP("10.1.0.2")))
	require.False(t, (&callerSocket{candidateIP: net.ParseIP("10.1.0.2")}).acceptsCandidate(net.ParseIP("127.0.0.1")))
}

func TestDialUsesExternalCallerSocketFactory(t *testing.T) {
	failure := errors.New("caller namespace unavailable")
	called := false
	harness, err := Start(Options{External: &ExternalTopology{
		SignalingURL: "http://127.0.0.1:1/calls",
		RelayAddr:    netip.MustParseAddrPort("127.0.0.1:9"),
		CallerSocket: func() (net.PacketConn, error) { called = true; return nil, failure },
	}})
	require.NoError(t, err)
	defer harness.Close()
	_, err = harness.Dial(context.Background(), CallOptions{})
	require.ErrorIs(t, err, failure)
	require.True(t, called, "the production Dial path must create media through the supplied factory before signaling")
}
