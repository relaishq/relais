package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relais/internal/privateapi"
	"github.com/relais/pkg/callharness"
	"github.com/relais/pkg/controlplane"
	"github.com/relais/pkg/mediaworker"
	"github.com/stretchr/testify/require"
)

func goodProfileRows(trials int) []profileRow {
	var rows []profileRow
	for i := 0; i < trials; i++ {
		b := gapBaseline{Samples: 20, Median: 20 * time.Millisecond, P95: 20 * time.Millisecond}
		for j := 0; j < 20; j++ {
			b.Intervals = append(b.Intervals, 20*time.Millisecond)
		}
		m := callharness.EventMeasurement{SettleWindow: 500 * time.Millisecond, AudioLoss: callharness.MetricVerdict{Trusted: true}, VideoLoss: callharness.MetricVerdict{Trusted: true}}
		m.Audio.BaselineSamples, m.Video.BaselineSamples = 20, 20
		m.ContentLoss.Event = callharness.ContentLossWindow{Start: 5 * time.Second, End: 6 * time.Second, Audio: callharness.ContentLossCounts{Sent: 50, Trusted: true, RunTrusted: true}, Video: callharness.ContentLossCounts{Sent: 30, Trusted: true, RunTrusted: true}}
		for j := 0; j < 3; j++ {
			w := m.ContentLoss.Event
			w.End = 4500*time.Millisecond - time.Duration(j)*time.Second
			w.Start = w.End - time.Second
			m.ContentLoss.Baselines = append(m.ContentLoss.Baselines, w)
		}
		rows = append(rows, profileRow{Trial: i + 1, AudioGap: 60 * time.Millisecond, VideoGap: 65 * time.Millisecond, AudioBaseline: b, VideoBaseline: b, Measurement: m, Integrity: callharness.MetricVerdict{Trusted: true}})
	}
	return rows
}
func TestProfileVerdictSeparateGapsAndExactCeilings(t *testing.T) {
	for _, name := range []string{"lan", "region", "far-caller", "lossy-dc"} {
		t.Run(name, func(t *testing.T) {
			options := profileTrialOptions{Name: name, Scenario: "move"}
			rows := withProfileProbes(name, goodProfileRows(20))
			rows[0].BarrierResends, rows[0].InjectedDiscardedReplies = 1, 1
			r := judgeProfile(options, rows, profileTestLinks(t, options.Name))
			require.Equal(t, "pass", r.Verdict)
			require.Equal(t, 150*time.Millisecond, r.AbsoluteGapCeiling)
			for i := range rows {
				rows[i].VideoGap = 71 * time.Millisecond
			}
			r = judgeProfile(options, rows, profileTestLinks(t, options.Name))
			require.Equal(t, "fail", r.Verdict)
			require.Contains(t, r.Reasons, "video move p95 exceeds same-run baseline p95 + 50ms")
			for i := range rows {
				rows[i].VideoGap = 65 * time.Millisecond
				rows[i].AudioGap = 151 * time.Millisecond
				rows[i].AudioBaseline.P95 = 120 * time.Millisecond
				rows[i].AudioBaseline.Median = 120 * time.Millisecond
				for j := range rows[i].AudioBaseline.Intervals {
					rows[i].AudioBaseline.Intervals[j] = 120 * time.Millisecond
				}
			}
			r = judgeProfile(options, rows, profileTestLinks(t, options.Name))
			require.Equal(t, "fail", r.Verdict)
			require.Contains(t, r.Reasons, "audio move p95 exceeds absolute 150ms ceiling")
			for i := range rows {
				rows[i].AudioGap = 150 * time.Millisecond
			}
			require.Equal(t, "pass", judgeProfile(options, rows, profileTestLinks(t, options.Name)).Verdict)
		})
	}
}
func TestProfileInconclusiveCannotHideKnownFailure(t *testing.T) {
	options := profileTrialOptions{Name: "far-caller", Scenario: "takeover"}
	rows := goodProfileRows(10)
	rows[0].Measurement.AudioLoss.Trusted = false
	r := judgeProfile(options, rows, profileTestLinks(t, options.Name))
	require.Equal(t, "inconclusive", r.Verdict)
	require.Equal(t, "inconclusive", r.Rows[0].Verdict)
	require.NotEmpty(t, r.Rows[0].Reasons)
	rows[1].Integrity.Failed = true
	rows[1].Integrity.Reasons = []string{"decryption failures"}
	r = judgeProfile(options, rows, profileTestLinks(t, options.Name))
	require.Equal(t, "fail", r.Verdict)
	require.Equal(t, "inconclusive", r.Rows[0].Verdict)
	require.Equal(t, "fail", r.Rows[1].Verdict)
	rows = goodProfileRows(10)
	rows[0].VideoGap = 2 * time.Second
	require.Equal(t, "fail", judgeProfile(options, rows, profileTestLinks(t, options.Name)).Verdict)
	rows = goodProfileRows(10)
	rows[0].Measurement.LostAudio = time.Millisecond
	require.Equal(t, "fail", judgeProfile(profileTrialOptions{Name: "lan", Scenario: "takeover"}, rows, profileTestLinks(t, "lan")).Verdict)
	rows = goodProfileRows(10)
	rows[0].AudioBaseline.P95 = 80 * time.Millisecond
	require.Equal(t, "inconclusive", judgeProfile(options, rows, profileTestLinks(t, options.Name)).Verdict)
	rows = goodProfileRows(10)
	rows[0].Measurement.Video.BaselineP95 = 21 * time.Millisecond
	require.Equal(t, "inconclusive", judgeProfile(options, rows, profileTestLinks(t, options.Name)).Verdict)
	require.Equal(t, "inconclusive", judgeProfile(options, goodProfileRows(10)[:1], profileTestLinks(t, options.Name)).Verdict)
	options.Name = "lossy-dc"
	r = judgeProfile(options, withProfileProbes(options.Name, goodProfileRows(10)), profileTestLinks(t, options.Name))
	require.Equal(t, "inconclusive", r.Verdict)
	require.Contains(t, r.Reasons, "barrier resend NOT-PROVEN")
	require.Contains(t, r.Reasons, "injected discarded reply NOT-PROVEN")
}
func TestGapBaselineUsesOnlySameCallPreEventIntervals(t *testing.T) {
	var arrivals []time.Duration
	for i := 0; i < 100; i++ {
		arrivals = append(arrivals, time.Duration(i)*20*time.Millisecond)
	}
	arrivals = append(arrivals, 3*time.Second, 4*time.Second)
	b := baselineGaps(arrivals, 2*time.Second)
	require.Equal(t, 99, b.Samples)
	require.Equal(t, 20*time.Millisecond, b.P95)
	require.Equal(t, 20*time.Millisecond, quantile([]time.Duration{time.Second, time.Millisecond, 20 * time.Millisecond}, .5))
	require.Zero(t, quantile(nil, .95))
}
func TestUncertainProxyCommitsBeforeDiscardingExactlyOneReply(t *testing.T) {
	var resumes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/resume" {
			resumes.Add(1)
			privateapi.Write(w, mediaworker.ResumeReply{ID: "call"})
		} else {
			privateapi.Write(w, map[string]string{"id": "call"})
		}
	}))
	defer server.Close()
	fault, err := newUncertainProxy("127.0.0.1", server.URL)
	require.NoError(t, err)
	defer fault.Close()
	worker := controlplane.RemoteWorker{URL: fault.URL}
	fault.Arm()
	_, err = worker.ResumeSession(nil, mediaworker.ResumeOptions{})
	require.ErrorIs(t, err, privateapi.ErrUncertain)
	require.EqualValues(t, 1, resumes.Load())
	require.Equal(t, 1, fault.Count())
	id, err := worker.ResumeSession(nil, mediaworker.ResumeOptions{})
	require.NoError(t, err)
	require.Equal(t, "call", id)
	require.EqualValues(t, 2, resumes.Load())
	require.Equal(t, 1, fault.Count())
	fault.Arm()
	require.NoError(t, privateapi.Do(context.Background(), server.Client(), fault.URL, http.MethodGet, "/status", nil, &map[string]string{}, nil))
	require.Equal(t, 1, fault.Count())
	_, err = worker.ResumeSession(nil, mediaworker.ResumeOptions{})
	require.True(t, errors.Is(err, privateapi.ErrUncertain))
	require.Equal(t, 2, fault.Count())
}

func TestProfileBaselinePoolsIntervalsInsteadOfTrialPercentiles(t *testing.T) {
	rows := goodProfileRows(20)
	for i := range rows {
		rows[i].AudioGap = 100 * time.Millisecond
	}
	rows[0].AudioBaseline.P95 = 60 * time.Millisecond
	rows[0].AudioBaseline.Intervals[18] = 60 * time.Millisecond
	rows[0].AudioBaseline.Intervals[19] = 60 * time.Millisecond
	r := judgeProfile(profileTrialOptions{Name: "region", Scenario: "move"}, rows, profileTestLinks(t, "region"))
	require.Equal(t, 20*time.Millisecond, r.AudioBaselineP95)
	require.Equal(t, "fail", r.Verdict)
	require.Contains(t, r.Reasons, "audio move p95 exceeds same-run baseline p95 + 50ms")
}
func TestProfileScenarioOptionValidation(t *testing.T) {
	require.NoError(t, validateProfileScenario("netns", "move", false))
	require.NoError(t, validateProfileScenario("netns", "takeover", false))
	require.Error(t, validateProfileScenario("loopback", "move", false))
	require.Error(t, validateProfileScenario("netns", "drain", false))
	require.Error(t, validateProfileScenario("netns", "takeover", true))
}

func TestMeasureProfileSeparatesProbeFromAcceptanceAndDecoderDiagnostics(t *testing.T) {
	measurement := goodProfileRows(10)[0].Measurement
	report := &callharness.Report{
		OfferAnswerExchanges: 1, RemoteAddr: "127.0.0.1:10001",
		ConnectionStates: []callharness.StateChange{{State: "connected"}},
		Consent:          callharness.ConsentReport{ResponsesAfter: 1, ObservedFor: 60 * time.Second},
		Moves: []callharness.MoveReport{
			{Kind: "move", Start: time.Second, End: 1030 * time.Millisecond, Tracks: []callharness.MoveTrackReport{{Kind: "audio", PacketsAfter: 1}, {Kind: "video", PacketsAfter: 1}}, Error: "uncertain reply probe rolled back", Result: mediaworker.HandoverResult{Duration: time.Second}},
			{Kind: "move", Start: 2 * time.Second, End: 2120 * time.Millisecond, Tracks: []callharness.MoveTrackReport{{Kind: "audio", PacketsAfter: 1}, {Kind: "video", PacketsAfter: 1}}},
			{Kind: "takeover", Start: 4 * time.Second, End: 4100 * time.Millisecond, Measurement: measurement, Result: mediaworker.HandoverResult{Duration: 100 * time.Millisecond, Resume: 10 * time.Millisecond}, Tracks: []callharness.MoveTrackReport{{Kind: "audio", Gap: 200 * time.Millisecond, PacketsAfter: 1}, {Kind: "video", Gap: 250 * time.Millisecond, PacketsAfter: 1}}},
		},
		Tracks: []callharness.TrackReport{{Kind: "audio"}, {Kind: "video", Video: &callharness.VideoReport{FullDecode: callharness.FullDecode{Failure: callharness.DecodeInvalid, Errors: "baseline packet loss", FramesDecoded: 1}}}},
	}
	for i := 0; i <= 200; i++ {
		for j := range report.Tracks {
			report.Tracks[j].Arrivals = append(report.Tracks[j].Arrivals, time.Duration(i)*20*time.Millisecond)
		}
	}
	report.Tracks[0].Arrivals = append(report.Tracks[0].Arrivals, 4200*time.Millisecond, 4400*time.Millisecond)
	report.Tracks[1].Arrivals = append(report.Tracks[1].Arrivals, 4250*time.Millisecond, 4400*time.Millisecond)
	options := profileTrialOptions{Name: "lossy-dc", Scenario: "takeover", Links: profileTestLinks(t, "lossy-dc")}
	row := measureProfile(report, report.RemoteAddr, options, 60*time.Second)
	require.True(t, row.Integrity.Trusted)
	require.False(t, row.Integrity.Failed, row.Integrity.Reasons)
	require.Len(t, row.ProbeEvents, 2)
	require.Equal(t, report.Moves[2].Result, row.EventResult)
	require.NotEqual(t, report.Moves[0].Result, row.EventResult)
	require.Equal(t, 200*time.Millisecond, row.AudioGap)
	require.Equal(t, 250*time.Millisecond, row.VideoGap)
	require.Equal(t, callharness.DecodeInvalid, row.VideoDecode.Failure)
	require.GreaterOrEqual(t, row.AudioBaseline.Samples, 10)
	report.DecryptionFailures.Replay = 1
	require.True(t, measureProfile(report, report.RemoteAddr, options, 60*time.Second).Integrity.Failed)
	report.DecryptionFailures.Replay = 0
	report.ConnectionStates = append(report.ConnectionStates, callharness.StateChange{State: "disconnected"})
	require.True(t, measureProfile(report, report.RemoteAddr, options, 60*time.Second).Integrity.Failed)
}

func TestProfileMoveTrialCountAndP95(t *testing.T) {
	for _, name := range []string{"lan", "region", "far-caller", "lossy-dc"} {
		t.Run(name, func(t *testing.T) {
			options := profileTrialOptions{Name: name, Scenario: "move"}
			links := profileTestLinks(t, name)
			rows := withProfileProbes(name, goodProfileRows(20))
			rows[0].BarrierResends, rows[0].InjectedDiscardedReplies = 1, 1
			for _, count := range []int{10, 19} {
				r := judgeProfile(options, rows[:count], links)
				require.Equal(t, 20, r.MinimumTrials)
				require.Equal(t, "inconclusive", r.Verdict, r.Reasons)
				require.Contains(t, r.Reasons, fmt.Sprintf("too few trials: %d, need 20", count))
			}
			r := judgeProfile(options, rows, links)
			require.Equal(t, "pass", r.Verdict, r.Reasons)
			rows[19].AudioGap, rows[19].VideoGap = 500*time.Millisecond, 500*time.Millisecond
			r = judgeProfile(options, rows, links)
			require.Equal(t, "pass", r.Verdict, r.Reasons)
			require.Equal(t, 60*time.Millisecond, r.AudioP95)
			require.Equal(t, 65*time.Millisecond, r.VideoP95)
			rows[19].Integrity.Failed = true
			rows[19].Integrity.Reasons = []string{"decryption failures"}
			require.Equal(t, "fail", judgeProfile(options, rows, links).Verdict)
			rows[19].Integrity.Failed = false
			rows[18].AudioGap, rows[18].VideoGap = 400*time.Millisecond, 400*time.Millisecond
			r = judgeProfile(options, rows, links)
			require.Equal(t, "fail", r.Verdict, r.Reasons)
			require.Equal(t, 400*time.Millisecond, r.AudioP95)
			require.Equal(t, 400*time.Millisecond, r.VideoP95)
		})
	}
}

func TestProfileTakeoverCeilingStillChecksEveryTrial(t *testing.T) {
	options := profileTrialOptions{Name: "region", Scenario: "takeover"}
	links := profileTestLinks(t, options.Name)
	require.Equal(t, "pass", judgeProfile(options, goodProfileRows(10), links).Verdict)
	rows := goodProfileRows(20)
	rows[19].VideoGap = 2 * time.Second
	r := judgeProfile(options, rows, links)
	require.Equal(t, 10, r.MinimumTrials)
	require.Equal(t, 65*time.Millisecond, r.VideoP95)
	require.Equal(t, "fail", r.Verdict, r.Reasons)
	require.Contains(t, r.Reasons, "trial 20: pause reaches 2s ceiling")
}

func TestProfileReportRetainsOutlierPhasesAndPrintsThem(t *testing.T) {
	rows := goodProfileRows(20)
	rows[0].EventResult.Duration = time.Second // Longest controller event, not the largest media gap.
	rows[19].AudioGap, rows[19].VideoGap = 250*time.Millisecond, 260*time.Millisecond
	rows[19].EventResult = mediaworker.HandoverResult{Duration: 232 * time.Millisecond, Drain: 4 * time.Millisecond, Export: 210 * time.Millisecond, Resume: 9 * time.Millisecond, HeldPackets: 19}
	report := judgeProfile(profileTrialOptions{Name: "lan", Scenario: "move"}, rows, profileTestLinks(t, "lan"))
	require.Equal(t, "pass", report.Verdict, report.Reasons)
	line := profileResultLine(report)
	for _, field := range []string{"PROFILE_RESULT verdict=pass", "audio_p95=60ms", "video_p95=65ms", "slowest_trial=20", "slowest_audio_gap=250ms", "slowest_video_gap=260ms", "slowest_duration=232ms", "slowest_barrier=4ms", "slowest_export=210ms", "slowest_resume=9ms", "slowest_other=9ms"} {
		require.Contains(t, line, field)
	}
	dir := t.TempDir()
	require.NoError(t, writeProfileReport(dir, report))
	data, err := os.ReadFile(filepath.Join(dir, "profile-report.json"))
	require.NoError(t, err)
	var saved profileReport
	require.NoError(t, json.Unmarshal(data, &saved))
	require.Equal(t, rows[19].EventResult, saved.Rows[19].EventResult)
	require.Len(t, saved.Rows, 20)
	require.Equal(t, line, profileResultLine(saved))
	rows[19].EventResult = mediaworker.HandoverResult{}
	require.Contains(t, profileResultLine(report), "slowest_trial=20")
	require.Contains(t, profileResultLine(report), "slowest_phases=unavailable")
	require.Contains(t, profileResultLine(profileReport{}), "slowest_trial=unavailable slowest_phases=unavailable")
}
