package controlplane

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

type assignmentReadCount struct {
	*sessionstore.Memory
	reads atomic.Int64
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
