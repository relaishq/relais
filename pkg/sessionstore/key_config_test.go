package sessionstore

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestComponentKeyEnvironment(t *testing.T) {
	for _, test := range []struct {
		name, id, old string
		valid         bool
	}{
		{"default", "", "", true}, {"ring", "7", `{"0":"` + hex.EncodeToString(make([]byte, 32)) + `","255":"` + base64.StdEncoding.EncodeToString(make([]byte, 32)) + `"}`, true},
		{"id-bound", "256", "", false}, {"negative", "-1", "", false}, {"active-duplicate", "1", `{"1":"` + strings.Repeat("00", 32) + `"}`, false},
		{"malformed", "0", "secret-not-json", false}, {"null", "0", "null", false}, {"old-bound", "1", `{"256":"` + strings.Repeat("00", 32) + `"}`, false}, {"old-secret", "1", `{"0":"do-not-echo-this-secret"}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("RELAIS_SESSIONSTORE_KEY_ID", test.id)
			t.Setenv("RELAIS_SESSIONSTORE_OLD_KEYS", test.old)
			opts, err := RedisKeyOptionsFromEnv()
			if test.valid {
				require.NoError(t, err)
				if test.name == "ring" {
					require.EqualValues(t, 7, opts.KeyID)
					require.Len(t, opts.OldKeys, 2)
				}
			} else {
				require.Error(t, err)
				require.NotContains(t, err.Error(), "do-not-echo-this-secret")
			}
		})
	}
}
func TestComponentReadCancelledContext(t *testing.T) {
	forComponentStores(t, func(t *testing.T, s Store) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		cs := s.(ComponentStore)
		_, err := cs.ReadComponents(ctx, "cancelled")
		require.ErrorIs(t, err, context.Canceled)
		_, err = cs.ComponentKeyInventory(ctx)
		require.ErrorIs(t, err, context.Canceled)
	})
}
func TestComponentClockRefusesBackwardTime(t *testing.T) {
	_, err := checkpointAt(time.Unix(10, 0), time.Unix(9, 0))
	require.ErrorIs(t, err, ErrUnsafeCheckpointClock)
}
