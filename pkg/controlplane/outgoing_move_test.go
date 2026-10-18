package controlplane

import (
	"context"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type outgoingHoldRelay struct {
	*fakeRelay
	once             sync.Once
	entered, release chan struct{}
}

func (r *outgoingHoldRelay) HoldSession(ctx context.Context, id string, from netip.AddrPort) error {
	first := false
	r.once.Do(func() { first = true; close(r.entered) })
	if first {
		select {
		case <-r.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return r.fakeRelay.HoldSession(ctx, id, from)
}

func holdOutgoingMove(t *testing.T, p *Plane, r *fakeRelay, id string) func() {
	t.Helper()
	gate := &outgoingHoldRelay{fakeRelay: r, entered: make(chan struct{}), release: make(chan struct{})}
	p.relay = gate
	done := make(chan error, 1)
	go func() { _, err := p.Move(context.Background(), id, "b"); done <- err }()
	var release sync.Once
	finish := func() { release.Do(func() { close(gate.release); <-done }) }
	t.Cleanup(finish)
	select {
	case <-gate.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("move did not reach relay hold")
	}
	return finish
}

func TestOutgoingMoveDoesNotBlockDrain(t *testing.T) {
	p, _, b, r := setup(t)
	p.config.WorkerCallCap = 2
	ctx := context.Background()
	first, _, err := p.Create(ctx, "offer", "a")
	require.NoError(t, err)
	second, _, err := p.Create(ctx, "offer", "a")
	require.NoError(t, err)
	finish := holdOutgoingMove(t, p, r, first)
	moves, err := p.Drain(ctx, "a")
	finish()
	// The held call reports an overlapping move; the other call still drains.
	require.ErrorIs(t, err, ErrMoveInProgress)
	require.Len(t, moves, 2)
	require.True(t, b.runs(second), "rollback protection is not incoming work")
}

func TestOutgoingRollbackSlotDoesNotIncreaseDemand(t *testing.T) {
	p, _, _, r := setup(t)
	p.config.WorkerCallCap = 2
	id, _, err := p.Create(context.Background(), "offer", "a")
	require.NoError(t, err)
	holdOutgoingMove(t, p, r, id)
	require.NoError(t, p.SamplePool(context.Background()))
	sample := p.PoolSnapshot()
	require.Equal(t, 1, sample.Reservations, "only the target has incoming work")
	require.Equal(t, 2, *sample.Demand, "one call plus its incoming destination reservation")
}

func TestCrashRecoveryContinuesDuringOutgoingMove(t *testing.T) {
	p, _, b, r := setup(t)
	p.config.WorkerCallCap = 2
	p.workers["b"].worker = &takeoverWorker{fakeWorker: b}
	ctx := context.Background()
	first, _, err := p.Create(ctx, "offer", "a")
	require.NoError(t, err)
	second, _, err := p.Create(ctx, "offer", "a")
	require.NoError(t, err)
	lease, err := p.store.Get(ctx, second)
	require.NoError(t, err)
	require.NoError(t, p.store.PutState(ctx, lease, takeoverSnapshot(t, second, 0)))
	finish := holdOutgoingMove(t, p, r, first)
	p.mu.Lock()
	source := p.workers["a"]
	source.dead = true
	p.mu.Unlock()
	done := make(chan bool, 1)
	go func() { done <- p.recoverWorker(ctx, source, time.Now()) }()
	t.Cleanup(func() { finish(); <-done })
	require.Eventually(t, func() bool { return b.runs(second) }, time.Second, 5*time.Millisecond, "other calls recover before the outgoing move finishes")
}

func TestUnlimitedDrainPreservesNoTargetDuringMove(t *testing.T) {
	p, _, _, r := setup(t)
	id, _, err := p.Create(context.Background(), "offer", "a")
	require.NoError(t, err)
	holdOutgoingMove(t, p, r, id)
	require.NoError(t, p.MarkDraining("b"))
	moves, err := p.Drain(context.Background(), "a")
	require.ErrorIs(t, err, ErrNoTarget)
	require.Empty(t, moves)
}
