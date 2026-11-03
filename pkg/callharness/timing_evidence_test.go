package callharness

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNumericTimingMappingRetainsCompositionAndBindingProbes(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock, err := NewClockTracker(100)
	require.NoError(t, err)
	for i := range 20 {
		at := base.Add(time.Duration(i) * time.Second)
		require.NoError(t, clock.Add(ClockProbe{CallerSend: at, CallerReceive: at.Add(time.Millisecond), ServerReceive: at.Add(time.Millisecond / 2), ServerSend: at.Add(time.Millisecond / 2)}))
	}
	host := OffsetInterval{Lower: -2 * time.Millisecond, Upper: 2 * time.Millisecond}
	server := base.Add(20 * time.Second)
	plain, err := clock.Map(server, host)
	require.NoError(t, err)
	var evidence ClockMappingEvidence
	mapped, err := clock.mapEvent(server, host, &evidence)
	require.NoError(t, err)
	require.Equal(t, plain, mapped)
	require.Equal(t, mapped, evidence.Mapped)
	require.Len(t, evidence.Probes, 7) // first six plus the last, binding probe
	require.Equal(t, 13, evidence.OmittedProbes)
	require.Equal(t, 19, evidence.LowerProbe)
	require.Equal(t, 19, evidence.UpperProbe)
	for _, row := range evidence.Probes {
		require.Equal(t, time.Millisecond, row.ResidualWidth)
		require.Positive(t, row.Age.Lower)
		composed, err := row.Residual.Compose(evidence.Host)
		require.NoError(t, err)
		widened, err := composed.Widen(row.AnchorDistance, evidence.DriftPPM)
		require.NoError(t, err)
		require.Equal(t, row.Drift, composed.Lower-widened.Lower)
		if row.Index == evidence.LowerProbe {
			require.Equal(t, mapped.Earliest, widened.Map(server).Earliest)
			require.Equal(t, 5*time.Millisecond+2*row.Drift, mapped.Latest.Sub(mapped.Earliest))
		}
	}
}

func TestNumericTimingDoesNotChangeClassifierVerdictsOrReasons(t *testing.T) {
	for _, scenario := range []string{"pass", "fail", "uncertain", "label-boundary"} {
		t.Run(scenario, func(t *testing.T) {
			r, move := classificationFixture(t, 6*time.Millisecond)
			switch scenario {
			case "fail":
				delayFreshness(r, 3600*time.Millisecond)
			case "uncertain":
				delayFreshness(r, 3500*time.Millisecond)
			case "label-boundary":
				r.tracks[0].video.frames[0].source.requestedAt = 1500 * time.Millisecond
			}
			classify := func(retain bool) MoveReport {
				p := MoveReport{Kind: move.kind, Receipt: move.event, Distributed: r.distributedMeasurement(move, r.hungUpAt)}
				if !retain {
					p.Distributed.Timing = nil
				}
				r.selectDistributedPoint(&p)
				p.Recovery = r.videoRecovery(p.Start, r.hungUpAt)
				p.Measurement = r.eventMeasurement(p, r.hungUpAt, 0)
				normalizeEventFreshness(&p.Measurement, firstBaselinesOf(p.Measurement))
				r.classifyDistributedEvent(&p, r.hungUpAt, DurationInterval{}, nil, true)
				return p
			}
			without, with := classify(false), classify(true)
			requireCandidateLabelsMatchFrames(t, r, with)
			evidence := with.Distributed.Timing
			require.NotNil(t, evidence)
			require.NotEmpty(t, evidence.Start.Probes)
			require.NotEmpty(t, evidence.Candidates)
			require.NotEmpty(t, with.Distributed.Durations)
			if scenario == "pass" {
				for _, row := range evidence.Candidates {
					require.Equal(t, 6*time.Millisecond, row.StartRange.Upper-row.StartRange.Lower)
					require.Equal(t, 6*time.Millisecond, row.EndRange.Upper-row.EndRange.Lower)
				}
			}
			if scenario == "label-boundary" {
				labels := map[string]ClassificationCandidate{}
				for _, row := range evidence.Candidates {
					if row.Metric == "first_content" {
						labels[row.Label] = row
					}
				}
				require.Contains(t, labels, "keyframe")
				require.Contains(t, labels, "live")
				require.True(t, labels["keyframe"].RequestedAfterStart)
				require.False(t, labels["live"].RequestedAfterStart)
				require.True(t, labels["live"].Changed.KeyframeRequest)
				require.False(t, labels["live"].Changed.DecodedFrame)
				require.Equal(t, labels["keyframe"].FrameDecoded, labels["live"].FrameDecoded)
				require.Equal(t, 1500*time.Millisecond, labels["keyframe"].Requested)
			}
			with.Distributed.Timing = nil
			require.Equal(t, without, with, "all measurements, verdicts and reasons must remain identical")
		})
	}
}

func TestNumericTimingRetainsPlannedSpanAllowance(t *testing.T) {
	r, move := classificationFixture(t, time.Millisecond)
	move.kind, move.event.Kind = "move", "move"
	r.moves = []moveRecord{move}
	r.distributed.ControlOffset = OffsetInterval{Lower: -2 * time.Millisecond, Upper: 2 * time.Millisecond}
	got := r.measureMoves(r.hungUpAt, 1)[0].Distributed.Timing
	require.NotNil(t, got.PlannedSpan)
	require.Equal(t, 4*time.Millisecond, got.PlannedSpan.HostAllowance)
	require.Equal(t, DurationInterval{Lower: 296 * time.Millisecond, Upper: 304 * time.Millisecond}, got.PlannedSpan.Bounds)
}

func requireCandidateLabelsMatchFrames(t *testing.T, r *recorder, report MoveReport) {
	t.Helper()
	for _, row := range report.Distributed.Timing.Candidates {
		if !row.FramePresent {
			require.Empty(t, row.Label, "a named candidate must retain its selected frame")
			continue
		}
		point := MoveReport{Kind: report.Kind, Start: row.Start, End: row.End}
		if point.Kind == "takeover" {
			point.Recovery = r.classificationRecovery(point.Start, r.hungUpAt)
		}
		measurement := r.eventMeasurement(point, r.hungUpAt, row.BaselineFloor)
		found := false
		for _, track := range r.tracks {
			if track.video == nil {
				continue
			}
			for _, frame := range track.video.frames {
				if frame.at == row.FrameDecoded && frame.source.at == row.FrameSent {
					found = true
					require.Equal(t, classifyContent(frame, row.Start, measurement.WindowStart, row.Resume, measurement.Video), row.Label, "retained witness differs from classifier")
				}
			}
		}
		require.True(t, found, "retained frame must exist in the fixture")
		require.LessOrEqual(t, row.StartRange.Lower, row.Start)
		require.GreaterOrEqual(t, row.StartRange.Upper, row.Start)
		require.LessOrEqual(t, row.EndRange.Lower, row.End)
		require.GreaterOrEqual(t, row.EndRange.Upper, row.End)
	}
}

func TestNumericTimingCandidateWitnessesUseClassifierLabels(t *testing.T) {
	for _, label := range []string{"cache", "keyframe", "outage media", "natural keyframe", "live"} {
		t.Run(label, func(t *testing.T) {
			r, _ := classificationFixture(t, 6*time.Millisecond)
			frame := &r.tracks[0].video.frames[0]
			switch label {
			case "cache":
				frame.source.at, frame.source.returnedAt, frame.source.pli = time.Second, time.Second+time.Millisecond, false
			case "outage media":
				frame.source.at, frame.source.pli = 1600*time.Millisecond, false
			case "natural keyframe":
				frame.source.pli, frame.restartsDecodeChain = false, true
			case "live":
				frame.source.pli = false
			}
			report := r.measureMoves(r.hungUpAt, 1)[0]
			requireCandidateLabelsMatchFrames(t, r, report)
			found := false
			for _, row := range report.Distributed.Timing.Candidates {
				if row.Metric == "first_content" && row.Label == label {
					found = true
				}
			}
			require.True(t, found, "fixture must exercise its named class")
		})
	}
}

func TestNumericTimingMappingFailureHasNoBindingProbe(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, scenario := range []string{"missing", "invalidated", "host", "zero-event", "disagreement"} {
		t.Run(scenario, func(t *testing.T) {
			clock, err := NewClockTracker(0)
			require.NoError(t, err)
			server, host := base.Add(time.Second), OffsetInterval{}
			if scenario != "missing" {
				require.NoError(t, clock.Add(ClockProbe{CallerSend: base, CallerReceive: base, ServerReceive: base, ServerSend: base}))
			}
			switch scenario {
			case "invalidated":
				clock.Invalidate(errors.New("invalid-clock"))
			case "host":
				host = OffsetInterval{Lower: time.Second}
			case "zero-event":
				server = time.Time{}
			case "disagreement":
				clock.probes = append(clock.probes, ClockProbe{CallerSend: base.Add(time.Second), CallerReceive: base.Add(time.Second), ServerReceive: base.Add(2 * time.Second), ServerSend: base.Add(2 * time.Second)})
			}
			var evidence ClockMappingEvidence
			_, plainErr := clock.Map(server, host)
			_, err = clock.mapEvent(server, host, &evidence)
			require.EqualError(t, err, plainErr.Error())
			require.Equal(t, -1, evidence.LowerProbe)
			require.Equal(t, -1, evidence.UpperProbe)
			require.Equal(t, len(clock.probes), evidence.OmittedProbes+len(evidence.Probes))
			require.Zero(t, evidence.Mapped)
		})
	}
}
