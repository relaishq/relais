package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relais/internal/privateapi"
	"github.com/relais/pkg/callharness"
	"github.com/relais/pkg/controlplane"
	"github.com/relais/pkg/mediaworker"
	"github.com/stretchr/testify/require"
)

func goodProfileRows() []profileRow {
	var rows []profileRow
	for i := 0; i < 10; i++ {
		b := gapBaseline{Samples: 20, Median: 20 * time.Millisecond, P95: 20 * time.Millisecond}
		for j := 0; j < 20; j++ {
			b.Intervals = append(b.Intervals, 20*time.Millisecond)
		}
		m := callharness.EventMeasurement{SettleWindow: 500 * time.Millisecond, AudioLoss: callharness.MetricVerdict{Trusted: true}, VideoLoss: callharness.MetricVerdict{Trusted: true}}
		m.Audio.BaselineSamples, m.Video.BaselineSamples = 20, 20
		m.ContentLoss.Event = callharness.ContentLossWindow{Start: 5 * time.Second, End: 6 * time.Second, Audio: callharness.ContentLossCounts{Sent: 50, Trusted: true}, Video: callharness.ContentLossCounts{Sent: 30, Trusted: true}}
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
			rows := goodProfileRows()
			rows[0].BarrierResends, rows[0].UncertainOutcomes = 1, 1
			r := judgeProfile(options, rows, nil)
			require.Equal(t, "pass", r.Verdict)
			require.Equal(t, 150*time.Millisecond, r.AbsoluteGapCeiling)
			for i := range rows {
				rows[i].VideoGap = 71 * time.Millisecond
			}
			r = judgeProfile(options, rows, nil)
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
			r = judgeProfile(options, rows, nil)
			require.Equal(t, "fail", r.Verdict)
			require.Contains(t, r.Reasons, "audio move p95 exceeds absolute 150ms ceiling")
			for i := range rows {
				rows[i].AudioGap = 150 * time.Millisecond
			}
			require.Equal(t, "pass", judgeProfile(options, rows, nil).Verdict)
		})
	}
}
func TestProfileInconclusiveCannotHideKnownFailure(t *testing.T) {
	options := profileTrialOptions{Name: "far-caller", Scenario: "takeover"}
	rows := goodProfileRows()
	rows[0].Measurement.AudioLoss.Trusted = false
	r := judgeProfile(options, rows, nil)
	require.Equal(t, "inconclusive", r.Verdict)
	require.Equal(t, "inconclusive", r.Rows[0].Verdict)
	require.NotEmpty(t, r.Rows[0].Reasons)
	rows[1].Integrity.Failed = true
	rows[1].Integrity.Reasons = []string{"decryption failures"}
	r = judgeProfile(options, rows, nil)
	require.Equal(t, "fail", r.Verdict)
	require.Equal(t, "inconclusive", r.Rows[0].Verdict)
	require.Equal(t, "fail", r.Rows[1].Verdict)
	rows = goodProfileRows()
	rows[0].VideoGap = 2 * time.Second
	require.Equal(t, "fail", judgeProfile(options, rows, nil).Verdict)
	rows = goodProfileRows()
	rows[0].Measurement.LostAudio = time.Millisecond
	require.Equal(t, "fail", judgeProfile(profileTrialOptions{Name: "lan", Scenario: "takeover"}, rows, nil).Verdict)
	rows = goodProfileRows()
	rows[0].AudioBaseline.P95 = 80 * time.Millisecond
	require.Equal(t, "inconclusive", judgeProfile(options, rows, nil).Verdict)
	rows = goodProfileRows()
	rows[0].Measurement.Video.BaselineP95 = 21 * time.Millisecond
	require.Equal(t, "inconclusive", judgeProfile(options, rows, nil).Verdict)
	require.Equal(t, "inconclusive", judgeProfile(options, goodProfileRows()[:1], nil).Verdict)
	options.Name = "lossy-dc"
	r = judgeProfile(options, goodProfileRows(), nil)
	require.Equal(t, "inconclusive", r.Verdict)
	require.Contains(t, r.Reasons, "barrier resend NOT-PROVEN")
	require.Contains(t, r.Reasons, "uncertain outcome NOT-PROVEN")
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
	rows := goodProfileRows()
	for i := range rows {
		rows[i].AudioGap = 100 * time.Millisecond
	}
	rows[0].AudioBaseline.P95 = 60 * time.Millisecond
	rows[0].AudioBaseline.Intervals[18] = 60 * time.Millisecond
	rows[0].AudioBaseline.Intervals[19] = 60 * time.Millisecond
	r := judgeProfile(profileTrialOptions{Name: "region", Scenario: "move"}, rows, nil)
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
	measurement := goodProfileRows()[0].Measurement
	report := &callharness.Report{
		OfferAnswerExchanges: 1, RemoteAddr: "127.0.0.1:10001",
		ConnectionStates: []callharness.StateChange{{State: "connected"}},
		Consent:          callharness.ConsentReport{ResponsesAfter: 1, ObservedFor: 60 * time.Second},
		Moves: []callharness.MoveReport{
			{Kind: "move", Error: "uncertain reply probe rolled back"},
			{Kind: "takeover", Start: 4 * time.Second, Measurement: measurement, Tracks: []callharness.MoveTrackReport{{Kind: "audio", Gap: 200 * time.Millisecond, PacketsAfter: 1}, {Kind: "video", Gap: 250 * time.Millisecond, PacketsAfter: 1}}},
		},
		Tracks: []callharness.TrackReport{{Kind: "audio"}, {Kind: "video", Video: &callharness.VideoReport{FullDecode: callharness.FullDecode{Failure: callharness.DecodeInvalid, Errors: "baseline packet loss", FramesDecoded: 1}}}},
	}
	for i := 0; i < 150; i++ {
		for j := range report.Tracks {
			report.Tracks[j].Arrivals = append(report.Tracks[j].Arrivals, time.Duration(i)*20*time.Millisecond)
		}
	}
	options := profileTrialOptions{Name: "region", Scenario: "takeover"}
	row := measureProfile(report, report.RemoteAddr, options, 60*time.Second)
	require.True(t, row.Integrity.Trusted)
	require.False(t, row.Integrity.Failed, row.Integrity.Reasons)
	require.Len(t, row.ProbeEvents, 1)
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
