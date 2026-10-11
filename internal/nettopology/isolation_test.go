package nettopology

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIsolationAcceptsOnlyDenialOrTimeout(t *testing.T) {
	for _, err := range []error{syscall.EPERM, syscall.ENETUNREACH, os.ErrDeadlineExceeded} {
		require.True(t, isolationDenied(fmt.Errorf("wrapped: %w", &net.OpError{Op: "dial", Err: err})), "%v", err)
	}
	for _, err := range []error{nil, syscall.ECONNREFUSED, syscall.EACCES, syscall.EHOSTUNREACH, context.Canceled, errors.New("unexpected failure")} {
		require.False(t, isolationDenied(err), "%v", err)
	}
}

func TestIsolationProbeAcceptsUDPTimeoutButNotCancellation(t *testing.T) {
	udp, err := net.ListenPacket("udp4", "127.0.0.1:0")
	require.NoError(t, err)
	defer udp.Close()
	require.NoError(t, requireBlocked(context.Background(), "udp4", udp.LocalAddr().String()))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, requireBlocked(ctx, "udp4", udp.LocalAddr().String()), context.Canceled)
}

func TestIsolationProbeRejectsRefusedTCPAndUDP(t *testing.T) {
	for _, network := range []string{"tcp4", "udp4"} {
		t.Run(network, func(t *testing.T) {
			var address string
			if network == "tcp4" {
				listener, err := net.Listen(network, "127.0.0.1:0")
				require.NoError(t, err)
				address = listener.Addr().String()
				require.NoError(t, listener.Close())
			} else {
				listener, err := net.ListenPacket(network, "127.0.0.1:0")
				require.NoError(t, err)
				address = listener.LocalAddr().String()
				require.NoError(t, listener.Close())
			}
			require.ErrorIs(t, requireBlocked(context.Background(), network, address), syscall.ECONNREFUSED)
		})
	}
}

func TestIsolationProbeRejectsReachableTCPAndUDP(t *testing.T) {
	tcp, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	defer tcp.Close()
	require.ErrorContains(t, requireBlocked(context.Background(), "tcp4", tcp.Addr().String()), "caller reached forbidden tcp4")
	udp, err := net.ListenPacket("udp4", "127.0.0.1:0")
	require.NoError(t, err)
	defer udp.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		var data [64]byte
		n, addr, err := udp.ReadFrom(data[:])
		if err == nil {
			_, _ = udp.WriteTo(data[:n], addr)
		}
	}()
	require.ErrorContains(t, requireBlocked(context.Background(), "udp4", udp.LocalAddr().String()), "caller reached forbidden udp4")
	_ = udp.Close()
	<-done
}
