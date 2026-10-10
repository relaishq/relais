package callharness

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestExternalWorkerIndicesMatchRelayPlacement(t *testing.T) {
	h := &Harness{external: &ExternalTopology{}, signalingURL: "http://127.0.0.1:10000/calls"}
	for _, index := range []int{-1, 0, 1} {
		endpoint, err := h.offerURL(index)
		require.NoError(t, err)
		parsed, err := url.Parse(endpoint)
		require.NoError(t, err)
		expected := ""
		if index == 0 {
			expected = "0"
		}
		if index == 1 {
			expected = "1"
		}
		require.Equal(t, expected, parsed.Query().Get("worker"))
	}
	_, err := h.offerURL(-2)
	require.Error(t, err)
}
