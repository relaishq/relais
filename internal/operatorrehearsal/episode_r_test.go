package operatorrehearsal

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/relaishq/relais/internal/operatorevidence/relayjudge"
	"github.com/relaishq/relais/internal/operatorgates/observe"
	"github.com/stretchr/testify/require"
)

func TestPartRReportEpisodeCounts(t *testing.T) {
	r := passingPartRReport()
	for i := range r.Gates[0].Process.RelayLoss.Calls {
		episodes, restarts := 2, 1
		if i >= 2 {
			episodes, restarts = 0, 0
		}
		c := &r.Gates[0].Process.RelayLoss.Calls[i]
		c.EpisodeCount, c.ICERestarts = &episodes, &restarts
	}
	r.Gates[0].Process.RelayLoss.Calls[0].Causes = []string{"relay-loss-ice-restart-missing", "private-host"}
	var out bytes.Buffer
	require.NoError(t, r.Write(&out))
	var saved Report
	require.NoError(t, json.Unmarshal(out.Bytes(), &saved))
	calls := saved.Gates[0].Process.RelayLoss.Calls
	require.NotNil(t, calls[0].EpisodeCount)
	require.NotNil(t, calls[0].ICERestarts)
	require.NotNil(t, calls[2].EpisodeCount)
	require.NotNil(t, calls[2].ICERestarts)
	require.Equal(t, 2, *calls[0].EpisodeCount)
	require.Equal(t, 1, *calls[0].ICERestarts)
	require.Equal(t, 0, *calls[2].EpisodeCount)
	require.Equal(t, 0, *calls[2].ICERestarts)
	require.Contains(t, calls[0].Causes, "relay-loss-ice-restart-missing")
	require.NotContains(t, out.String(), "private-host")
	for _, bad := range []int{-1, int(observe.MaxInteger + 1)} {
		got := publicRelayLoss(relayjudge.Result{Calls: []relayjudge.CallResult{{EpisodeCount: &bad, ICERestarts: &bad}}})
		require.Nil(t, got.Calls[0].EpisodeCount)
		require.Nil(t, got.Calls[0].ICERestarts)
	}
}
