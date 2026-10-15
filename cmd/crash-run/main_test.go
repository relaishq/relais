package main

import (
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/callharness"
	"github.com/stretchr/testify/require"
)

func successfulReport() *callharness.Report {
	return &callharness.Report{
		OfferAnswerExchanges: 1, RemoteAddr: "127.0.0.1:9", ConnectionStates: []callharness.StateChange{{State: "connected"}},
		Consent: callharness.ConsentReport{ObservedFor: 61 * time.Second, ResponsesAfter: 30},
		Tracks:  []callharness.TrackReport{{Kind: "video", Video: &callharness.VideoReport{FullDecode: callharness.FullDecode{FramesIn: 10, FramesDecoded: 10}}}},
		Moves:   []callharness.MoveReport{{Kind: "takeover", Tracks: []callharness.MoveTrackReport{{Kind: "audio", Gap: 450 * time.Millisecond, PacketsAfter: 10}, {Kind: "video", Gap: 460 * time.Millisecond, PacketsAfter: 10}}, Measurement: callharness.EventMeasurement{SettleWindow: 500 * time.Millisecond, AudioLoss: callharness.MetricVerdict{Trusted: true}, VideoLoss: callharness.MetricVerdict{Trusted: true}, Audio: callharness.FreshnessReport{Verdict: callharness.MetricVerdict{Trusted: true}}, Video: callharness.FreshnessReport{Verdict: callharness.MetricVerdict{Trusted: true}}, ResumeVerdict: callharness.MetricVerdict{Trusted: true}, FirstContentVerdict: callharness.MetricVerdict{Trusted: true}, FirstNewContentVerdict: callharness.MetricVerdict{Trusted: true}, FirstLiveVerdict: callharness.MetricVerdict{Trusted: true}}, Recovery: callharness.VideoRecovery{Path: "Keyframe", LivePath: "Keyframe", FirstDecodedAfterKill: 470 * time.Millisecond, FirstDecodedLiveAfterKill: 470 * time.Millisecond, FirstDecodedPLIAfterKill: 470 * time.Millisecond}}},
	}
}
func TestCrashRunRejectsIncompleteOrUncleanCallerEvidence(t *testing.T) {
	require.True(t, measure(successfulReport(), "127.0.0.1:9", 60*time.Second).Pass)
	mutations := map[string]func(*callharness.Report){
		"missing measurement":     func(r *callharness.Report) { r.Moves[0].Measurement = callharness.EventMeasurement{} },
		"no takeover":             func(r *callharness.Report) { r.Moves = nil },
		"missing resumed packets": func(r *callharness.Report) { r.Moves[0].Tracks[0].PacketsAfter = 0 },
		"slow gap":                func(r *callharness.Report) { r.Moves[0].Tracks[0].Gap = 2 * time.Second },
		"post-resume decryption":  func(r *callharness.Report) { r.Moves[0].DecryptionFailuresAfterResume = 1 },
		"ICE restart":             func(r *callharness.Report) { r.ICERestarts = 1 },
		"renegotiation":           func(r *callharness.Report) { r.Renegotiations = 1 },
		"disconnected": func(r *callharness.Report) {
			r.ConnectionStates = append(r.ConnectionStates, callharness.StateChange{State: "disconnected"})
		},
		"no decoder":               func(r *callharness.Report) { r.Tracks[0].Video.FullDecode.Skipped = "ffmpeg absent" },
		"decode error":             func(r *callharness.Report) { r.Tracks[0].Video.FullDecode.Errors = "invalid frame" },
		"incomplete decode":        func(r *callharness.Report) { r.Tracks[0].Video.FullDecode.FramesDecoded = 9 },
		"unknown path":             func(r *callharness.Report) { r.Moves[0].Recovery.Path = "" },
		"cache without packets":    func(r *callharness.Report) { r.Moves[0].Recovery.Path = "Cache" },
		"missing live attribution": func(r *callharness.Report) { r.Moves[0].Recovery.LivePath = "" },
		"no live decode":           func(r *callharness.Report) { r.Moves[0].Recovery.FirstDecodedLiveAfterKill = 0 },
		"short consent":            func(r *callharness.Report) { r.Consent.ObservedFor = 59 * time.Second },
		"no consent answer":        func(r *callharness.Report) { r.Consent.ResponsesAfter = 0 },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			r := successfulReport()
			mutate(r)
			require.False(t, measure(r, "127.0.0.1:9", 60*time.Second).Pass)
		})
	}
	require.False(t, measure(successfulReport(), "127.0.0.1:10", 60*time.Second).Pass)
}

func TestCacheAttributionCountsActualDecodedPath(t *testing.T) {
	require.Equal(t, 1, cacheAttributions([]result{{Path: "Cache", ReplayPackets: 2}, {Path: "Keyframe", ReplayPackets: 2}, {Path: "Cache"}}))
	require.Equal(t, 25*time.Millisecond, median([]time.Duration{40 * time.Millisecond, 10 * time.Millisecond, 30 * time.Millisecond, 20 * time.Millisecond}))
}

func TestCacheOffPLIAttribution(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*callharness.VideoRecovery)
		want   bool
	}{
		{"requested keyframe first", func(r *callharness.VideoRecovery) {}, true},
		{"natural keyframe before requested keyframe", func(r *callharness.VideoRecovery) {
			r.Path = "Live"
			r.FirstDecodedAfterKill = 400 * time.Millisecond
		}, true},
		{"natural first live before requested keyframe", func(r *callharness.VideoRecovery) {
			r.Path, r.LivePath = "Live", "Live"
			r.FirstDecodedAfterKill, r.FirstDecodedLiveAfterKill = 400*time.Millisecond, 400*time.Millisecond
		}, true},
		{"natural keyframe alone", func(r *callharness.VideoRecovery) {
			r.Path, r.LivePath = "Live", "Live"
			r.FirstDecodedPLIAfterKill = 0
		}, false},
		{"path label without decoded PLI evidence", func(r *callharness.VideoRecovery) {
			r.FirstDecodedPLIAfterKill = 0
		}, false},
		{"cache replay with decoded PLI", func(r *callharness.VideoRecovery) {
			r.Path, r.ReplayPackets = "Cache", 2
		}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := successfulReport()
			test.mutate(&r.Moves[0].Recovery)
			measured := measure(r, "127.0.0.1:9", 60*time.Second)
			require.True(t, measured.Pass)
			require.Equal(t, r.Moves[0].Recovery.FirstDecodedPLIAfterKill, measured.PLI)
			require.Equal(t, test.want, provesPLIRecovery(measured))
		})
	}
}

// An unknown metric preserves its reason; an independent trusted failure
// still rejects the run. Per-mode coverage rejects an entirely unknown run.
func TestCrashRunCarriesIndependentMetricUncertainty(t *testing.T) {
	report := successfulReport()
	report.Moves[0].Measurement = callharness.EventMeasurement{
		SettleWindow: 500 * time.Millisecond,
		LostAudio:    460 * time.Millisecond, LostVideoFrames: 14,
		Inconclusive: true, Reasons: []string{"video baseline has 2 samples (need 10)"},
	}
	measured := measure(report, "127.0.0.1:9", 60*time.Second)
	require.True(t, measured.Pass, "uncertainty alone is not a failure; the run-level coverage gate applies")
	require.Equal(t, report.Moves[0].Measurement, measured.Measurement)
	require.Contains(t, measured.Measurement.Summary(), "video baseline has 2 samples")
	require.Contains(t, (callharness.EventMeasurement{}).Summary(), "inconclusive: event measurement not available")
}

func TestCrashRunRejectsMeasuredRecoveryFailure(t *testing.T) {
	for _, metric := range []string{"resume", "new", "first", "live"} {
		t.Run(metric, func(t *testing.T) {
			r := successfulReport()
			m := &r.Moves[0].Measurement
			m.SettleWindow = 500 * time.Millisecond
			failed := callharness.MetricVerdict{Trusted: true, Failed: true}
			switch metric {
			case "resume":
				m.ResumeVerdict = failed
			case "new":
				m.FirstNewContentVerdict = failed
			case "first":
				m.FirstContentVerdict = failed
			case "live":
				m.FirstLiveVerdict = failed
			}
			m.Inconclusive = true // unrelated uncertainty cannot hide this failure.
			require.False(t, measure(r, "127.0.0.1:9", 60*time.Second).Pass)
		})
	}
}

func TestCrashRunRequiresConclusiveCoverage(t *testing.T) {
	unknown := make([]result, 10)
	for i := range unknown {
		unknown[i].Measurement.SettleWindow = time.Second
	}
	require.Zero(t, conclusiveEvents(unknown))
	require.False(t, callharness.EnoughConclusive(conclusiveEvents(unknown), len(unknown)))
	for i := range 8 {
		unknown[i].Measurement = successfulReport().Moves[0].Measurement
	}
	require.Equal(t, 8, conclusiveEvents(unknown))
	require.True(t, callharness.EnoughConclusive(conclusiveEvents(unknown), len(unknown)))
}

func TestCrashRunReportsFreshnessWithoutPhaseOneGate(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "unknown", true: "failed"}[failed], func(t *testing.T) {
			r := successfulReport()
			m := &r.Moves[0].Measurement
			m.Audio.Verdict = callharness.MetricVerdict{Trusted: failed, Failed: failed, Reasons: []string{"late freshness"}}
			m.Video.Verdict = m.Audio.Verdict
			measured := measure(r, "127.0.0.1:9", 60*time.Second)
			require.True(t, measured.Pass, "phase-1 freshness must remain diagnostic")
			require.Equal(t, *m, measured.Measurement, "keep the measured freshness in output")
			m.FirstNewContentVerdict = callharness.MetricVerdict{Trusted: true, Failed: true, Reasons: []string{"conflicting video content identities"}}
			require.False(t, measure(r, "127.0.0.1:9", 60*time.Second).Pass, "identity conflicts must remain gated")
		})
	}
}
