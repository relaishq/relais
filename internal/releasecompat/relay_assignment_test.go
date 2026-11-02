package releasecompat

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRelayAssignmentContractKeepsLegacyStoreWriters(t *testing.T) {
	m, err := Generate("../..", "0.3.2", strings.Repeat("f", 40), "darwin", "arm64")
	require.NoError(t, err)
	require.Equal(t, 2, m.StoreContract)
	for role, profiles := range m.Roles {
		for name, profile := range profiles {
			if f, exists := profile.Formats["store_contract"]; exists {
				if name == "relay-change" {
					require.Equal(t, []int{2}, f.Writes)
					require.Contains(t, f.Continues, 2)
					continue
				}
				require.Equal(t, []int{1}, f.Writes, role+"/"+name)
				require.Equal(t, []int{1}, f.Continues, role+"/"+name)
				require.Contains(t, f.Reads, 2)
			}
		}
	}
}
