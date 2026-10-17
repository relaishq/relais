package mediaworker

import (
	"encoding/json"
	"math"
	"testing"

	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/stretchr/testify/require"
)

func TestControlPackedRefusalsRoundTrip(t *testing.T) {
	refs := []taskRefusal{{ID: deriveTaskID("s", "n", "e", "a")}, {ID: deriveTaskID("s", "n", "e", "b"), Turn: 7}, {ID: deriveTaskID("s", "n", "e", "c"), Turn: agent.Turn(math.MaxUint64)}}
	for _, high := range []uint64{0, 1} {
		original := v2Envelope{ControlHigh: high, Capacity: refs}
		encoded, err := json.Marshal(original)
		require.NoError(t, err)
		var restored v2Envelope
		require.NoError(t, json.Unmarshal(encoded, &restored))
		require.Equal(t, original, restored)
	}
	packed, ok := packTaskRefusals(refs)
	require.True(t, ok)
	for i := range len(packed) {
		_, err := unpackTaskRefusals(packed[:i])
		require.Error(t, err)
	}
	corrupt := append([]byte{}, packed...)
	corrupt[0] = 2
	_, err := unpackTaskRefusals(corrupt)
	require.Error(t, err)
	_, err = unpackTaskRefusals(append(append([]byte{}, packed...), 0))
	require.Error(t, err)
}

func FuzzPackedTaskRefusals(f *testing.F) {
	f.Add([]byte{1, 0})
	packed, _ := packTaskRefusals([]taskRefusal{{ID: deriveTaskID("s", "n", "e", "a")}, {ID: deriveTaskID("s", "n", "e", "b"), Turn: math.MaxUint64}})
	f.Add(packed)
	f.Fuzz(func(t *testing.T, data []byte) {
		refs, err := unpackTaskRefusals(data)
		if err != nil {
			return
		}
		canonical, ok := packTaskRefusals(refs)
		require.True(t, ok)
		require.Equal(t, data, canonical)
	})
}
