package controlplane

import (
	"context"
	"net/netip"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

type legacyRelayLeaseStore struct {
	*sessionstore.Memory
	reads atomic.Int64
}

func (s *legacyRelayLeaseStore) GetRelay(ctx context.Context, id string) (sessionstore.RelayLease, error) {
	s.reads.Add(1)
	return s.Memory.GetRelay(ctx, id)
}

// relais-control's legacy constructor does not configure a relay set. Even
// beyond the loss threshold it must not observe slots or start loss episodes.
func TestSingleRelayWithoutSetDoesNotDetectRelayLoss(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := &legacyRelayLeaseStore{Memory: sessionstore.NewMemory()}
		p := New(&fakeRelay{}, store)
		worker := &fakeWorker{store: store, addr: netip.MustParseAddrPort("127.0.0.1:24711"), running: map[string]bool{}}
		require.NoError(t, p.Register("w", worker.addr, worker))
		id, _, err := p.Create(t.Context(), "offer", "w")
		require.NoError(t, err)
		call, err := p.lookup(id)
		require.NoError(t, err)
		call.relayCapable = true
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() { defer close(done); p.runRelayLoss(ctx) }()
		time.Sleep(3 * relayLossAfter)
		cancel()
		<-done
		require.Zero(t, store.reads.Load())
		require.Empty(t, p.relaySet)
		require.Nil(t, call.relayEpisode)
		_, err = store.Get(t.Context(), id)
		require.NoError(t, err, "the live call remains owned")
		require.NotContains(t, planeMetricsBody(p), "relais_control_relay_")
		journal, err := store.ControlIntents(t.Context())
		require.NoError(t, err)
		require.Empty(t, journal)
	})
}
