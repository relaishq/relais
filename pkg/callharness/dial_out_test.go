package callharness_test

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/callharness"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

func TestDialOutEchoAndMixedMoves(t *testing.T) { forSessionStores(t, testDialOutEchoAndMixedMoves) }

func testDialOutEchoAndMixedMoves(t *testing.T, store sessionstore.Store) {
	h := startHarness(t, callharness.Options{Relay: true, SessionStore: store, Workers: 3, DialOutWorkers: []int{1, 2}})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	call, err := h.Dial(ctx, callharness.CallOptions{Video: true, Worker: 0})
	require.NoError(t, err)
	sent := make(chan error, 1)
	go func() { sent <- call.SendMedia(ctx, 20*time.Second) }()
	time.Sleep(300 * time.Millisecond)
	// Twenty moves in each direction, including dial-out -> dial-out.
	for range 20 {
		for _, target := range []int{1, 2, 0} {
			err := call.Handover(callharness.HandoverOptions{To: target})
			require.NoError(t, err)
			time.Sleep(250 * time.Millisecond)
		}
	}
	require.NoError(t, <-sent)
	report, err := call.Hangup(ctx)
	require.NoError(t, err)
	t.Log(report.Summary())
	assertCleanCall(t, report)
	workerFailures, err := h.WorkerDecryptionFailures()
	require.NoError(t, err)
	require.Zero(t, workerFailures)
	assertAudioEcho(t, report)
	assertVideoEcho(t, report)
	gaps := map[[2]int][]time.Duration{}
	var pooled []time.Duration
	for _, move := range report.Moves {
		require.Len(t, move.Tracks, 2)
		var gap time.Duration
		for _, track := range move.Tracks {
			require.Positive(t, track.PacketsAfter)
			gap = max(gap, track.Gap)
		}
		pair := [2]int{move.From, move.To}
		gaps[pair] = append(gaps[pair], gap)
		pooled = append(pooled, gap)
	}
	slices.Sort(pooled)
	t.Logf("DIAL_OUT_POOLED moves=%d p95=%s decryption_failures=%d worker_decryption_failures=%g", len(pooled), pooled[(95*len(pooled)+99)/100-1], report.DecryptionFailures.Total(), workerFailures)
	require.Len(t, gaps, 3)
	for pair, values := range gaps {
		require.Len(t, values, 20)
		slices.Sort(values)
		p95 := values[18]
		t.Logf("DIAL_OUT_MOVES from=%d to=%d moves=20 p95=%s decryption_failures=%d", pair[0], pair[1], p95, report.DecryptionFailures.Total())
		require.LessOrEqual(t, p95, 150*time.Millisecond)
	}

}

func TestDialOutLinkResetKeepsCalls(t *testing.T) {
	forSessionStores(t, testDialOutLinkResetKeepsCalls)
}

func testDialOutLinkResetKeepsCalls(t *testing.T, store sessionstore.Store) {
	for _, duration := range []time.Duration{200 * time.Millisecond, time.Second, 5 * time.Second} {
		t.Run(fmt.Sprint(duration), func(t *testing.T) {
			h := startHarness(t, callharness.Options{Relay: true, SessionStore: store, Workers: 2, DialOutWorkers: []int{0, 1}})
			ctx, cancel := context.WithTimeout(context.Background(), duration+12*time.Second)
			defer cancel()
			call, err := h.Dial(ctx, callharness.CallOptions{Video: true, Worker: 0})
			require.NoError(t, err)
			sent := make(chan error, 1)
			go func() { sent <- call.SendMedia(ctx, duration+6*time.Second) }()
			time.Sleep(300 * time.Millisecond)
			before, err := h.Status(ctx)
			require.NoError(t, err)
			require.NoError(t, h.SetWorkerLink(0, false))
			time.Sleep(duration)
			during, err := h.Status(ctx)
			require.NoError(t, err)
			require.Equal(t, before.Calls[0].Epoch, during.Calls[0].Epoch)
			require.Empty(t, during.Takeovers)
			require.Zero(t, during.LostCount)
			require.NoError(t, h.SetWorkerLink(0, true))
			require.Eventually(t, func() bool { err := call.Handover(callharness.HandoverOptions{To: 1}); return err == nil }, 4*time.Second, 50*time.Millisecond)
			require.NoError(t, <-sent)
			report, err := call.Hangup(ctx)
			require.NoError(t, err)
			t.Log(report.Summary())
			assertCleanCall(t, report)
			workerFailures, err := h.WorkerDecryptionFailures()
			require.NoError(t, err)
			require.Zero(t, workerFailures)
			assertAudioEcho(t, report)
			t.Logf("DIAL_OUT_LINK outage=%s false_takeovers=%d lost_calls=%d decryption_failures=%d worker_decryption_failures=%g", duration, len(during.Takeovers), during.LostCount, report.DecryptionFailures.Total(), workerFailures)
		})
	}
}

func TestDialOutCrashTakeover(t *testing.T) { forSessionStores(t, testDialOutCrashTakeover) }

func testDialOutCrashTakeover(t *testing.T, store sessionstore.Store) {
	for _, linkDown := range []bool{false, true} {
		t.Run(fmt.Sprintf("link-down=%t", linkDown), func(t *testing.T) {
			h := startHarness(t, callharness.Options{Relay: true, SessionStore: store, Workers: 2, DialOutWorkers: []int{0, 1}})
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			call, err := h.Dial(ctx, callharness.CallOptions{Video: true, Worker: 0})
			require.NoError(t, err)
			sent := make(chan error, 1)
			go func() { sent <- call.SendMedia(ctx, 6*time.Second) }()
			time.Sleep(time.Second)
			if linkDown {
				require.NoError(t, h.SetWorkerLink(0, false))
				time.Sleep(200 * time.Millisecond)
			}
			killed := time.Now()
			require.NoError(t, h.Kill(0))
			status := waitTakeovers(t, ctx, h, 1)
			elapsed := time.Since(killed)
			require.Less(t, elapsed, 2*time.Second)
			require.Zero(t, status.LostCount)
			require.False(t, status.Takeovers[0].Lost)
			require.NoError(t, <-sent)
			report, err := call.Hangup(ctx)
			require.NoError(t, err)
			assertCleanCall(t, report)
			workerFailures, err := h.WorkerDecryptionFailures()
			require.NoError(t, err)
			require.Zero(t, workerFailures)
			require.Len(t, report.Moves, 1)
			for _, track := range report.Moves[0].Tracks {
				require.Less(t, track.Gap, 2*time.Second)
				require.Positive(t, track.PacketsAfter)
			}
			t.Logf("DIAL_OUT_CRASH link_down=%t takeover=%s decryption_failures=%d worker_decryption_failures=%g", linkDown, elapsed, report.DecryptionFailures.Total(), workerFailures)
		})
	}
}
