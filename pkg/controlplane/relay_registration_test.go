package controlplane

import (
	"context"
	"errors"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/require"
)

type registryRecorder struct {
	addresses []netip.AddrPort
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
