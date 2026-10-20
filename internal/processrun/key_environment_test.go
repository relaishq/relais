package processrun

import (
	"encoding/base64"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestObserverKeyConfigurationMatchesWorker(t *testing.T) {
	key := make([]byte, 32)
	old := make([]byte, 32)
	key[0] = 1
	old[0] = 2
	values := map[string]string{"RELAIS_SESSIONSTORE_KEY": base64.StdEncoding.EncodeToString(key), "RELAIS_SESSIONSTORE_KEY_ID": "7", "RELAIS_SESSIONSTORE_OLD_KEYS": `{"6":"` + hex.EncodeToString(old) + `"}`}
	for k, v := range values {
		t.Setenv(k, v)
	}
	active, opts, err := SessionStoreKeys(func(name string) string { return values[name] })
	require.NoError(t, err)
	require.Equal(t, key, active)
	require.EqualValues(t, 7, opts.KeyID)
	require.Equal(t, old, opts.OldKeys[6])
	actual, err := storeKey()
	require.NoError(t, err)
	require.Equal(t, active, actual)
}
