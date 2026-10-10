package main

import (
	"testing"
	"time"

	"github.com/relais/pkg/callharness"
	"github.com/stretchr/testify/require"
)

func successfulReport() *callharness.Report {
	return &callharness.Report{
		OfferAnswerExchanges: 1, RemoteAddr: "127.0.0.1:9", ConnectionStates: []callharness.StateChange{{State: "connected"}},
		Consent: callharness.ConsentReport{ObservedFor: 61 * time.Second, ResponsesAfter: 30},
		Tracks:  []callharness.TrackReport{{Kind: "video", Video: &callharness.VideoReport{FullDecode: callharness.FullDecode{FramesIn: 10, FramesDecoded: 10}}}},
		Moves:   []callharness.MoveReport{{Kind: "takeover", Tracks: []callharness.MoveTrackReport{{Kind: "audio", Gap: 450 * time.Millisecond, PacketsAfter: 10}, {Kind: "video", Gap: 460 * time.Millisecond, PacketsAfter: 10}}, Recovery: callharness.VideoRecovery{FirstDecodedAfterKill: 470 * time.Millisecond, FirstDecodedLiveAfterKill: 470 * time.Millisecond}}},
	}
}
func TestCrashRunRejectsIncompleteOrUncleanCallerEvidence(t *testing.T) {
	require.True(t, measure(successfulReport(), "127.0.0.1:9", 60*time.Second).Pass)
	mutations := map[string]func(*callharness.Report){
		"no takeover":             func(r *callharness.Report) { r.Moves = nil },
		"missing resumed packets": func(r *callharness.Report) { r.Moves[0].Tracks[0].PacketsAfter = 0 },
		"slow gap":                func(r *callharness.Report) { r.Moves[0].Tracks[0].Gap = 2 * time.Second },
		"post-resume decryption":  func(r *callharness.Report) { r.Moves[0].DecryptionFailuresAfterResume = 1 },
		"ICE restart":             func(r *callharness.Report) { r.ICERestarts = 1 },
		"renegotiation":           func(r *callharness.Report) { r.Renegotiations = 1 },
		"disconnected": func(r *callharness.Report) {
			r.ConnectionStates = append(r.ConnectionStates, callharness.StateChange{State: "disconnected"})
		},
		"no decoder":        func(r *callharness.Report) { r.Tracks[0].Video.FullDecode.Skipped = "ffmpeg absent" },
		"decode error":      func(r *callharness.Report) { r.Tracks[0].Video.FullDecode.Errors = "invalid frame" },
		"incomplete decode": func(r *callharness.Report) { r.Tracks[0].Video.FullDecode.FramesDecoded = 9 },
		"no live decode":    func(r *callharness.Report) { r.Moves[0].Recovery.FirstDecodedLiveAfterKill = 0 },
		"short consent":     func(r *callharness.Report) { r.Consent.ObservedFor = 59 * time.Second },
		"no consent answer": func(r *callharness.Report) { r.Consent.ResponsesAfter = 0 },
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
