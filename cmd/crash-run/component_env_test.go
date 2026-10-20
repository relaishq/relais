package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProcessEnvironmentIsolatesKeyRing(t *testing.T) {
	t.Setenv("RELAIS_SESSIONSTORE_KEY_ID", "12")
	t.Setenv("RELAIS_SESSIONSTORE_OLD_KEYS", `{"11":"private-decrypt-key"}`)
	env := processEnv("owned-key", "127.0.0.1:16379", "owned:")
	require.Empty(t, envValue(env, "RELAIS_SESSIONSTORE_KEY_ID"))
	require.Empty(t, envValue(env, "RELAIS_SESSIONSTORE_OLD_KEYS"))
	require.Equal(t, "owned-key", envValue(env, "RELAIS_SESSIONSTORE_KEY"))
	relay := withoutKey(append(env, "RELAIS_SESSIONSTORE_KEY_ID=12", "RELAIS_SESSIONSTORE_OLD_KEYS=private-decrypt-key"))
	for _, name := range []string{"RELAIS_SESSIONSTORE_KEY", "RELAIS_SESSIONSTORE_KEY_ID", "RELAIS_SESSIONSTORE_OLD_KEYS"} {
		require.Empty(t, envValue(relay, name))
	}
}
