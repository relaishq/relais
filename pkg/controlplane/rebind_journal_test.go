package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/netip"
	"sync/atomic"
	"testing"

	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

type rebindJournalStore struct {
	*heldDrainStore
	refuseWrites atomic.Bool
}

func (s *rebindJournalStore) PutControlRecord(ctx context.Context, lease sessionstore.ControlLease, key string, data json.RawMessage) error {
	if key == "worker:a" && s.refuseWrites.Load() {
		return sessionstore.ErrTransient
	}
	return s.Memory.PutControlRecord(ctx, lease, key, data)
}

func TestRepeatedLegObservationNeedsNoJournalWrite(t *testing.T) {
	store := &rebindJournalStore{heldDrainStore: &heldDrainStore{Memory: sessionstore.NewMemory(), entered: make(chan struct{}), release: make(chan struct{})}}
	r := newPipeAdmissionRig(t, noContentHandler(), store)
	authorizeLongControl(t, r.p)
	store.address = r.addr
	stream, err := r.open(t, "A", 1, 0, r.addr)
	require.NoError(t, err)
	peer := stream.run(statusHandler())
	store.enabled.Store(true)
	defer close(store.release)
	client := &http.Client{Transport: peer}
	next := netip.MustParseAddrPort("127.0.0.1:17002")
	require.NoError(t, reportLeg(context.Background(), client, next))
	<-store.entered
	store.refuseWrites.Store(true)
	require.NoError(t, reportLeg(context.Background(), client, next), "unchanged keepalive must not rewrite the durable drain target")
	store.refuseWrites.Store(false)
}
