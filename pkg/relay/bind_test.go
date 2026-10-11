package relay

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

func TestBindBudgetClosesPartialSockets(t *testing.T) {
	public, err := net.ListenPacket("udp4", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = public.Close() }()
	workers, err := net.ListenPacket("udp4", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = workers.Close() }()
	addr := public.LocalAddr().String()
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	closed := make(chan struct{})
	go func() { time.Sleep(20 * time.Millisecond); _ = public.Close(); close(closed) }()
	continuity := &ContinuityStatus{}
	r, err := New(Config{Owners: sessionstore.NewMemory(), PublicAddr: addr, WorkerAddr: workers.LocalAddr().String(), BindContext: ctx, Continuity: continuity})
	<-closed
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Nil(t, r, "packet loops must never start with an incomplete socket set")
	require.Positive(t, continuity.BindWait)
	rebound, err := net.ListenPacket("udp4", addr)
	require.NoError(t, err, "failed construction must release its successful public bind")
	require.NoError(t, rebound.Close())
}
