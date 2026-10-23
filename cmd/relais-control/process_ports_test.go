package main

import (
	"errors"
	"io"
	"net"
	"syscall"
	"testing"

	"github.com/relaishq/relais/internal/clusterprocess"
	"github.com/stretchr/testify/require"
)

// processPorts holds every allocated endpoint until its child is about to
// bind. A later allocation therefore cannot return an earlier child's port,
// even when that child (control or a late worker) has not started yet.
type processPorts map[string]io.Closer

func newProcessPorts(t *testing.T) processPorts {
	t.Helper()
	ports := processPorts{}
	t.Cleanup(func() {
		for _, socket := range ports {
			_ = socket.Close()
		}
	})
	return ports
}

func (p processPorts) reserve(t *testing.T, network string) string {
	t.Helper()
	for range 100 {
		var addr string
		var socket io.Closer
		var err error
		if network == "udp" {
			addr, err = clusterprocess.FreeUDP()
			if err == nil {
				socket, err = net.ListenPacket(network, addr)
			}
		} else {
			addr, err = clusterprocess.FreeTCP()
			if err == nil {
				socket, err = net.Listen(network, addr)
			}
		}
		if errors.Is(err, syscall.EADDRINUSE) {
			continue // another fixture won before this reservation was held
		}
		require.NoError(t, err)
		if _, held := p[addr]; held {
			require.NoError(t, socket.Close())
			continue
		}
		p[addr] = socket
		return addr
	}
	t.Fatal("could not reserve a process port")
	return ""
}

func (p processPorts) handover(t *testing.T, args []string) {
	t.Helper()
	for _, arg := range args {
		if socket, ok := p[arg]; ok {
			require.NoError(t, socket.Close())
			delete(p, arg)
		}
	}
}

func TestProcessPortsReservedUntilChildStart(t *testing.T) {
	ports := newProcessPorts(t)
	for _, network := range []string{"tcp", "udp"} {
		var addresses []string
		for range 4 {
			addr := ports.reserve(t, network)
			require.NotContains(t, addresses, addr)
			addresses = append(addresses, addr)
		}
		for _, addr := range addresses {
			bind := func() (io.Closer, error) {
				if network == "udp" {
					return net.ListenPacket(network, addr)
				}
				return net.Listen(network, addr)
			}
			socket, err := bind()
			if socket != nil {
				_ = socket.Close()
			}
			require.ErrorIs(t, err, syscall.EADDRINUSE, "endpoint must stay reserved before handover")
			ports.handover(t, []string{"-http", addr})
			socket, err = bind()
			require.NoError(t, err, "child must be able to bind after handover")
			require.NoError(t, socket.Close())
		}
	}
}
