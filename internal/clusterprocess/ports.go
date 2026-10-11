package clusterprocess

import (
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"strconv"
	"syscall"
)

type portBand struct {
	first int
	last  int
}

// driverPortBand falls back below the default Linux and macOS ephemeral ranges
// when the OS settings cannot be read. Never select well-known service ports.
func driverPortBand() (portBand, error) {
	ephemeral, err := ephemeralPortRange()
	return bandOutsideEphemeral(ephemeral, err)
}

func bandOutsideEphemeral(ephemeral portBand, rangeErr error) (portBand, error) {
	if rangeErr != nil || ephemeral.first < 1 || ephemeral.last > 65535 || ephemeral.first > ephemeral.last {
		return portBand{20000, 29999}, nil
	}
	if ephemeral.first > 20000 {
		return portBand{20000, ephemeral.first - 1}, nil
	}
	// A custom low ephemeral range may leave room above it instead.
	if ephemeral.last < 65535 {
		return portBand{max(20000, ephemeral.last+1), 65535}, nil
	}
	return portBand{}, fmt.Errorf("no driver ports outside ephemeral range %d-%d", ephemeral.first, ephemeral.last)
}

// FreeTCP returns a bind-checked loopback address outside the OS ephemeral range.
// The port is released for a child to bind; another explicit bind can still win.
func FreeTCP() (string, error) { return freePort("tcp") }

// FreeUDP is the UDP counterpart of FreeTCP.
func FreeUDP() (string, error) { return freePort("udp") }

func freePort(network string) (string, error) {
	band, err := driverPortBand()
	if err != nil {
		return "", err
	}
	return freePortInBand(network, band)
}

func freePortInBand(network string, band portBand) (string, error) {
	count := band.last - band.first + 1
	start := rand.IntN(count)
	var err error
	// Start randomly and visit each candidate once, even when the band is busy.
	for i := range count {
		addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(band.first+(start+i)%count))
		var socket io.Closer
		if network == "udp" {
			socket, err = net.ListenPacket(network, addr)
		} else {
			socket, err = net.Listen(network, addr)
		}
		if err == nil {
			return addr, socket.Close()
		}
		if !errors.Is(err, syscall.EADDRINUSE) {
			return "", err
		}
	}
	return "", fmt.Errorf("no free %s driver port in %d-%d: %w", network, band.first, band.last, err)
}
