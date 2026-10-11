package processrun

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestPrivateListenerAllowList(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:0", "[::1]:0", "[::ffff:127.0.0.1]:0"} {
		require.NoError(t, ValidatePrivateListener(addr, ""))
	}
	for _, addr := range []string{"10.130.4.2:8080", "[fd12::2]:8080"} {
		require.Error(t, ValidatePrivateListener(addr, ""))
	}
	allowed := "10.130.4.0/24, fd12::/64"
	for _, addr := range []string{"10.130.4.2:8080", "[fd12::2]:8080", "127.0.0.1:0"} {
		require.NoError(t, ValidatePrivateListener(addr, allowed))
	}
	for _, addr := range []string{"10.130.5.2:8080", "192.168.1.2:8080", "8.8.8.8:8080", "0.0.0.0:0", "[::]:0", ":0", "localhost:0", "[fd12::2%eth0]:0"} {
		require.Error(t, ValidatePrivateListener(addr, allowed), addr)
	}
	for _, bad := range []string{"0.0.0.0/0", "10.0.0.0/7", "172.16.0.0/11", "192.168.0.0/15", "fc00::/6", "8.8.8.8/32", "::/0", "not-a-prefix", "10.1.0.0/24,"} {
		require.Error(t, ValidatePrivateListener("127.0.0.1:0", bad), bad)
	}
	t.Setenv("RELAIS_PRIVATE_NETS", "10.130.4.0/24")
	listener, err := ListenPrivate("127.0.0.1:0")
	require.NoError(t, err)
	require.NoError(t, listener.Close())
	// Reserved Redis addresses remain rejected even with private listeners enabled.
	require.Error(t, ValidateRedis("10.130.4.4:6379"))
	require.NoError(t, ValidateRedis("10.130.4.4:16379"))
}
