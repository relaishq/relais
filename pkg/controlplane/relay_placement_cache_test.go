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
	p, _, _, _ := relayFailurePlane(t, store)
	before := store.reads.Load()
	_, err := p.pickRelay(context.Background(), "")
	require.NoError(t, err)
	require.Equal(t, before, store.reads.Load(), "placement must not read every existing assignment")
}
