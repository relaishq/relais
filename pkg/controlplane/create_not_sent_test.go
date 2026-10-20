package controlplane

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/relaishq/relais/internal/privateapi"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

func TestDialRefusedCreateIsDefiniteAndReleasesReservation(t *testing.T) {
	ctx := context.Background()
	store := sessionstore.NewMemory()
	address := netip.MustParseAddrPort("127.0.0.1:23501")
	var dials atomic.Int64
	transport := &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
		dials.Add(1)
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	}}
	defer transport.CloseIdleConnections()
	worker := &RemoteWorker{URL: "http://worker.invalid", Client: &http.Client{Transport: transport}}
	p := New(&fakeRelay{}, store)
	require.NoError(t, p.Register("a", address, worker))
	authorizeTestControl(t, p)
	_, _, err := p.Create(ctx, "offer", "a")
	require.ErrorIs(t, err, ErrCreateNotSent)
	require.ErrorIs(t, err, syscall.ECONNREFUSED)
	require.False(t, errors.Is(err, privateapi.ErrUncertain), "nothing could have been accepted")
	require.EqualValues(t, 1, dials.Load(), "mutating effects must not be retried")
	require.Zero(t, p.workers["a"].reserved)
	require.Empty(t, p.restart.creates)
	records, err := store.ControlRecords(ctx, p.restart.guard.Load().lease)
	require.NoError(t, err)
	require.Empty(t, records, "the definite failure must clear the creation journal")
	leases, err := store.ListByWorker(ctx, address)
	require.NoError(t, err)
	require.Empty(t, leases)
}
