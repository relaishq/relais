package operatorrehearsal

import (
	"bytes"
	"encoding/json"
	"math"
	"testing"

	"github.com/relaishq/relais/internal/operatorevidence/relayjudge"
	"github.com/stretchr/testify/require"
)

func TestPartRReportGeneratorEvidence(t *testing.T) {
	r := passingPartRReport()
	want := relayjudge.GeneratorCheck{Rule: "generator-max-ns-max", Observed: 501000000, Limit: 500000000, Count: 2, EventOverlaps: 1}
	r.Gates[0].Process.RelayLoss.Generator = &relayjudge.GeneratorEvidence{Samples: 30, InlineSamples: 30, MediaNS: 29000000000, FlaggedNS: 1000000000, FlaggedPercent: 100.0 / 29, Checks: []relayjudge.GeneratorCheck{want,
		{Rule: "session-10.42.0.10", Observed: 1},
		{Rule: "generator-p99-ns-max", Observed: math.Inf(1)},
		{Rule: "generator-p99-ns-max", Observed: -1},
	}}
	var out bytes.Buffer
	require.NoError(t, r.Write(&out))
	var saved Report
	require.NoError(t, json.Unmarshal(out.Bytes(), &saved))
	g := saved.Gates[0].Process.RelayLoss.Generator
	require.NotNil(t, g)
	require.Equal(t, 30, g.Samples)
	require.Equal(t, int64(29000000000), g.MediaNS)
	require.Equal(t, int64(1000000000), g.FlaggedNS)
	require.Equal(t, 100.0/29, g.FlaggedPercent)
	require.Equal(t, []relayjudge.GeneratorCheck{want}, g.Checks)
	require.NotContains(t, out.String(), "10.42.0.10")
	// A malformed summary cannot prevent report publication or disclose a raw
	// diagnostic through a fallback path.
	r.Gates[0].Process.RelayLoss.Generator.FlaggedPercent = math.NaN()
	out.Reset()
	require.NoError(t, r.Write(&out))
	saved = Report{}
	require.NoError(t, json.Unmarshal(out.Bytes(), &saved))
	require.Nil(t, saved.Gates[0].Process.RelayLoss.Generator)
}
