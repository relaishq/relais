//go:build !windows

package main

import (
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/callharness"
	"github.com/stretchr/testify/require"
)

func TestRelayDrainGapVerdicts(t *testing.T) {
	for _, tc := range []struct {
		name       string
		calls, bad int
		pass       bool
	}{
		{"smoke within target", 8, 0, true},
		{"smoke above target", 8, 1, false},
		{"one outlier in twenty", 20, 1, true},
		{"two outliers in twenty", 20, 2, false},
		{"pooled allowance", 40, 2, true},
		{"partial batch adds no allowance", 39, 2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var samples []relayDrainSample
			for i := range tc.calls {
				gap := 80 * time.Millisecond
				if i < tc.bad {
					gap = 101 * time.Millisecond
				}
				samples = append(samples,
					relayDrainSample{Kind: "audio", Gap: gap, Limit: 100 * time.Millisecond},
					relayDrainSample{Kind: "video", Gap: 100 * time.Millisecond, Limit: 100 * time.Millisecond})
			}
			err := judgeRelayDrainSamples(samples)
			if tc.pass {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "above planned-change target")
			}
		})
	}
}

func TestRelayDrainMissingGapEvidenceFails(t *testing.T) {
	for _, samples := range [][]relayDrainSample{
		nil,
		{{Kind: "audio", Gap: time.Millisecond, Limit: time.Second}},
		{{Kind: "audio", Gap: 0, Limit: time.Second}},
		{{Kind: "audio", Gap: time.Millisecond, Limit: 0}},
	} {
		require.Error(t, judgeRelayDrainSamples(samples))
	}
}

func TestRelayDrainCallContinuityVerdicts(t *testing.T) {
	started := time.Now()
	for _, tc := range []struct {
		name   string
		change func(*callharness.Report, *[]callharness.StateChange)
	}{
		{"intact", nil},
		{"decryption failure", func(r *callharness.Report, _ *[]callharness.StateChange) { r.DecryptionFailures.AuthTag = 1 }},
		{"audio duplicate", func(r *callharness.Report, _ *[]callharness.StateChange) { r.Tracks[0].DuplicatePackets = 1 }},
		{"video duplicate", func(r *callharness.Report, _ *[]callharness.StateChange) { r.Tracks[1].DuplicatePackets = 1 }},
		{"missing video", func(r *callharness.Report, _ *[]callharness.StateChange) { r.Tracks = r.Tracks[:1] }},
		{"audio stopped", func(r *callharness.Report, _ *[]callharness.StateChange) { r.Tracks[0].LastArrival = time.Second }},
		{"extra restart", func(r *callharness.Report, _ *[]callharness.StateChange) { r.ICERestarts++ }},
		{"extra exchange", func(r *callharness.Report, _ *[]callharness.StateChange) { r.OfferAnswerExchanges++ }},
		{"DTLS reconnected", func(_ *callharness.Report, s *[]callharness.StateChange) {
			*s = append(*s, callharness.StateChange{State: "connected"})
		}},
		{"DTLS failed", func(_ *callharness.Report, s *[]callharness.StateChange) {
			*s = append(*s, callharness.StateChange{State: "failed"})
		}},
		{"DTLS missing", func(_ *callharness.Report, s *[]callharness.StateChange) { *s = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report := &callharness.Report{
				StartedAt: started, OfferAnswerExchanges: 2, ICERestarts: 1,
				Tracks: []callharness.TrackReport{
					{Kind: "audio", LastArrival: 2 * time.Second},
					{Kind: "video", LastArrival: 2 * time.Second},
				},
			}
			states := []callharness.StateChange{{State: "connected"}}
			if tc.change != nil {
				tc.change(report, &states)
				require.Error(t, judgeRelayDrainCall(report, states, started.Add(time.Second)))
			} else {
				require.NoError(t, judgeRelayDrainCall(report, states, started.Add(time.Second)))
			}
		})
	}
}
