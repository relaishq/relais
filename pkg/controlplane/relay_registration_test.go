package controlplane

import (
	"context"
	"errors"
	"net/netip"
	"testing"

	"github.com/relais/pkg/relay"
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
	plane.workers["a"].dead = true
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
	plane.workers["a"].dead = true
	plane.workers["a"].recovered = true
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
	reply, err := plane.remoteHeartbeat("a", HeartbeatRequest{Address: a.addr})
	require.NoError(t, err)
	require.NotEmpty(t, reply.Token)
	_, err = plane.remoteHeartbeat("a", HeartbeatRequest{Address: a.addr, Token: reply.Token})
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
