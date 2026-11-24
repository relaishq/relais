package controlplane

import (
	"context"
	"errors"
	"net/netip"
	"testing"

	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/relay"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

type registryRecorder struct {
	addresses []netip.AddrPort
	instance  string
	err       error
}

func (r *registryRecorder) AddWorker(_ context.Context, addr netip.AddrPort) error {
	r.addresses = append(r.addresses, addr)
	return r.err
}
func TestRelayRestartReRegistersOnlyLiveWorkers(t *testing.T) {
	plane, a, b, _ := setup(t)
	plane.mu.Lock()
	expireObservedWorker(t, plane.workers["a"])
	plane.mu.Unlock()
	registry := &registryRecorder{}
	require.NoError(t, plane.RegisterRelayWorkers(context.Background(), registry))
	require.Equal(t, []netip.AddrPort{b.addr}, registry.addresses)
	require.NotContains(t, registry.addresses, a.addr)
	registry.err = errors.New("relay unavailable")
	require.ErrorIs(t, plane.RegisterRelayWorkers(context.Background(), registry), registry.err)
}

func (r *registryRecorder) Status(context.Context) (relay.RelayStatus, error) {
	return relay.RelayStatus{Instance: r.instance}, nil
}
func TestRelayRegistryFindsRejoinsAndLateRegistrationsOnSameInstance(t *testing.T) {
	plane, a, b, _ := setup(t)
	plane.mu.Lock()
	expireObservedWorker(t, plane.workers["a"])
	plane.workers["a"].observation.recoveryFinished(true)
	plane.mu.Unlock()
	remote := &registryRecorder{instance: "before"}
	var registry RelayWorkerRegistry
	ctx := context.Background()
	require.NoError(t, registry.Sync(ctx, plane, remote))
	require.Equal(t, []netip.AddrPort{b.addr}, remote.addresses)
	remote.instance = "after"
	remote.addresses = nil
	require.NoError(t, registry.Sync(ctx, plane, remote))
	require.Equal(t, []netip.AddrPort{b.addr}, remote.addresses)
	// Exercise the actual remote rejoin challenge/ACK path.
	reply, err := plane.remoteHeartbeat(context.Background(), "a", HeartbeatRequest{Address: a.addr})
	require.NoError(t, err)
	require.NotEmpty(t, reply.Token)
	_, err = plane.remoteHeartbeat(context.Background(), "a", HeartbeatRequest{Address: a.addr, Token: reply.Token})
	require.NoError(t, err)
	require.NoError(t, registry.Sync(ctx, plane, remote))
	require.ElementsMatch(t, []netip.AddrPort{a.addr, b.addr}, remote.addresses)
	require.NoError(t, registry.Sync(ctx, plane, remote))
	require.Len(t, remote.addresses, 2, "successful adds are not repeated on every tick")
	c := &fakeWorker{store: plane.store, addr: netip.MustParseAddrPort("127.0.0.1:3"), running: map[string]bool{}}
	require.NoError(t, plane.Register("late", c.addr, c))
	remote.err = errors.New("add failed")
	require.Error(t, registry.Sync(ctx, plane, remote))
	remote.err = nil
	require.NoError(t, registry.Sync(ctx, plane, remote))
	require.Equal(t, c.addr, remote.addresses[len(remote.addresses)-1])
}

func TestDrainedOrLostRelaySlotWaitsForNewInstance(t *testing.T) {
	for _, state := range []string{"draining", "lost"} {
		t.Run(state, func(t *testing.T) {
			ctx := t.Context()
			store := sessionstore.NewMemory()
			plane := New(&fakeRelay{}, store)
			configureRelayFixture(t, plane, relayTestSlots())
			slot := plane.relaySet["r1"].Slot
			worker, err := mediaworker.New(mediaworker.Config{Relay: &mediaworker.RelayConfig{Owners: store, Addr: slot.Leg, PublicAddr: slot.Public}})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, worker.Close()) })
			require.NoError(t, plane.Register("w", worker.LocalAddr(), worker))
			if state == "draining" {
				require.NoError(t, plane.DrainRelay(ctx, "r1"))
			} else {
				require.NoError(t, plane.loseRelay(ctx, "r1"))
			}
			remote := &registryRecorder{instance: "before"}
			registry := RelayWorkerRegistry{Slot: "r1"}
			for _, instance := range []string{"before", "after", "after"} {
				remote.instance = instance
				syncErr := registry.Sync(ctx, plane, remote)
				require.Error(t, syncErr, "status alone is not a leased relay admission")
				status, err := plane.Status(ctx)
				require.NoError(t, err)
				require.Equal(t, RelayStatus{Name: "r1", Draining: state == "draining", Lost: state == "lost", Pending: true, Error: syncErr.Error()}, status.Relays[0],
					"healthy polls and a new relay instance do not clear the mark")
			}
			// The marks are local to this control, rather than restored from the store.
			restarted := New(&fakeRelay{}, store)
			require.NoError(t, restarted.ConfigureRelaySet(ctx, relayTestSlots()))
			status, err := restarted.Status(ctx)
			require.NoError(t, err)
			require.Equal(t, []RelayStatus{{Name: "r1", Pending: true}, {Name: "r2", Pending: true}}, status.Relays)
			if state == "lost" {
				require.NoError(t, restarted.Register("w", worker.LocalAddr(), worker), "a pending worker can register without admitting the revoked slot")
				require.ErrorIs(t, worker.ConfigureRelays(ctx, mediaworker.RelaySetRequest{Slots: []mediaworker.RelaySlot{slot}, Epochs: map[string]uint64{"r1": 1}}), mediaworker.ErrRelayUnsupported, "worker cutoff still refuses the old epoch")
				status, err = restarted.Status(ctx)
				require.NoError(t, err)
				require.False(t, status.Ready, "registering without a joined pair cannot make control ready")
			}
		})
	}
}
