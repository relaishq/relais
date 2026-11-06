package inventory

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestComponentSlotRelocationAndAmbiguity(t *testing.T) {
	for _, slot := range Slots {
		hosts := map[string][]string{slot: {"signal", "control"}}
		got, ok := ComponentSlot(hosts, "signal")
		require.True(t, ok)
		require.Equal(t, slot, got)
	}
	for _, hosts := range []map[string][]string{nil, {"w1": {"signal"}, "w2": {"signal"}}, {"shared": {"signal", "signal"}}} {
		got, ok := ComponentSlot(hosts, "signal")
		require.False(t, ok)
		require.Empty(t, got)
	}
}
