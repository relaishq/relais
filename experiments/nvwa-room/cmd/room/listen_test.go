package main

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestListenerPrivateNamespaceValidation(t *testing.T) {
	require.NoError(t, validateListeners("", "127.0.0.1:38690", "127.0.0.1:38691"))
	require.NoError(t, validateListeners("10.189.80.0/24", "10.189.80.4:38690", "10.189.80.4:38691"))
	require.ErrorContains(t, validateListeners("", "10.189.80.4:38690"), "-spike-private-net")
	require.Error(t, validateListeners("10.189.80.0/24", "10.190.80.4:38690"))
	for _, prefix := range []string{"bad", "0.0.0.0/0", "10.0.0.0/7", "8.8.8.0/24", "fd00::/64"} {
		require.ErrorContains(t, validateListeners(prefix, "0.0.0.0:38690"), "RFC1918")
	}
	require.Error(t, validateListeners("10.189.80.0/24", "0.0.0.0:38690"))
}
