package main

import (
	"errors"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/callharness"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/stretchr/testify/require"
)

func TestProfileGapIncludesDelayedFarCallerHold(t *testing.T) {
	event := callharness.MoveReport{Start: 0, End: 30 * time.Millisecond}
	event.Measurement.Audio.BaselineSamples = 10
	event.Measurement.Audio.BaselineMedian = 150 * time.Millisecond
	event.Measurement.Audio.BaselineP95 = 160 * time.Millisecond
	arrivals := []time.Duration{-20 * time.Millisecond, 0, 20 * time.Millisecond, 40 * time.Millisecond, 60 * time.Millisecond, 75 * time.Millisecond, 150 * time.Millisecond, 170 * time.Millisecond}
	report := &callharness.Report{Tracks: []callharness.TrackReport{{Kind: "audio", Arrivals: arrivals}}}
	old, complete := profileEventGap(arrivals, event.Start, event.End)
	require.True(t, complete)
	require.Equal(t, 20*time.Millisecond, old)
	gap, window, reasons := measureProfileGap(report, event, "audio", profileTestLinks(t, "far-caller"))
	require.Empty(t, reasons)
	require.True(t, window.Complete)
	require.Equal(t, 135*time.Millisecond, window.End)
	require.Equal(t, 75*time.Millisecond, gap)
	require.Greater(t, gap, 20*time.Millisecond+50*time.Millisecond)
	event.Measurement.Audio.BaselineMedian = 0
	_, _, reasons = measureProfileGap(report, event, "audio", profileTestLinks(t, "far-caller"))
	require.Contains(t, reasons, "audio content-age baseline disagrees with caller-link return delay")
	report.Tracks[0].Arrivals = arrivals[5:]
	_, window, reasons = measureProfileGap(report, event, "audio", profileTestLinks(t, "far-caller"))
	require.False(t, window.Complete)
	require.Contains(t, reasons, "audio extended event-gap history unavailable")
}

func TestProfileProbeCeilingsAndResume(t *testing.T) {
	for _, tc := range []struct {
		name    string
		barrier bool
		gap     time.Duration
		resumed bool
		want    string
	}{
		{"barrier inclusive", true, 225 * time.Millisecond, true, "pass"},
		{"barrier excess", true, 226 * time.Millisecond, true, "fail"},
		{"uncertain below cap", false, 1999 * time.Millisecond, true, "pass"},
		{"uncertain at cap", false, 2 * time.Second, true, "fail"},
		{"no resumed media", false, 60 * time.Millisecond, false, "fail"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			event := callharness.MoveReport{Kind: "move", Start: time.Second, End: time.Second + tc.gap}
			event.Measurement.Audio.BaselineSamples = 10
			event.Measurement.Video.BaselineSamples = 10
			if tc.resumed {
				event.Tracks = []callharness.MoveTrackReport{{Kind: "audio", PacketsAfter: 1}, {Kind: "video", PacketsAfter: 1}}
			}
			if !tc.barrier {
				event.Error = "uncertain move rolled back"
			}
			arrivals := []time.Duration{0, time.Second, time.Second + tc.gap, 4 * time.Second}
			// Keep the next sample close enough that the extended-window boundary
			// cannot import an unrelated later gap.
			arrivals[3] = arrivals[2] + 40*time.Millisecond
			report := &callharness.Report{Tracks: []callharness.TrackReport{{Kind: "audio", Arrivals: arrivals}, {Kind: "video", Arrivals: arrivals}}}
			got := judgeProfileProbe(report, event, profileTestLinks(t, "lossy-dc"), tc.barrier)
			require.Equal(t, tc.want, got.Verdict, got.Reasons)
			require.Equal(t, tc.resumed, got.MediaResumed)
		})
	}
}

func TestProfileEarlierEventsAndProbeVerdictsGate(t *testing.T) {
	for _, name := range []string{"lan", "region", "far-caller", "lossy-dc"} {
		t.Run(name, func(t *testing.T) {
			rows := withProfileProbes(name, goodProfileRows(20))
			rows[0].BarrierResends = 1
			rows[0].InjectedDiscardedReplies = 1
			options := profileTrialOptions{Name: name, Scenario: "move"}
			links := profileTestLinks(t, name)
			require.Equal(t, "pass", judgeProfile(options, rows, links).Verdict)
			rows[0].ProbeEvents = append(rows[0].ProbeEvents, callharness.MoveReport{Kind: "takeover"})
			require.Equal(t, "fail", judgeProfile(options, rows, links).Verdict)
			rows = withProfileProbes(name, goodProfileRows(20))
			rows[0].BarrierResends = 1
			rows[0].InjectedDiscardedReplies = 1
			if name == "lossy-dc" {
				rows[0].ProbeEvents = rows[0].ProbeEvents[:1]
				require.Equal(t, "fail", judgeProfile(options, rows, links).Verdict)
				rows = withProfileProbes(name, goodProfileRows(20))
				rows[0].BarrierResends = 1
				rows[0].InjectedDiscardedReplies = 1
				rows[0].BarrierResendProbe = profileProbeVerdict{Verdict: "fail", Trials: 1, AudioGap: 226 * time.Millisecond, Reasons: []string{"audio probe gap exceeds ceiling"}}
				result := judgeProfile(options, rows, links)
				require.Equal(t, "fail", result.Verdict)
				require.Contains(t, profileResultLine(result), "barrier_resend_probes=fail")
				rows[0].BarrierResendProbe.Verdict = "pass"
				rows[0].UncertainReplyProbe.Verdict = "inconclusive"
				require.Equal(t, "inconclusive", judgeProfile(options, rows, links).Verdict)
			} else {
				rows[0].ProbeEvents = []callharness.MoveReport{{Kind: "move"}}
				require.Equal(t, "fail", judgeProfile(options, rows, links).Verdict)
			}
		})
	}
}

func TestProfileTakeoverBurstGuardDoesNotHideInPool(t *testing.T) {
	options := profileTrialOptions{Name: "region", Scenario: "takeover"}
	links := profileTestLinks(t, "region")
	rows := matchingNetworkLossRows(10)
	rows[0].Measurement.ContentLoss.Event.Video.Lost = 15
	rows[0].Measurement.ContentLoss.Event.Video.LongestLostRun = 15
	result := judgeProfile(options, rows, links)
	require.Equal(t, "pass", result.VideoContentLoss.Verdict, "reviewer's burst fits the pooled bound")
	require.Equal(t, "fail", result.Verdict)
	require.Contains(t, result.Rows[0].Reasons, "trial 1: video loss burst: consecutive=15, limit=4")
	rows[0].Measurement.ContentLoss.Event.Video.LongestLostRun = 1
	require.Equal(t, "pass", judgeProfile(options, rows, links).Verdict, "independent scattered losses fit the bound and pass")
	rows[0].Measurement.ContentLoss.Event.Video.LongestLostRun = 4
	rows[9].Measurement.ContentLoss.Baselines[0].Video.Lost = 4
	rows[9].Measurement.ContentLoss.Baselines[0].Video.LongestLostRun = 4
	require.Equal(t, "pass", judgeProfile(options, rows, links).Verdict, "run-wide baseline maximum sets limit 5")
	rows[0].Measurement.ContentLoss.Event.Video.LongestLostRun = 5
	require.Equal(t, "fail", judgeProfile(options, rows, links).Verdict)
	rows[0].Measurement.ContentLoss.Event.Video.RunTrusted = false
	require.Equal(t, "inconclusive", judgeProfile(options, rows, links).Verdict)
}

func TestProfileFailureEvidenceAndNoiseCaps(t *testing.T) {
	for _, definite := range []bool{false, true} {
		err := errors.New("measurement unavailable")
		if definite {
			err = definiteProfileFailure("takeover lost=true")
		}
		rows := goodProfileRows(10)
		rows[0] = profileFailureRow(1, err)
		// A trial with no caller report supplies no event inventory either.
		result := judgeProfile(profileTrialOptions{Name: "lan", Scenario: "takeover"}, rows, profileTestLinks(t, "lan"))
		want := "inconclusive"
		if definite {
			want = "fail"
		}
		require.Equal(t, want, result.Verdict, result.Reasons)
	}
	for _, rate := range []float64{.1, .11, 1} {
		c := callharness.ContentLossCounts{Sent: 1000, Lost: int(rate * 1000), Trusted: true}
		got := judgeContentLoss(c, c, 30, true)
		want := "inconclusive"
		if rate == .1 {
			want = "pass"
		}
		require.Equal(t, want, got.Verdict)
		if rate > .1 {
			require.Contains(t, got.Reasons, "baseline too lossy to judge")
		}
	}
	rows := goodProfileRows(20)
	rows[19].AudioGap = 2 * time.Second
	result := judgeProfile(profileTrialOptions{Name: "region", Scenario: "move"}, rows, profileTestLinks(t, "region"))
	require.Equal(t, 60*time.Millisecond, result.AudioP95)
	require.Equal(t, "fail", result.Verdict)
	require.Contains(t, result.Rows[19].Reasons, "trial 20: planned move reaches 2s ceiling")
	for _, name := range []string{"lan", "region"} {
		rows = matchingNetworkLossRows(10)
		for i := range rows {
			rows[i].Measurement.AudioLoss = callharness.MetricVerdict{Trusted: false, Failed: true, Reasons: []string{"sent audio never returned", "event history expired"}}
			rows[i].Measurement.VideoLoss = callharness.MetricVerdict{Trusted: false, Failed: true, Reasons: []string{"sent video never returned complete", "event history expired"}}
			rows[i].Measurement.ContentLoss.Event.Audio.Trusted = false
			rows[i].Measurement.ContentLoss.Event.Video.Trusted = false
		}
		require.Equal(t, "inconclusive", judgeProfile(profileTrialOptions{Name: name, Scenario: "takeover"}, rows, profileTestLinks(t, name)).Verdict)
	}
	links := profileTestLinks(t, "region")
	for i := range links {
		if links[i].Role == "caller" {
			links[i].Qdisc = "qdisc netem 1: root loss 0.1%"
		}
	}
	require.Equal(t, "inconclusive", judgeProfile(profileTrialOptions{Name: "region", Scenario: "takeover"}, matchingNetworkLossRows(10), links).Verdict)
	rows = goodProfileRows(20)
	rows[0].Measurement.Audio.BaselineMedian = 150 * time.Millisecond
	rows[0].Measurement.Audio.BaselineP95 = 167600 * time.Microsecond
	result = judgeProfile(profileTrialOptions{Name: "far-caller", Scenario: "move"}, rows, profileTestLinks(t, "far-caller"))
	require.Equal(t, 22400*time.Microsecond, result.Rows[0].AudioSpreadMargin)
}

func TestProfileNoisyTrialDoesNotRejectHealthyPooledBaseline(t *testing.T) {
	for _, scenario := range []string{"move", "takeover"} {
		t.Run(scenario, func(t *testing.T) {
			rows := goodProfileRows(20)
			for i := range rows {
				for j := range rows[i].Measurement.ContentLoss.Baselines {
					counts := &rows[i].Measurement.ContentLoss.Baselines[j].Video
					counts.Sent = 16
				}
			}
			// Five missing frames among only 48 baseline frames is noisy locally;
			// it is still a healthy 5/960 pooled baseline in this run.
			rows[0].Measurement.ContentLoss.Baselines[0].Video.Lost = 5
			rows[0].Measurement.ContentLoss.Baselines[0].Video.LongestLostRun = 1
			options := profileTrialOptions{Name: "far-caller", Scenario: scenario}
			links := profileTestLinks(t, "far-caller")
			result := judgeProfile(options, rows, links)
			require.Equal(t, "pass", result.Verdict, result.Reasons)
			require.Equal(t, "conclusive", result.Rows[0].Verdict, result.Rows[0].Reasons)
			require.Equal(t, "pass", result.VideoContentLoss.Verdict)
			for i := range rows {
				for j := range rows[i].Measurement.ContentLoss.Baselines {
					counts := &rows[i].Measurement.ContentLoss.Baselines[j].Video
					counts.Lost = 2
					counts.LongestLostRun = 1
				}
			}
			result = judgeProfile(options, rows, links)
			require.Equal(t, "inconclusive", result.VideoContentLoss.Verdict)
			require.Contains(t, result.VideoContentLoss.Reasons, "baseline too lossy to judge")
			if scenario == "takeover" {
				require.Equal(t, "inconclusive", result.Verdict)
			} else {
				require.Equal(t, "pass", result.Verdict, "move content loss stays diagnostic")
			}
		})
	}
}

func TestProfileBeforeAcceptanceIgnoresDeferredMoveStatus(t *testing.T) {
	move := controlplane.MoveResult{Kind: "move", ID: "call"}
	takeover := controlplane.MoveResult{Kind: "takeover", ID: "call"}
	require.Empty(t, crashTakeovers([]controlplane.MoveResult{move}))
	require.Equal(t, []controlplane.MoveResult{takeover}, crashTakeovers([]controlplane.MoveResult{move, takeover, move}))
}

func TestProfileRequiresObservedUncertainResumeEvidence(t *testing.T) {
	for _, tc := range []struct {
		name      string
		available bool
		count     uint64
		want      string
	}{
		{"positive delta", true, 1, "pass"}, {"no observed path", true, 0, "inconclusive"}, {"missing counter", false, 0, "inconclusive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows := withProfileProbes("lossy-dc", goodProfileRows(20))
			rows[0].BarrierResends = 1
			rows[0].InjectedDiscardedReplies = 1
			for i := range rows {
				rows[i].ObservedUncertainMoveResumesAvailable = true
				rows[i].ObservedUncertainMoveResumes = 1
			}
			rows[0].ObservedUncertainMoveResumesAvailable = tc.available
			rows[0].ObservedUncertainMoveResumes = tc.count
			result := judgeProfile(profileTrialOptions{Name: "lossy-dc", Scenario: "move"}, rows, profileTestLinks(t, "lossy-dc"))
			require.Equal(t, tc.want, result.Verdict, result.Reasons)
			if tc.want != "pass" {
				require.Contains(t, result.Rows[0].Reasons, "trial 1: observed uncertain move-resume path NOT-PROVEN")
			}
		})
	}
}

func TestProfileObservedResumeCountUsesDelta(t *testing.T) {
	for _, tc := range []struct {
		name              string
		before, after     float64
		beforeOK, afterOK bool
		delta             uint64
		available         bool
	}{
		{"existing count is not a new path", 7, 7, true, true, 0, true},
		{"only new paths", 7, 9, true, true, 2, true},
		{"missing baseline", 0, 9, false, true, 0, false},
		{"missing final", 7, 0, true, false, 0, false},
		{"counter reset", 9, 1, true, true, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			delta, available := observedUncertainResumeDelta(tc.before, tc.beforeOK, tc.after, tc.afterOK)
			require.Equal(t, tc.available, available)
			require.Equal(t, tc.delta, delta)
		})
	}
}
