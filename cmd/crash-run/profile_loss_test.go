package main

import (
	"math"
	"testing"
	"time"

	"github.com/relais/pkg/callharness"
	"github.com/stretchr/testify/require"
)

func matchingNetworkLossRows() []profileRow {
	rows := goodProfileRows()
	for i := range rows {
		rows[i].Measurement.LostAudio = 20 * time.Millisecond
		rows[i].Measurement.LostVideoFrames = 1
		rows[i].Measurement.AudioLoss.Failed = true
		rows[i].Measurement.AudioLoss.Reasons = []string{"sent audio never returned"}
		rows[i].Measurement.VideoLoss.Failed = true
		rows[i].Measurement.VideoLoss.Reasons = []string{"sent video never returned complete"}
		rows[i].Measurement.ContentLoss.Event.Audio.Lost = 1
		rows[i].Measurement.ContentLoss.Event.Video.Lost = 1
		for j := range rows[i].Measurement.ContentLoss.Baselines {
			rows[i].Measurement.ContentLoss.Baselines[j].Audio.Lost = 1
			rows[i].Measurement.ContentLoss.Baselines[j].Video.Lost = 1
		}
	}
	return rows
}

func TestProfileTakeoverLossMatchesNetworkBaseline(t *testing.T) {
	rows := matchingNetworkLossRows()
	for _, name := range []string{"region", "far-caller"} {
		r := judgeProfile(profileTrialOptions{Name: name, Scenario: "takeover"}, rows, nil)
		require.Equal(t, "pass", r.Verdict, r.Reasons)
	}
}

func TestContentLossPredictionBound(t *testing.T) {
	counts := func(sent, lost int) callharness.ContentLossCounts {
		return callharness.ContentLossCounts{Sent: sent, Lost: lost, Trusted: true}
	}
	for _, tc := range []struct {
		name            string
		event, baseline callharness.ContentLossCounts
		windows         int
		verdict         string
	}{
		{"matching", counts(500, 10), counts(1500, 30), 30, "pass"},
		{"excess", counts(500, 100), counts(1500, 30), 30, "fail"},
		{"small baseline", counts(500, 10), counts(50, 1), 10, "inconclusive"},
		{"few windows", counts(500, 10), counts(1500, 30), 1, "inconclusive"},
		{"zero baseline with random loss", counts(500, 1), counts(1500, 0), 30, "pass"},
		{"zero baseline with takeover loss", counts(500, 100), counts(1500, 0), 30, "fail"},
		{"no event evidence", counts(0, 0), counts(1500, 30), 30, "inconclusive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := judgeContentLoss(tc.event, tc.baseline, tc.windows, true)
			require.Equal(t, tc.verdict, r.Verdict)
			if tc.verdict == "pass" {
				require.Positive(t, r.BaselineUpperRate)
				require.GreaterOrEqual(t, r.UpperBound, tc.event.Lost)
			}
		})
	}
	r := judgeContentLoss(counts(500, 1), counts(1500, 30), 30, false)
	require.Equal(t, "fail", r.Verdict, "clean caller links still require exact zero")
}

func TestExactBinomialCalculationAndBoundBoundary(t *testing.T) {
	require.InDelta(t, .75, binomialCDF(1, 2, .5), 1e-14)
	require.Zero(t, binomialCDF(-1, 2, .5))
	require.Equal(t, 1.0, binomialCDF(2, 2, 1))
	require.Zero(t, binomialCDF(1, 2, 1))
	// The zero-loss Clopper-Pearson limit has this closed form.
	require.InDelta(t, 1-math.Pow(contentLossTailAlpha, 1.0/1500), binomialUpperRate(0, 1500, contentLossTailAlpha), 1e-12)
	p := binomialUpperRate(30, 1500, contentLossTailAlpha)
	require.InDelta(t, contentLossTailAlpha, binomialCDF(30, 1500, p), 1e-12)
	bound := binomialUpperCount(500, p, contentLossTailAlpha)
	require.GreaterOrEqual(t, binomialCDF(bound, 500, p), 1-contentLossTailAlpha)
	require.Less(t, binomialCDF(bound-1, 500, p), 1-contentLossTailAlpha)
	baseline := callharness.ContentLossCounts{Sent: 1500, Lost: 30, Trusted: true}
	require.Equal(t, "pass", judgeContentLoss(callharness.ContentLossCounts{Sent: 500, Lost: bound, Trusted: true}, baseline, 30, true).Verdict)
	require.Equal(t, "fail", judgeContentLoss(callharness.ContentLossCounts{Sent: 500, Lost: bound + 1, Trusted: true}, baseline, 30, true).Verdict)
}

func TestProfileTakeoverPooledLossFailsAndCannotPassMissingEvidence(t *testing.T) {
	options := profileTrialOptions{Name: "region", Scenario: "takeover"}
	for _, tc := range []struct {
		name    string
		mutate  func([]profileRow)
		verdict string
	}{
		{"excess audio", func(rows []profileRow) {
			for i := range rows {
				rows[i].Measurement.ContentLoss.Event.Audio.Lost = 10
				rows[i].Measurement.LostAudio = 200 * time.Millisecond
			}
		}, "fail"},
		{"excess video", func(rows []profileRow) {
			for i := range rows {
				rows[i].Measurement.ContentLoss.Event.Video.Lost = 10
				rows[i].Measurement.LostVideoFrames = 10
			}
		}, "fail"},
		{"small baseline", func(rows []profileRow) {
			for i := range rows {
				rows[i].Measurement.ContentLoss.Baselines = rows[i].Measurement.ContentLoss.Baselines[:1]
				rows[i].Measurement.ContentLoss.Baselines[0].Audio.Sent = 5
			}
		}, "inconclusive"},
		{"missing baseline", func(rows []profileRow) { rows[0].Measurement.ContentLoss.Baselines = nil }, "inconclusive"},
		{"untrusted event", func(rows []profileRow) { rows[0].Measurement.ContentLoss.Event.Audio.Trusted = false }, "inconclusive"},
		{"identity failure", func(rows []profileRow) {
			rows[0].Measurement.VideoLoss.Reasons = append(rows[0].Measurement.VideoLoss.Reasons, "conflicting video content identities")
		}, "fail"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows := matchingNetworkLossRows()
			tc.mutate(rows)
			r := judgeProfile(options, rows, nil)
			require.Equal(t, tc.verdict, r.Verdict, r.Reasons)
		})
	}
	r := judgeProfile(options, matchingNetworkLossRows(), nil)
	require.Equal(t, "pooled run", r.AudioContentLoss.Scope)
	require.Equal(t, 500, r.AudioContentLoss.Event.Sent)
	require.Equal(t, 10, r.AudioContentLoss.Event.Lost)
	require.Equal(t, 1500, r.AudioContentLoss.Baseline.Sent)
	require.Equal(t, 30, r.AudioContentLoss.Baseline.Lost)
	require.Equal(t, 30, r.AudioContentLoss.BaselineWindows)
	require.Equal(t, 10.0, r.AudioContentLoss.ExpectedCount)
	require.Equal(t, contentLossConfidence, r.AudioContentLoss.Confidence)
	for _, row := range r.Rows {
		require.Equal(t, r.AudioContentLoss, row.AudioContentLoss)
		require.Equal(t, 50, row.Measurement.ContentLoss.Event.Audio.Sent)
		require.Equal(t, 1, row.Measurement.ContentLoss.Event.Audio.Lost)
	}
	// Moves do not currently gate on loss; their raw attribution is the same,
	// and changing the short move window cannot reinstate a zero-loss gate.
	options.Scenario = "move"
	r = judgeProfile(options, matchingNetworkLossRows(), nil)
	require.Equal(t, "pass", r.Verdict, r.Reasons)
	require.Equal(t, 10, r.AudioContentLoss.Event.Lost)
}
