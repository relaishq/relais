//go:build !windows

package main

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/relaishq/relais/internal/relayloss"
	"github.com/relaishq/relais/pkg/callharness"
	"github.com/relaishq/relais/pkg/relay"
	"github.com/stretchr/testify/require"
)

func TestRelayLossBaselineWaitsForQueuedWorkerPackets(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started := time.Now()
		polls := 0
		baseline, err := lossStableStatus(t.Context(), func(context.Context) (relay.RelayStatus, error) {
			polls++
			s := relay.RelayStatus{Stats: relay.Stats{WorkerPackets: 10, CallerPackets: uint64(polls)}}
			// HTTP answers twice with the old count before queued UDP drains.
			if time.Since(started) >= 50*time.Millisecond {
				s.Stats.WorkerPackets = 20
			}
			if time.Since(started) >= 225*time.Millisecond {
				s.Stats.WorkerPackets = 30
			}
			return s, nil
		})
		require.NoError(t, err)
		require.Equal(t, uint64(30), baseline.Stats.WorkerPackets)
		require.Equal(t, 425*time.Millisecond, time.Since(started), "each late drain resets the quiet window; caller traffic need not settle")
	})
}

func TestRelayLossBaselineAcceptsIdleWorkerCounter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started := time.Now()
		_, err := lossStableStatus(t.Context(), func(context.Context) (relay.RelayStatus, error) {
			return relay.RelayStatus{}, nil
		})
		require.NoError(t, err)
		require.Equal(t, 200*time.Millisecond, time.Since(started))
	})
}

func TestRelayLossBaselineRejectsCounterThatNeverSettles(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started := time.Now()
		var packets uint64
		last, err := lossStableStatus(t.Context(), func(context.Context) (relay.RelayStatus, error) {
			packets++
			return relay.RelayStatus{Stats: relay.Stats{WorkerPackets: packets}}, nil
		})
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.ErrorContains(t, err, "worker packets did not settle (limit 3s")
		require.Equal(t, packets, last.Stats.WorkerPackets)
		require.Equal(t, 3*time.Second, time.Since(started))
	})
}

func TestRelayLossBaselinePreservesStatusErrorAndCancellation(t *testing.T) {
	cause := errors.New("status unavailable")
	_, err := lossStableStatus(t.Context(), func(context.Context) (relay.RelayStatus, error) {
		return relay.RelayStatus{}, cause
	})
	require.ErrorIs(t, err, cause)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = lossStableStatus(ctx, func(context.Context) (relay.RelayStatus, error) {
		return relay.RelayStatus{}, nil
	})
	require.ErrorIs(t, err, context.Canceled)
}

func TestRelayLossFailureNamesCallPhaseAndGap(t *testing.T) {
	cause := errors.New("no decoded progress")
	err := relayLossCallError("call-8", "decoded-progress", 5*time.Second, cause)
	require.ErrorIs(t, err, cause)
	require.EqualError(t, err, "call=call-8 phase=decoded-progress gap=5s: no decoded progress")
}

func TestRelayLossObservationRejectsSecondAndTrailingFreeze(t *testing.T) {
	started := time.Unix(1000, 0)
	good := func() *callharness.Report {
		return &callharness.Report{StartedAt: started, Tracks: []callharness.TrackReport{
			{Kind: "audio", Packets: 100, LastArrival: 10 * time.Second, MediaGap: 3 * time.Second},
			{Kind: "video", Packets: 100, LastArrival: 10 * time.Second, MediaGap: 3 * time.Second},
		}}
	}
	now := started.Add(10 * time.Second)
	require.NoError(t, relayLossMediaProgress("call-8", good(), now))
	require.Equal(t, 3*time.Second, relayLossObservedGap(good(), now))
	for _, track := range []int{0, 1} {
		secondFreeze := good()
		secondFreeze.Tracks[track].MediaGap = 6 * time.Second
		require.ErrorContains(t, relayLossMediaProgress("call-8", secondFreeze, now), "call=call-8 phase=post-recovery-"+secondFreeze.Tracks[track].Kind+" gap=6s")
		trailingFreeze := good()
		trailingFreeze.Tracks[track].LastArrival = 4 * time.Second
		require.Equal(t, 6*time.Second, relayLossObservedGap(trailingFreeze, now))
		require.ErrorContains(t, relayLossMediaProgress("call-8", trailingFreeze, now), "gap=6s")
	}
	missing := good()
	missing.Tracks = missing.Tracks[:1]
	require.ErrorContains(t, relayLossMediaProgress("call-8", missing, now), "evidence missing")
}

func TestResumedRelayFenceRejectsWorkerPacketsDuringObservation(t *testing.T) {
	before := relay.RelayStatus{Stats: relay.Stats{WorkerPackets: 10, CallerPackets: 20}}
	// Replayed caller ciphertext must reach the old relay; it must not cause
	// any worker output there, either immediately or later in the window.
	after := relay.RelayStatus{Stats: relay.Stats{WorkerPackets: 10, CallerPackets: 180}}
	require.NoError(t, lossResumedWorkerFence(before, after))
	after.Stats.WorkerPackets++
	require.ErrorContains(t, lossResumedWorkerFence(before, after), "phase=restored-relay-fence")
	require.ErrorContains(t, lossResumedWorkerFence(before, after), "before=10 after=11")
}

// Characterize the existing polling deadline before adding rehearsal margin.
func TestRelayLossWaitPreservesDeadlineAndImmediateSuccess(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		require.NoError(t, lossWait(ctx, func() bool { return true }))
		require.Equal(t, time.Duration(0), time.Since(start))
		require.ErrorIs(t, lossWait(ctx, func() bool { return false }), context.DeadlineExceeded)
		require.Equal(t, 5*time.Second, time.Since(start))
	})
}

func TestRelayLossOperationalMarginKeepsFiveSecondGrade(t *testing.T) {
	require.Equal(t, 1, (relayLossOptions{}).margin())
	require.Equal(t, 2, (relayLossOptions{ReportPath: "report.json"}).margin())
	started := time.Unix(1000, 0)
	report := &callharness.Report{StartedAt: started, Tracks: []callharness.TrackReport{
		{Kind: "audio", Packets: 100, LastArrival: 10 * time.Second, MediaGap: 6 * time.Second},
		{Kind: "video", Packets: 100, LastArrival: 10 * time.Second, MediaGap: time.Second},
	}}
	require.ErrorContains(t, relayLossMediaProgress("call-1", report, started.Add(10*time.Second)), "gap=6s")
}

func TestRelayLossReportNamesCurrentPhase(t *testing.T) {
	evidence := &relayloss.Report{}
	options := relayLossOptions{Evidence: evidence}
	options.phase("recovery")
	require.Equal(t, "recovery", evidence.FailedPhase)
	(relayLossOptions{}).phase("baseline")
}
