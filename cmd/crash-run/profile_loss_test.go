package main

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/relais/internal/nettopology"
	"github.com/relais/pkg/callharness"
	"github.com/stretchr/testify/require"
)

func profileTestLinks(t *testing.T, name string) []nettopology.LinkSetting {
	t.Helper()
	plan, err := nettopology.NewPlan(0, 1, "aabbccddee", 2)
	require.NoError(t, err)
	links, err := plan.ProfilePlan(name)
	require.NoError(t, err)
	return recordedProfileLinks(links)
}

func matchingNetworkLossRows(trials int) []profileRow {
	rows := goodProfileRows(trials)
	rows[0].BarrierResends, rows[0].InjectedDiscardedReplies = 1, 1
	for i := range rows {
		rows[i].Measurement.LostAudio = 20 * time.Millisecond
		rows[i].Measurement.LostVideoFrames = 1
		rows[i].Measurement.AudioLoss.Failed = true
		rows[i].Measurement.AudioLoss.Reasons = []string{"sent audio never returned"}
		rows[i].Measurement.VideoLoss.Failed = true
		rows[i].Measurement.VideoLoss.Reasons = []string{"sent video never returned complete"}
		rows[i].Measurement.ContentLoss.Event.Audio.Lost = 1
		rows[i].Measurement.ContentLoss.Event.Audio.LongestLostRun = 1
		rows[i].Measurement.ContentLoss.Event.Video.Lost = 1
		rows[i].Measurement.ContentLoss.Event.Video.LongestLostRun = 1
		for j := range rows[i].Measurement.ContentLoss.Baselines {
			rows[i].Measurement.ContentLoss.Baselines[j].Audio.Lost = 1
			rows[i].Measurement.ContentLoss.Baselines[j].Audio.LongestLostRun = 1
			rows[i].Measurement.ContentLoss.Baselines[j].Video.Lost = 1
			rows[i].Measurement.ContentLoss.Baselines[j].Video.LongestLostRun = 1
		}
	}
	return rows
}

func TestProfileTakeoverLossMatchesNetworkBaseline(t *testing.T) {
	rows := matchingNetworkLossRows(10)
	for _, name := range []string{"lan", "region", "far-caller", "lossy-dc"} {
		t.Run(name, func(t *testing.T) {
			r := judgeProfile(profileTrialOptions{Name: name, Scenario: "takeover"}, withProfileProbes(name, rows), profileTestLinks(t, name))
			if name == "lan" {
				require.Equal(t, "fail", r.Verdict, r.Reasons)
				require.Equal(t, "exact zero loss", r.VideoContentLoss.Method)
			} else {
				require.Equal(t, "pass", r.Verdict, r.Reasons)
				require.Equal(t, "Clopper-Pearson upper rate + binomial predictive upper bound", r.VideoContentLoss.Method)
			}
		})
	}
}

func TestProfileLossPolicyUsesMediaLinkPlan(t *testing.T) {
	for _, tc := range []struct {
		name, profile string
		links         []nettopology.LinkSetting
		verdict       string
	}{
		{"caller loss", "lan", []nettopology.LinkSetting{{Role: "caller", Loss: .1}}, "pass"},
		{"relay loss", "lan", []nettopology.LinkSetting{{Role: "relay", Loss: .1}}, "pass"},
		{"worker loss", "lan", []nettopology.LinkSetting{{Role: "worker-2", Loss: .1}}, "pass"},
		{"control and store loss only", "region", []nettopology.LinkSetting{{Role: "caller"}, {Role: "relay"}, {Role: "worker-1"}, {Role: "control", Loss: .1}, {Role: "store", Loss: .1}}, "fail"},
		{"clean media with delay and reorder", "far-caller", []nettopology.LinkSetting{{Role: "caller", Delay: time.Millisecond, Reorder: .1}, {Role: "relay"}, {Role: "worker-1"}}, "fail"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := judgeProfile(profileTrialOptions{Name: tc.profile, Scenario: "takeover"}, matchingNetworkLossRows(10), recordedProfileLinks(tc.links))
			require.Equal(t, tc.verdict, r.Verdict, r.Reasons)
			if tc.verdict == "fail" {
				require.Equal(t, "exact zero loss", r.AudioContentLoss.Method)
			} else {
				require.NotEqual(t, "exact zero loss", r.AudioContentLoss.Method)
			}
		})
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
	require.Equal(t, "fail", r.Verdict, "lossless media links still require exact zero")
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
	for _, name := range []string{"region", "far-caller", "lossy-dc"} {
		t.Run(name, func(t *testing.T) {
			options := profileTrialOptions{Name: name, Scenario: "takeover"}
			for _, tc := range []struct {
				name    string
				mutate  func([]profileRow)
				verdict string
			}{
				{"excess audio", func(rows []profileRow) {
					for i := range rows {
						rows[i].Measurement.ContentLoss.Event.Audio.Lost = 10
						rows[i].Measurement.ContentLoss.Event.Audio.LongestLostRun = 10
						rows[i].Measurement.LostAudio = 200 * time.Millisecond
					}
				}, "fail"},
				{"excess video", func(rows []profileRow) {
					for i := range rows {
						rows[i].Measurement.ContentLoss.Event.Video.Lost = 10
						rows[i].Measurement.ContentLoss.Event.Video.LongestLostRun = 10
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
					rows := withProfileProbes(name, matchingNetworkLossRows(10))
					tc.mutate(rows)
					r := judgeProfile(options, rows, profileTestLinks(t, options.Name))
					require.Equal(t, tc.verdict, r.Verdict, r.Reasons)
				})
			}
			r := judgeProfile(options, withProfileProbes(name, matchingNetworkLossRows(10)), profileTestLinks(t, options.Name))
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
			r = judgeProfile(options, withProfileProbes(name, matchingNetworkLossRows(20)), profileTestLinks(t, options.Name))
			require.Equal(t, "pass", r.Verdict, r.Reasons)
			require.Equal(t, 20, r.AudioContentLoss.Event.Lost)
		})
	}
}

func recordedProfileLinks(links []nettopology.LinkSetting) []nettopology.LinkSetting {
	for i := range links {
		links[i].Qdisc = "qdisc noqueue 0: root"
		if links[i].Delay > 0 || links[i].Jitter > 0 || links[i].Reorder > 0 || links[i].Loss > 0 {
			links[i].Qdisc = fmt.Sprintf("qdisc netem 8001: root loss %g%%", links[i].Loss)
		}
	}
	return links
}
func withProfileProbes(name string, rows []profileRow) []profileRow {
	out := append([]profileRow(nil), rows...)
	if name == "lossy-dc" {
		for i := range out {
			out[i].ProbeEvents = []callharness.MoveReport{{Kind: "move"}, {Kind: "move"}}
			out[i].UncertainReplyProbe = profileProbeVerdict{Verdict: "pass", Trials: 1, MediaResumed: true}
			out[i].BarrierResendProbe = out[i].UncertainReplyProbe
			out[i].ObservedUncertainMoveResumesAvailable = true
			out[i].ObservedUncertainMoveResumes = 1
		}
	}
	return out
}
