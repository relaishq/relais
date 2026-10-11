package nettopology

import (
	"net"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCallerSocketBindsSpecificIPv4(t *testing.T) {
	bind := netip.MustParseAddrPort("127.0.0.1:0")
	socket, err := listenCallerSocket(bind)
	require.NoError(t, err)
	defer socket.Close()
	local, ok := socket.LocalAddr().(*net.UDPAddr)
	require.True(t, ok)
	require.True(t, local.IP.To4() != nil && !local.IP.IsUnspecified(), "callharness: caller socket must bind a specific IPv4 UDP address: %s", local)
	require.Equal(t, bind.Addr(), local.AddrPort().Addr())
	require.NotZero(t, local.Port)
}

func TestCallerSocketRejectsUnspecifiedOrNonIPv4(t *testing.T) {
	for _, bind := range []netip.AddrPort{{}, netip.MustParseAddrPort("0.0.0.0:0"), netip.MustParseAddrPort("[::1]:0")} {
		socket, err := listenCallerSocket(bind)
		require.Nil(t, socket)
		require.ErrorContains(t, err, "specific IPv4 UDP address")
	}
}
