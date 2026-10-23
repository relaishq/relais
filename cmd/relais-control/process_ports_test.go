package main

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"syscall"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/clusterprocess"
	"github.com/stretchr/testify/require"
)

// processPorts holds every allocated endpoint until its child is about to
// bind. A later allocation therefore cannot return an earlier child's port,
// even when that child (control or a late worker) has not started yet.
type processPorts map[string]io.Closer

type processTCPPort struct {
	listener  net.Listener
	done      chan struct{}
	acceptErr error
}

func holdProcessTCPPort(listener net.Listener) *processTCPPort {
	port := &processTCPPort{listener: listener, done: make(chan struct{})}
	go func() {
		defer close(port.done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				if !errors.Is(err, net.ErrClosed) {
					port.acceptErr = err
				}
				return
			}
			// Reject probes promptly while the child is absent. If reset-on-close
			// is unavailable, an ordinary close still rejects the HTTP request.
			if tcp, ok := conn.(*net.TCPConn); ok {
				_ = tcp.SetLinger(0)
			}
			_ = conn.Close()
		}
	}()
	return port
}

func (p *processTCPPort) Close() error {
	err := p.listener.Close()
	<-p.done // finish closing any accepted connection before the child binds
	return errors.Join(err, p.acceptErr)
}

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
				var listener net.Listener
				listener, err = net.Listen(network, addr)
				if err == nil {
					socket = holdProcessTCPPort(listener)
				}
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
	for i := 0; i+1 < len(args); i++ {
		// These flags are local binds for the fixture's child binaries.
		// Peer flags such as -relay-leg and -relay-media keep their reservations.
		switch args[i] {
		case "-http", "-leg", "-media":
			if socket, ok := p[args[i+1]]; ok {
				require.NoError(t, socket.Close())
				delete(p, args[i+1])
			}
			i++
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
			if network == "tcp" {
				started := time.Now()
				conn, dialErr := net.DialTimeout("tcp", addr, time.Second)
				if dialErr == nil {
					require.NoError(t, conn.SetDeadline(started.Add(time.Second)))
					request, requestErr := http.NewRequest(http.MethodGet, "http://"+addr+"/healthz", nil)
					require.NoError(t, requestErr)
					dialErr = request.Write(conn)
					if dialErr == nil {
						_, dialErr = http.ReadResponse(bufio.NewReader(conn), request)
					}
					require.NoError(t, conn.Close())
				}
				require.Error(t, dialErr, "held endpoint must reject HTTP requests")
				require.True(t, errors.Is(dialErr, syscall.ECONNREFUSED) || errors.Is(dialErr, syscall.ECONNRESET) || errors.Is(dialErr, syscall.EPIPE) || errors.Is(dialErr, io.EOF) || errors.Is(dialErr, io.ErrUnexpectedEOF), "expected connection rejection, got %v", dialErr)
			}
			ports.handover(t, []string{"-http", addr})
			socket, err = bind()
			require.NoError(t, err, "child must be able to bind after handover")
			require.NoError(t, socket.Close())
		}
	}
}

func TestProcessPortsHandoverKeepsPeerAddressesReserved(t *testing.T) {
	ports := newProcessPorts(t)
	workerHTTP := ports.reserve(t, "tcp")
	relayLeg := ports.reserve(t, "udp")
	relayMedia := ports.reserve(t, "udp")
	// Worker A can start before the relay in the late-relay bootstrap case.
	ports.handover(t, []string{"relais-worker", "-http", workerHTTP, "-media", "127.0.0.1:0", "-relay-leg", relayLeg, "-relay-media", relayMedia})
	listener, err := net.Listen("tcp", workerHTTP)
	require.NoError(t, err, "worker's own bind address must be released")
	require.NoError(t, listener.Close())
	for _, peer := range []string{relayLeg, relayMedia} {
		socket, err := net.ListenPacket("udp", peer)
		if socket != nil {
			_ = socket.Close()
		}
		require.ErrorIs(t, err, syscall.EADDRINUSE, "peer bind address must stay reserved until that peer starts")
	}
}
