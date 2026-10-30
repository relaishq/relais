package relay

import (
	"testing"

	"github.com/pion/stun/v4"
	"github.com/stretchr/testify/require"
)

func TestBindingRequestGenerationRoutesSession(t *testing.T) {
	for _, ufrag := range []string{"a1B2c3D4e5F6g7H8", "a1B2c3D4e5F6g7H8+1", "a1B2c3D4e5F6g7H8+18446744073709551615"} {
		m, err := stun.Build(stun.BindingRequest, stun.TransactionID, stun.NewUsername(ufrag+":caller"))
		require.NoError(t, err)
		id, tx, ok := parseBindingRequest(m.Raw)
		require.True(t, ok)
		require.Equal(t, "a1B2c3D4e5F6g7H8", id)
		require.Equal(t, m.TransactionID, tx)
	}
}
