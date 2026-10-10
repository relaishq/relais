package main

import (
	"testing"
	"time"

	"github.com/relais/pkg/callharness"
	"github.com/stretchr/testify/require"
)

func restartedReport() (*callharness.Report, time.Time, time.Time) {
	started := time.Now()
	report := &callharness.Report{StartedAt: started, OfferAnswerExchanges: 1, RemoteAddr: "127.0.0.1:9", ConnectionStates: []callharness.StateChange{{State: "connected"}}, Consent: callharness.ConsentReport{ResponsesAfter: 1}, Tracks: []callharness.TrackReport{
		{Kind: "audio", Packets: 5, Arrivals: []time.Duration{990 * time.Millisecond, 1080 * time.Millisecond, 1100 * time.Millisecond, 1200 * time.Millisecond, 2 * time.Second}, LastArrival: 2 * time.Second},
		{Kind: "video", Packets: 5, Arrivals: []time.Duration{980 * time.Millisecond, 1100 * time.Millisecond, 1130 * time.Millisecond, 1200 * time.Millisecond, 2 * time.Second}, LastArrival: 2 * time.Second},
	}}
	return report, started.Add(time.Second), started.Add(2 * time.Second)
}
func TestRelayRestartMeasurementRejectsIncompleteCallerEvidence(t *testing.T) {
	report, killed, until := restartedReport()
	r := measureRelayRestart(report, "127.0.0.1:9", killed, until, false)
	require.True(t, r.Pass)
	require.Equal(t, 120*time.Millisecond, r.Gap)
	mutations := map[string]func(*callharness.Report){
		"decrypt":       func(r *callharness.Report) { r.DecryptionFailures.AuthTag = 1 },
		"reconnect":     func(r *callharness.Report) { r.ICERestarts = 1 },
		"renegotiation": func(r *callharness.Report) { r.Renegotiations = 1 },
		"no post-restart media": func(r *callharness.Report) {
			r.Tracks[0].Arrivals = []time.Duration{100 * time.Millisecond, 500 * time.Millisecond}
		},
		"slow recovery": func(r *callharness.Report) {
			r.Tracks[0].Arrivals = []time.Duration{990 * time.Millisecond, 2 * time.Second}
		},
		"missing track":                       func(r *callharness.Report) { r.Tracks = r.Tracks[:1] },
		"media stops before observation ends": func(r *callharness.Report) { r.Tracks[0].LastArrival = time.Second },
		"no consent":                          func(r *callharness.Report) { r.Consent.ResponsesAfter = 0 },
		"disconnected": func(r *callharness.Report) {
			r.ConnectionStates = append(r.ConnectionStates, callharness.StateChange{State: "disconnected"})
		},
		"different public address": func(r *callharness.Report) { r.RemoteAddr = "127.0.0.1:10" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			r, killed, until := restartedReport()
			mutate(r)
			require.False(t, measureRelayRestart(r, "127.0.0.1:9", killed, until, false).Pass)
		})
	}
	// Slow baseline recovery is explicitly an observation, not a <1s claim.
	report.Tracks[0].Arrivals = []time.Duration{990 * time.Millisecond, 2 * time.Second}
	require.True(t, measureRelayRestart(report, "127.0.0.1:9", killed, until, true).Pass)
}
