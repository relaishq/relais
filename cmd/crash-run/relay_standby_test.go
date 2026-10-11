package main

import (
	"github.com/relais/pkg/callharness"
	"github.com/relais/pkg/relay"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestRelayStandbyRejectsDuplicateMediaOrUnfencedTenure(t *testing.T) {
	check := func(mutate func(*callharness.Report, *relay.RelayStatus, *relay.RelayStatus)) result {
		report, killed, until := restartedReport()
		old := relay.RelayStatus{Instance: "old", Stats: relay.Stats{WorkerPackets: 20}, Continuity: &relay.ContinuityStatus{Epoch: 1}}
		next := relay.RelayStatus{Instance: "new", Stats: relay.Stats{WorkerPackets: 30, RoutesRestored: 1}, Continuity: &relay.ContinuityStatus{Epoch: 2, ClaimAt: killed.Add(50 * time.Millisecond)}}
		if mutate != nil {
			mutate(report, &old, &next)
		}
		return measureRelayStandby(report, "127.0.0.1:9", killed, until, old, next, 60*time.Millisecond, 70*time.Millisecond)
	}
	require.True(t, check(nil).Pass)
	for name, mutate := range map[string]func(*callharness.Report, *relay.RelayStatus, *relay.RelayStatus){
		"duplicate media":      func(r *callharness.Report, _, _ *relay.RelayStatus) { r.Tracks[0].DuplicatePackets = 1 },
		"same instance":        func(_ *callharness.Report, a, b *relay.RelayStatus) { b.Instance = a.Instance },
		"same epoch":           func(_ *callharness.Report, a, b *relay.RelayStatus) { b.Continuity.Epoch = a.Continuity.Epoch },
		"missing old counters": func(_ *callharness.Report, a, _ *relay.RelayStatus) { a.Stats.WorkerPackets = 0 },
		"missing new counters": func(_ *callharness.Report, _, b *relay.RelayStatus) { b.Stats.WorkerPackets = 0 },
		"no lease evidence":    func(_ *callharness.Report, _, b *relay.RelayStatus) { b.Continuity = nil },
		"fenced successor":     func(_ *callharness.Report, _, b *relay.RelayStatus) { b.Stats.SelfFences = 1 },
	} {
		t.Run(name, func(t *testing.T) { require.False(t, check(mutate).Pass) })
	}
}
