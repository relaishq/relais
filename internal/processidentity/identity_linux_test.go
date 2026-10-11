package processidentity

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestParseStatCommParentheses(t *testing.T) {
	start, dead, err := parseStat("123 (worker (test)) Z 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 123456")
	require.NoError(t, err)
	require.Equal(t, "123456", start)
	require.True(t, dead)
	_, _, err = parseStat("123 broken")
	require.Error(t, err)
}
