package main

import (
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCallerHostBindWithoutNamespace(t *testing.T) {
	external, err := childTopology("http://127.0.0.1/calls", netip.MustParseAddrPort("127.0.0.1:20000"), "", "127.0.0.1:0")
	require.NoError(t, err)
	require.NotNil(t, external.CallerSocket)
	conn, err := external.CallerSocket()
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	require.Equal(t, "127.0.0.1", conn.LocalAddr().(*net.UDPAddr).IP.String())
	require.NotZero(t, conn.LocalAddr().(*net.UDPAddr).Port)
	_, err = conn.WriteTo([]byte("host socket"), conn.LocalAddr())
	require.NoError(t, err)
	buf := make([]byte, 32)
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(time.Second)))
	n, _, err := conn.ReadFrom(buf)
	require.NoError(t, err)
	require.Equal(t, "host socket", string(buf[:n]))
}
