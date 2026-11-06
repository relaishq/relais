package clockprobe

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTargetsValidateFixedPrivateOrigins(t *testing.T) {
	var targets Targets
	require.Empty(t, targets.String())
	require.NoError(t, targets.Validate(false, ""))
	for _, value := range []string{"w2=https://[::1]:1234/", "control=http://127.0.0.1", "w1=http://10.0.0.1:1234"} {
		require.NoError(t, targets.Set(value))
	}
	require.NoError(t, targets.Validate(true, "probes"))
	require.Equal(t, "w2=https://[::1]:1234,control=http://127.0.0.1,w1=http://10.0.0.1:1234", targets.String())
	require.Error(t, targets.Set("control=http://127.0.0.2"))
	require.Error(t, targets.Validate(false, "probes"))
	require.Error(t, targets.Validate(true, ""))
	for _, value := range []string{"unknown=http://127.0.0.1", "control", "control=", "control=http://example.test", "control=http://203.0.113.1", "control=http://0.0.0.0", "control=ftp://127.0.0.1", "control=http://user:secret@127.0.0.1", "control=http://127.0.0.1/path", "control=http://127.0.0.1/%2f", "control=http://127.0.0.1?", "control=http://127.0.0.1?a=b", "control=http://127.0.0.1#", "control=http://127.0.0.1#fragment", "control=http://127.0.0.1:0", "control=http://127.0.0.1:65536", "control=http://127.0.0.1:", "control=http://[fe80::1%25lo0]:80"} {
		t.Run(value, func(t *testing.T) {
			var invalid Targets
			assertErr := invalid.Set(value)
			require.Error(t, assertErr)
			require.Empty(t, invalid)
		})
	}
}
