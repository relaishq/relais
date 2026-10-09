package sessionstore

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestStateFencingTransferAndDeletion(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	a, b := netip.MustParseAddrPort("127.0.0.1:1"), netip.MustParseAddrPort("127.0.0.1:2")
	lease, err := m.Claim(ctx, "snap", a, time.Minute)
	require.NoError(t, err)
	data := []byte("original")
	require.NoError(t, m.PutState(ctx, lease, data))
	data[0] = 'X'
	got, err := m.GetState(ctx, "snap")
	require.NoError(t, err)
	require.Equal(t, "original", string(got))
	got[0] = 'X'
	next, err := m.Transfer(ctx, lease, b, time.Minute)
	require.NoError(t, err)
	require.ErrorIs(t, m.PutState(ctx, lease, []byte("stale")), ErrLeaseLost)
	require.NoError(t, m.Release(ctx, lease))
	got, err = m.GetState(ctx, "snap")
	require.NoError(t, err)
	require.Equal(t, "original", string(got))
	require.NoError(t, m.PutState(ctx, next, []byte("resumed")))
	require.NoError(t, m.Release(ctx, next))
	_, err = m.GetState(ctx, "snap")
	require.ErrorIs(t, err, ErrNotFound)
	reclaimed, err := m.Claim(ctx, "snap", a, time.Minute)
	require.NoError(t, err)
	require.Greater(t, reclaimed.Epoch, next.Epoch)
	_, err = m.GetState(ctx, "snap")
	require.ErrorIs(t, err, ErrNotFound, "hangup removed all stored key material")
	require.ErrorIs(t, m.PutState(ctx, lease, []byte("stale")), ErrLeaseLost)
}

func TestExpiredLeaseCannotSnapshot(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()
	lease, err := m.Claim(ctx, "snap", netip.MustParseAddrPort("127.0.0.1:1"), time.Millisecond)
	require.NoError(t, err)
	require.NoError(t, m.PutState(ctx, lease, []byte("secret")))
	time.Sleep(3 * time.Millisecond)
	require.ErrorIs(t, m.PutState(ctx, lease, []byte("too late")), ErrLeaseLost)
	_, err = m.GetState(ctx, "snap")
	require.ErrorIs(t, err, ErrNotFound)
	require.NotContains(t, m.states, "snap")
}
