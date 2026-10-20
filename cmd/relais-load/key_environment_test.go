package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRelayEnvironmentStripsSessionKeyRing(t *testing.T) {
	env := []string{"RELAIS_SESSIONSTORE_KEY=secret", "RELAIS_SESSIONSTORE_KEY_ID=7", "RELAIS_SESSIONSTORE_OLD_KEYS=old-secret", "RELAIS_REDIS_ADDR=owned"}
	require.Equal(t, []string{"RELAIS_REDIS_ADDR=owned"}, withoutSessionKeys(env))
}
