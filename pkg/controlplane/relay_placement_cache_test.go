package controlplane

import (
	"context"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

type assignmentReadCount struct {
	*sessionstore.Memory
	reads atomic.Int64
}

type pausedPlacementSample struct {
	*sessionstore.Memory
	armed            atomic.Bool
	entered, release chan struct{}
}

func (s *pausedPlacementSample) ListByWorker(ctx context.Context, addr netip.AddrPort) ([]sessionstore.Lease, error) {
	if s.armed.CompareAndSwap(true, false) {
		close(s.entered)
		select {
		case <-s.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return s.Memory.ListByWorker(ctx, addr)
}

func TestCreateRefusesSlotThatTurnsPendingDuringSelection(t *testing.T) {
	store := &pausedPlacementSample{Memory: sessionstore.NewMemory(), entered: make(chan struct{}), release: make(chan struct{})}
	p, a, b, _ := relayFailurePlane(t, store)
	store.armed.Store(true)
	beforeA, beforeB := a.next, b.next
	var release sync.Once
	var createErr error
	done := make(chan struct{})
	go func() { defer close(done); _, _, createErr = p.Create(t.Context(), "offer", "a") }()
	t.Cleanup(func() { release.Do(func() { close(store.release) }); <-done })
	awaitAdmission(t, store.entered) // pickRelay already chose the less-loaded r2.
	p.mu.Lock()
	binding := p.relaySet["r2"]
	binding.pending, binding.pairs = true, map[*registration]relayPairAck{}
	p.relaySet["r2"] = binding
	p.loadVersion++
	p.mu.Unlock()
	release.Do(func() { close(store.release) })
	awaitAdmission(t, done)
	require.ErrorIs(t, createErr, ErrNoTarget, "Create cannot use another slot's ready pair")
	require.Equal(t, beforeA, a.next, "no create command may reach an unjoined pair")
	require.Equal(t, beforeB, b.next)
}

func (s *assignmentReadCount) RelayOwner(ctx context.Context, id string) (sessionstore.Lease, sessionstore.RelayAssignment, error) {
	s.reads.Add(1)
	return s.Memory.RelayOwner(ctx, id)
}
func TestRelayPlacementReusesKnownAssignment(t *testing.T) {
	store := &assignmentReadCount{Memory: sessionstore.NewMemory()}
	p, _, _, id := relayFailurePlane(t, store)
	before := store.reads.Load()
	_, err := p.pickRelay(context.Background(), "", "")
	require.NoError(t, err)
	require.Equal(t, before, store.reads.Load(), "placement must not read every existing assignment")
	_, err = p.pickCapacity(context.Background(), "b", nil, true, id)
	require.NoError(t, err)
	require.Equal(t, before, store.reads.Load(), "takeover target selection must reuse the known slot")
}

func TestRelayPlacementDistributesNewCallsAcrossLiveRelays(t *testing.T) {
	store := sessionstore.NewMemory()
	p, _, _, first := relayFailurePlane(t, store)
	ids := []string{first}
	for range 7 {
		id, _, err := p.Create(context.Background(), "offer", "a")
		require.NoError(t, err)
		ids = append(ids, id)
	}
	counts := map[string]int{}
	for i, id := range ids {
		_, assignment, err := store.RelayOwner(context.Background(), id)
		require.NoError(t, err)
		require.Equal(t, []string{"r1", "r2"}[i%2], assignment.Slot)
		require.Empty(t, assignment.Pending)
		counts[assignment.Slot]++
	}
	require.Equal(t, map[string]int{"r1": 4, "r2": 4}, counts)
}
