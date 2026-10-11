package socketbind

import (
	"context"
	"net"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestOccupiedSocketBudgetAndRelease(t *testing.T) {
	held, err := net.ListenPacket("udp4", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = held.Close() })
	addr := held.LocalAddr().String()
	attempt := func() (net.PacketConn, error) { return net.ListenPacket("udp4", addr) }
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	socket, wait, err := Retry(ctx, attempt)
	require.Nil(t, socket)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.ErrorIs(t, err, syscall.EADDRINUSE)
	require.Positive(t, wait)
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { time.Sleep(20 * time.Millisecond); _ = held.Close(); close(done) }()
	socket, wait, err = Retry(ctx, attempt)
	<-done
	require.NoError(t, err)
	defer func() { _ = socket.Close() }()
	require.Positive(t, wait)
}

func TestDoesNotRetryOtherErrors(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	attempts := 0
	_, wait, err := Retry(ctx, func() (net.Listener, error) { attempts++; return nil, syscall.EACCES })
	require.ErrorIs(t, err, syscall.EACCES)
	require.Equal(t, 1, attempts)
	require.Zero(t, wait)
}

func TestCancellationDoesNotBind(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	cancel()
	attempts := 0
	_, _, err := Retry(ctx, func() (net.Listener, error) { attempts++; return nil, syscall.EADDRINUSE })
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, attempts)
}

func TestCancellationClosesJustBoundSocket(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var bound net.Listener
	t.Cleanup(func() {
		if bound != nil {
			_ = bound.Close()
		}
	})
	socket, _, err := Retry(ctx, func() (net.Listener, error) {
		var err error
		bound, err = net.Listen("tcp4", "127.0.0.1:0")
		cancel()
		return bound, err
	})
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, socket)
	require.NotNil(t, bound)
	if err := bound.(*net.TCPListener).SetDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		require.ErrorIs(t, err, net.ErrClosed)
	}
	_, err = bound.Accept()
	require.ErrorIs(t, err, net.ErrClosed)
}
