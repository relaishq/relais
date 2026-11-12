package clusterprocess

import (
	"errors"
	"io"
	"net"
	"strconv"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDriverPortBand(t *testing.T) {
	for _, tc := range []struct {
		name      string
		ephemeral portBand
		rangeErr  error
		want      portBand
		wantErr   bool
	}{
		{name: "Linux", ephemeral: portBand{32768, 60999}, want: portBand{20000, 32767}},
		{name: "macOS", ephemeral: portBand{49152, 65535}, want: portBand{20000, 49151}},
		{name: "read failure", rangeErr: errors.New("unavailable"), want: portBand{20000, 29999}},
		{name: "zero range", want: portBand{20000, 29999}},
		{name: "reversed range", ephemeral: portBand{49152, 32768}, want: portBand{20000, 29999}},
		{name: "out of bounds", ephemeral: portBand{32768, 65536}, want: portBand{20000, 29999}},
		{name: "single lower candidate", ephemeral: portBand{20001, 65535}, want: portBand{20000, 20000}},
		{name: "low custom range", ephemeral: portBand{10000, 30000}, want: portBand{30001, 65535}},
		{name: "below driver ports", ephemeral: portBand{1024, 9999}, want: portBand{20000, 65535}},
		{name: "no safe band", ephemeral: portBand{10000, 65535}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			band, err := bandOutsideEphemeral(tc.ephemeral, tc.rangeErr)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, band)
			require.GreaterOrEqual(t, band.first, 20000)
			require.GreaterOrEqual(t, band.last, band.first)
			if tc.rangeErr == nil && tc.ephemeral.first > 0 && tc.ephemeral.first <= tc.ephemeral.last && tc.ephemeral.last <= 65535 {
				require.True(t, band.last < tc.ephemeral.first || band.first > tc.ephemeral.last)
			}
		})
	}
}

func TestFreeTCPOutsideEphemeralRange(t *testing.T) { testFreePort(t, "tcp", FreeTCP) }
func TestFreeUDPOutsideEphemeralRange(t *testing.T) { testFreePort(t, "udp", FreeUDP) }

func testFreePort(t *testing.T, network string, allocate func() (string, error)) {
	t.Helper()
	band, err := driverPortBand()
	require.NoError(t, err)
	ephemeral, rangeErr := ephemeralPortRange()
	if rangeErr != nil {
		t.Logf("OS range unavailable, testing fallback band: %v", rangeErr)
	}
	for range 100 {
		addr, err := allocate()
		require.NoError(t, err)
		host, value, err := net.SplitHostPort(addr)
		require.NoError(t, err)
		require.Equal(t, "127.0.0.1", host)
		port, err := strconv.Atoi(value)
		require.NoError(t, err)
		require.NotEqual(t, 6379, port)
		require.NotEqual(t, 9101, port)
		require.GreaterOrEqual(t, port, band.first)
		require.LessOrEqual(t, port, band.last)
		if rangeErr == nil {
			require.True(t, port < ephemeral.first || port > ephemeral.last, "port %d inside ephemeral range %+v", port, ephemeral)
		}
		// The probe must release the socket so the child can bind it.
		if network == "udp" {
			conn, err := net.ListenPacket(network, addr)
			require.NoError(t, err)
			require.NoError(t, conn.Close())
		} else {
			listener, err := net.Listen(network, addr)
			require.NoError(t, err)
			require.NoError(t, listener.Close())
		}
	}
}

func TestFreePortRejectsOccupiedBand(t *testing.T) {
	for _, network := range []string{"tcp", "udp"} {
		t.Run(network, func(t *testing.T) {
			// Another socket holds the only candidate; it was never issued here.
			socket, port := occupy(t, network, "127.0.0.1:0")
			defer socket.Close()
			allocated, err := freePortInBand(network, portBand{port, port})
			require.Empty(t, allocated)
			require.ErrorIs(t, err, syscall.EADDRINUSE)
		})
	}
}

// #413: a topology picked its relay HTTP and control ports separately. Both
// picks returned one released port, the relay bound it first and control
// failed with EADDRINUSE. A later pick must never name an issued port, even
// across networks: callers also use TCP picks for UDP sockets.
func TestFreePortNeverReissuesAPort(t *testing.T) {
	for _, networks := range [][2]string{{"tcp", "tcp"}, {"udp", "udp"}, {"tcp", "udp"}, {"udp", "tcp"}} {
		t.Run(networks[0]+"-then-"+networks[1], func(t *testing.T) {
			socket, port := occupy(t, networks[0], "127.0.0.1:0")
			require.NoError(t, socket.Close())
			band := portBand{port, port}
			relayAddr, err := freePortInBand(networks[0], band)
			require.NoError(t, err)
			require.Equal(t, net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), relayAddr)
			// Control picks while the port is still free, before the relay binds.
			controlAddr, err := freePortInBand(networks[1], band)
			require.Empty(t, controlAddr)
			require.ErrorIs(t, err, errPortIssued)
			// The relay, which owns the port, still binds it.
			relay, _ := occupy(t, networks[0], relayAddr)
			require.NoError(t, relay.Close())
		})
	}
}

func occupy(t *testing.T, network, addr string) (io.Closer, int) {
	t.Helper()
	var socket io.Closer
	var local net.Addr
	if network == "udp" {
		conn, err := net.ListenPacket(network, addr)
		require.NoError(t, err)
		socket, local = conn, conn.LocalAddr()
	} else {
		listener, err := net.Listen(network, addr)
		require.NoError(t, err)
		socket, local = listener, listener.Addr()
	}
	_, value, err := net.SplitHostPort(local.String())
	require.NoError(t, err)
	port, err := strconv.Atoi(value)
	require.NoError(t, err)
	return socket, port
}
