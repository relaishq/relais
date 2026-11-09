package callharness

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/relaishq/relais/internal/operatorgates/observe"
	"github.com/stretchr/testify/require"
)

// This is synthetic caller evidence, not a rejudgment of the historical run.
// The first decoded frame's first return straddles the kill interval only with
// fixed bounds. Decoding after resume does not change that content identity.
func TestTakeover094msClockBoundary(t *testing.T) {
	raw, err := os.ReadFile("../../internal/operatorgates/clocks/testdata/bound-receipt.json")
	require.NoError(t, err)
	var selection clocks.ClockSelection
	require.NoError(t, json.Unmarshal(raw, &selection))
	bound, err := (clocks.CoverageEvidence{FrontendSlot: selection.Receipt.FrontendSlot, ControlSlot: selection.Receipt.ControlSlot}).WithBinding(clocks.ReceiptBinding{ReceiptSHA256: selection.SHA256, EvidenceSHA256: selection.Receipt.EvidenceSHA256, Bounds: selection.Receipt.Bounds}, selection.Receipt)
	require.NoError(t, err)
	require.True(t, bound.Binding.Valid(selection.SHA256))
	require.Equal(t, int64(120000), bound.Binding.Binding.Bounds.FrontendControlBoundNS)
	require.Equal(t, int64(170000), bound.Binding.Binding.Bounds.FrontendWorkerBoundNS)
	for _, side := range []struct {
		name  string
		delta time.Duration
		class string
	}{{"before", -940 * time.Microsecond, "cache"}, {"after", 940 * time.Microsecond, "outage media"}} {
		for _, policy := range []struct {
			name    string
			bounds  observe.ClockBounds
			trusted bool
		}{{"fixed", clocks.FixedCoverageBounds(), false}, {"receipt", bound.Binding.Binding.Bounds, true}} {
			t.Run(side.name+"/"+policy.name, func(t *testing.T) {
				r, _ := classificationFixture(t, 0)
				kill := 1500 * time.Millisecond
				tracker, err := NewClockTracker(policy.bounds.DriftPPM)
				require.NoError(t, err)
				at := r.start.Add(kill)
				require.NoError(t, tracker.Add(ClockProbe{CallerSend: at, CallerReceive: at, ServerReceive: at, ServerSend: at}))
				r.distributed.Clock = tracker
				control, worker := time.Duration(policy.bounds.FrontendControlBoundNS), time.Duration(policy.bounds.FrontendWorkerBoundNS)
				r.distributed.ControlOffset = OffsetInterval{Lower: -control, Upper: control}
				r.distributed.WorkerOffset = OffsetInterval{Lower: -worker, Upper: worker}
				first := &r.tracks[0].video.frames[0]
				first.source = sentVideoFrame{at: 1490 * time.Millisecond, returnedAt: kill + side.delta, written: true, identity: 100}
				require.Equal(t, side.class, r.eventMeasurement(testMeasurementMove(), r.hungUpAt, 0).FirstContent)
				m := r.measureMoves(r.hungUpAt, 1)[0].Measurement
				require.Equal(t, policy.trusted, m.FirstContentVerdict.Trusted, m.FirstContentVerdict.Reasons)
				require.False(t, m.FirstContentVerdict.Failed)
				cause := observe.TimingCause(strings.Join(m.FirstContentVerdict.Reasons, ";"))
				if !policy.trusted {
					require.Equal(t, "classification-varies-with-clock-interval", cause)
				} else {
					require.Equal(t, side.class, m.FirstContent)
					require.Empty(t, m.FirstContentVerdict.Reasons)
				}
			})
		}
	}
}
