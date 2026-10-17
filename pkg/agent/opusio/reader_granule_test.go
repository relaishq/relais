package opusio

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDamagedPageGranuleBounds(t *testing.T) {
	good := []byte{0xf8, 0xff, 0xfe}
	for _, tc := range []struct {
		name      string
		final     bool
		granule   uint64
		wantError bool
	}{
		{"backward-nonfinal", false, 0, true},
		{"cropped-first-page", false, 960, true},
		{"valid-nonfinal-gap", false, 2880, false},
		{"backward-final", true, 960, true},
		{"valid-final-end", true, 1920, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			flags := byte(0)
			if tc.final {
				flags = 4
			}
			b := append(prefix(), page(2, flags, tc.granule, []byte{3, 3, 1}, append(append(bytes.Clone(good), good...), 0xfb))...)
			if !tc.final {
				b = append(b, page(3, 4, 3840, []byte{3}, good)...)
			}
			r, err := NewReader(bytes.NewReader(b))
			require.NoError(t, err)
			_, err = r.Next(context.Background())
			require.NoError(t, err)
			require.Equal(t, 13500*time.Microsecond, r.Position())
			_, err = r.Next(context.Background())
			require.NoError(t, err)
			require.Equal(t, 33500*time.Microsecond, r.Position())
			_, err = r.Next(context.Background())
			if tc.wantError {
				require.ErrorIs(t, err, ErrGranule)
			} else if !tc.final {
				require.NoError(t, err)
				require.GreaterOrEqual(t, r.Position(), 33500*time.Microsecond)
			}
		})
	}
}

func TestDamagedFinalPageStillAllowsLastPacketTrim(t *testing.T) {
	good := []byte{0xf8, 0xff, 0xfe}
	b := append(prefix(), page(2, 4, 1800, []byte{3, 1, 3}, append(append(append([]byte{}, good...), 0xfb), good...))...)
	r, err := NewReader(bytes.NewReader(b))
	require.NoError(t, err)
	_, err = r.Next(context.Background())
	require.NoError(t, err)
	_, err = r.Next(context.Background())
	require.NoError(t, err)
	require.Equal(t, 31*time.Millisecond, r.Position())
}
