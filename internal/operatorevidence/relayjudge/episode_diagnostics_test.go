package relayjudge

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRelayLossEpisodeDiagnostics(t *testing.T) {
	for _, row := range []struct {
		name, cause        string
		episodes, restarts int
	}{
		{"second-applied-restart", "relay-loss-episode-count", 2, 1},
		{"no-episode", "relay-loss-episode-count", 0, 1},
		{"restart-counter-zero", "relay-loss-ice-restart-missing", 1, 0},
	} {
		t.Run(row.name, func(t *testing.T) {
			in, ms := fixture(t)
			if row.episodes == 0 {
				ms[0].Episodes = nil
			}
			if row.episodes == 2 {
				ms[0].Episodes = append(ms[0].Episodes, ms[0].Episodes[0])
			}
			ms[0].Call.ICERestarts = ptr(row.restarts)
			result := Judge(wire(t, in, ms))
			require.Equal(t, []string{row.cause}, result.Calls[0].Causes)
			raw, err := json.Marshal(result)
			require.NoError(t, err)
			var saved struct {
				Calls []struct{ EpisodeCount, ICERestarts *int }
			}
			require.NoError(t, json.Unmarshal(raw, &saved))
			require.NotNil(t, saved.Calls[0].EpisodeCount)
			require.NotNil(t, saved.Calls[0].ICERestarts)
			require.Equal(t, row.episodes, *saved.Calls[0].EpisodeCount)
			require.Equal(t, row.restarts, *saved.Calls[0].ICERestarts)
			require.Equal(t, 0, *saved.Calls[2].EpisodeCount)
			require.Equal(t, 0, *saved.Calls[2].ICERestarts)
		})
	}
}

func TestRelayLossMalformedRecordDoesNotInventZeroCounts(t *testing.T) {
	in, ms := fixture(t)
	in = wire(t, in, ms)
	in.CallerJSONL = strings.Replace(in.CallerJSONL, `"ICERestarts":1`, `"ICERestarts":0.5`, 1)
	result := Judge(in)
	require.Contains(t, result.Calls[0].Causes, "relay-loss-missing")
	require.Nil(t, result.Calls[0].ICERestarts, "a partially decoded fractional counter must not become an observed zero")
	require.Nil(t, result.Calls[0].EpisodeCount, "partially decoded records do not supply count diagnostics")
	require.Equal(t, 1, *result.Calls[1].EpisodeCount, "independent complete records keep their evidence")
}
