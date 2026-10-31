package relay

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

type namedOutageOwners struct {
	*sessionstore.Memory
	failed atomic.Bool
}

func (s *namedOutageOwners) RelayOwner(ctx context.Context, id string) (sessionstore.Lease, sessionstore.RelayAssignment, error) {
	if s.failed.Load() {
		return sessionstore.Lease{}, sessionstore.RelayAssignment{}, errors.New("store unavailable")
	}
	return s.Memory.RelayOwner(ctx, id)
}
func TestNamedSlotConsentUsesConfirmedRouteDuringOutage(t *testing.T) {
	owners := &namedOutageOwners{Memory: sessionstore.NewMemory()}
	sys := startTestRelay(t, Config{Owners: owners, SlotID: "r1"})
	worker := sys.worker(t, sessionA)
	lease, err := owners.Get(context.Background(), sessionA)
	require.NoError(t, err)
	require.NoError(t, owners.CASRelay(context.Background(), lease, sessionstore.RelayAssignment{}, sessionstore.RelayAssignment{Slot: "r1"}))
	caller := newTestCaller(t)
	sys.connect(t, caller, worker, sessionA)
	owners.failed.Store(true)
	consent := bindingRequest(t, sessionA)
	caller.send(t, consent, sys.relay.PublicAddr())
	worker.expect(t, caller.addr(), consent)
	sys.confirm(t, worker, caller, consent)
	// An unconfirmed address gets no admission from a failed lookup.
	fresh := newTestCaller(t)
	fresh.send(t, bindingRequest(t, sessionA), sys.relay.PublicAddr())
	worker.expectNothing(t)
}
