package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestChildEnvironmentStripsSessionKeyRing(t *testing.T) {
	t.Setenv("RELAIS_SESSIONSTORE_KEY_ID", "7")
	t.Setenv("RELAIS_SESSIONSTORE_OLD_KEYS", `{"6":"secret"}`)
	for _, withKey := range []bool{false, true} {
		env := processEnv("owned", "127.0.0.1:16379", "owned:", withKey)
		require.NotContains(t, env, "RELAIS_SESSIONSTORE_KEY_ID=7")
		require.NotContains(t, env, `RELAIS_SESSIONSTORE_OLD_KEYS={"6":"secret"}`)
	}
}
