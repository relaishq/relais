package controlplane

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/sessionstore"

	"github.com/relaishq/relais/internal/privateapi"
	"github.com/stretchr/testify/require"
)

func TestRebindFlapBackPreservesOperatorDrain(t *testing.T) {
	for _, operator := range []bool{false, true} {
		t.Run(map[bool]string{false: "automatic", true: "operator"}[operator], func(t *testing.T) {
			r := newAdmissionRig(t, nil)
			authorizeLongControl(t, r.p)
			stream, err := r.open(t, "A", 1, 0, r.addr)
			require.NoError(t, err)
			peer := stream.run(statusHandler())
			client := &http.Client{Transport: peer}
			next := netip.MustParseAddrPort("127.0.0.1:17002")
			err = privateapi.Do(context.Background(), client, "http://control", "POST", "/leg-address", LinkAddress{Address: next}, nil, RemoteErrors)
			require.True(t, err == nil || errors.Is(err, privateapi.ErrUncertain), "%v", err)
			before, err := r.p.Status(context.Background())
			require.NoError(t, err)
			require.True(t, before.Workers[0].Draining, "drain must be committed even if reconnect overtakes its reply")
			if operator {
				require.NoError(t, r.p.MarkDraining("a"))
			}
			stream, err = r.open(t, "A", 2, 0, r.addr)
			require.NoError(t, err)
			stream.run(statusHandler())
			status, err := r.p.Status(context.Background())
			require.NoError(t, err)
			require.Equal(t, operator, status.Workers[0].Draining, "only the rebind-induced drain should be cancelled")
			records, err := r.p.restart.store.ControlRecords(context.Background(), r.p.restart.guard.Load().lease)
			require.NoError(t, err)
			_, saved := records["drain:a"]
			require.Equal(t, operator, saved, "cancelled automatic drain must not remain durable")
		})
	}
}

type heldDrainStore struct {
	*sessionstore.Memory
	address          netip.AddrPort
	enabled          atomic.Bool
	reads            atomic.Int32
	once             sync.Once
	entered, release chan struct{}
}

func (s *heldDrainStore) ListByWorker(ctx context.Context, addr netip.AddrPort) ([]sessionstore.Lease, error) {
	if addr == s.address && s.enabled.Load() {
		s.reads.Add(1)
		s.once.Do(func() {
			close(s.entered)
			select {
			case <-s.release:
			case <-ctx.Done():
			}
		})
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	return s.Memory.ListByWorker(ctx, addr)
}
func TestRebindDrainOutlivesCommandBudget(t *testing.T) {
	store := &heldDrainStore{Memory: sessionstore.NewMemory(), entered: make(chan struct{}), release: make(chan struct{})}
	r := newAdmissionRig(t, nil, store)
	authorizeLongControl(t, r.p)
	store.address = r.addr
	stream, err := r.open(t, "A", 1, 0, r.addr)
	require.NoError(t, err)
	peer := stream.run(statusHandler())
	var release sync.Once
	defer release.Do(func() { close(store.release) })
	store.enabled.Store(true)
	client := &http.Client{Transport: peer}
	report := func() error {
		return privateapi.Do(context.Background(), client, "http://control", "POST", "/leg-address", LinkAddress{Address: netip.MustParseAddrPort("127.0.0.1:17002")}, nil, RemoteErrors)
	}
	ack := make(chan error, 1)
	go func() { ack <- report() }()
	<-store.entered
	select {
	case err := <-ack:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("leg report waited for the drain instead of acknowledging")
	}
	require.NoError(t, report(), "repeated observation must acknowledge the same drain")
	time.Sleep(2200 * time.Millisecond)
	require.EqualValues(t, 1, store.reads.Load(), "only one detached drain may run per worker")
	release.Do(func() { close(store.release) })
	select {
	case <-peer.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("drain inherited the expired request deadline and never completed rejoin")
	}
}

func TestShutdownCancelsDetachedRebind(t *testing.T) {
	store := &heldDrainStore{Memory: sessionstore.NewMemory(), entered: make(chan struct{}), release: make(chan struct{})}
	r := newAdmissionRig(t, nil, store)
	store.address = r.addr
	stream, err := r.open(t, "A", 1, 0, r.addr)
	require.NoError(t, err)
	peer := stream.run(statusHandler())
	defer close(store.release)
	store.enabled.Store(true)
	client := &http.Client{Transport: peer}
	require.NoError(t, privateapi.Do(context.Background(), client, "http://control", "POST", "/leg-address", LinkAddress{Address: netip.MustParseAddrPort("127.0.0.1:17002")}, nil, RemoteErrors))
	<-store.entered
	stopped := make(chan struct{})
	go func() { r.p.BeginShutdown(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not cancel and join the detached drain")
	}
}
