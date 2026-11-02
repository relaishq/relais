package iceufrag

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGenerationUfragRoundTrip(t *testing.T) {
	for _, generation := range []uint64{0, 1, 20, 1 << 53, math.MaxUint64} {
		text := Format("a1B2c3D4e5F6g7H8", generation)
		id, got, ok := Parse(text)
		require.True(t, ok)
		require.Equal(t, "a1B2c3D4e5F6g7H8", id)
		require.Equal(t, generation, got)
		if generation == 0 {
			require.Equal(t, id, text)
		}
	}
}

func TestGenerationUfragRejectsAmbiguousSuffix(t *testing.T) {
	for _, text := range []string{"", "+1", "a+", "a+0", "a+01", "a+-1", "a++1", "a+1+2", "a+18446744073709551616", "a:b", "a+1:caller", "a+ 1"} {
		_, _, ok := Parse(text)
		require.False(t, ok, text)
	}
}
