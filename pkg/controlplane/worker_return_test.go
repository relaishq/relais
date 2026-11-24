package controlplane

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestReturningWorkerAtSameAddressReceivesNewCallsAfterTakeover(t *testing.T) {
	ctx := context.Background()
	r := newAdmissionRig(t, nil)
	r.p.SetRelay(&fakeRelay{})
	source := &fakeWorker{store: r.p.store, addr: r.addr, running: map[string]bool{}}
	stream, err := r.open(t, "before-crash", 1, 0, r.addr)
	require.NoError(t, err)
	peer := stream.run(unitWorkerHandler(source))
	t.Cleanup(peer.Close)
	target := &takeoverWorker{fakeWorker: &fakeWorker{store: r.p.store, addr: netip.MustParseAddrPort("127.0.0.1:17002"), running: map[string]bool{}}}
	require.NoError(t, r.p.Register("b", target.addr, target))
	id, _, err := r.p.Create(ctx, "offer", "a")
	require.NoError(t, err)
	lease, err := r.p.store.Get(ctx, id)
	require.NoError(t, err)
	require.NoError(t, r.p.store.PutState(ctx, lease, takeoverSnapshot(t, id, 0)))
	peer.Close()
	r.p.mu.Lock()
	old := r.p.workers["a"]
	expireObservedWorker(t, old)
	r.p.mu.Unlock()
	require.True(t, r.p.recoverWorker(ctx, old, time.Now()))
	r.p.mu.Lock()
	old.observation.recoveryFinished(true)
	r.p.mu.Unlock()
	require.True(t, target.runs(id), "takeover completes before the worker returns")
	leases, err := r.p.store.ListByWorker(ctx, r.addr)
	require.NoError(t, err)
	require.Empty(t, leases)

	// A fresh instance's hello must invoke Replace even at the same address.
	// No operator replace request or manual heartbeat acknowledgement is sent.
	returning := &fakeWorker{store: r.p.store, addr: r.addr, next: 100, running: map[string]bool{}}
	stream, err = r.open(t, "after-crash", 1, 0, r.addr)
	require.NoError(t, err)
	peer = stream.run(unitWorkerHandler(returning))
	t.Cleanup(peer.Close)
	r.p.mu.Lock()
	replacement := r.p.workers["a"]
	r.p.mu.Unlock()
	require.NotSame(t, old, replacement)
	require.Equal(t, r.addr, replacement.addr)
	newID, _, err := r.p.Create(ctx, "new offer", "a")
	require.NoError(t, err)
	require.True(t, returning.runs(newID))
	lease, err = r.p.store.Get(ctx, newID)
	require.NoError(t, err)
	require.Equal(t, r.addr, lease.Worker)
	oldLease, err := r.p.store.Get(ctx, id)
	require.NoError(t, err)
	require.Equal(t, target.addr, oldLease.Worker)
	status, err := r.p.Status(ctx)
	require.NoError(t, err)
	require.Len(t, status.Takeovers, 1)
	require.False(t, status.Takeovers[0].Lost)
	require.Zero(t, status.LostCount)
}
