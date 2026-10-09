package workerprobe

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOverlappingEnableScopes(t *testing.T) {
	a := netip.MustParseAddrPort("127.0.0.1:10001")
	b := netip.MustParseAddrPort("127.0.0.1:10002")
	capture := func(id string) (Sender, error) {
		return func(packet []byte) ([]byte, error) { return append([]byte(id), packet...), nil }, nil
	}
	Register(a, capture)
	_, err := Zombie(a, "a")
	require.Error(t, err, "probe is inert by default")
	first := Enable()
	t.Cleanup(first)
	Register(a, capture)
	second := Enable()
	t.Cleanup(second)
	Register(b, capture)
	_, err = Zombie(a, "a")
	require.NoError(t, err, "second enable preserves first registration")
	first()
	first()
	_, err = Zombie(b, "b")
	require.NoError(t, err, "idempotent first cleanup leaves second scope active")
	Remove(a)
	_, err = Zombie(a, "a")
	require.Error(t, err)
	_, err = Zombie(b, "b")
	require.NoError(t, err, "removing A leaves B")
	second()
	_, err = Zombie(b, "b")
	require.Error(t, err, "last cleanup disables registry")
}

func TestBarrierSuppressionScopes(t *testing.T) {
	addr := netip.MustParseAddrPort("127.0.0.1:10003")
	require.False(t, BarrierIgnored(addr))
	_, err := IgnoreBarriers(addr)
	require.Error(t, err, "cannot suppress a production worker")
	disable := Enable()
	t.Cleanup(disable)
	Register(addr, func(string) (Sender, error) { return nil, nil })
	first, err := IgnoreBarriers(addr)
	require.NoError(t, err)
	t.Cleanup(first)
	second, err := IgnoreBarriers(addr)
	require.NoError(t, err)
	t.Cleanup(second)
	require.True(t, BarrierIgnored(addr))
	first()
	first()
	require.True(t, BarrierIgnored(addr), "one remaining suppression scope")
	second()
	require.False(t, BarrierIgnored(addr))
	Remove(addr)
	require.False(t, BarrierIgnored(addr), "closed workers leave no hook")
}
