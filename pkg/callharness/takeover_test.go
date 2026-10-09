package callharness_test

import (
	"context"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/relais/internal/workerprobe"
	"github.com/relais/pkg/callharness"
	"github.com/relais/pkg/controlplane"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func waitTakeovers(t *testing.T, ctx context.Context, h *callharness.Harness, count int) controlplane.Status {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		status, err := h.Status(ctx)
		require.NoError(t, err)
		if len(status.Takeovers) == count && len(status.Calls) == 1 && status.Calls[0].TakeoverCount == uint64(count) { //nolint:gosec // positive test count
			ready := true
			for _, worker := range status.Workers {
				if worker.Recovering {
					ready = false
				}
			}
			if ready {
				return status
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("automatic takeover did not complete within 2 s")
	return controlplane.Status{}
}

// One live caller survives twenty hard kills on three distinct private legs.
// Replacements are empty and never export or release the crashed tenure.
func TestRelayHardKillTakeover(t *testing.T) {
	h := startHarness(t, callharness.Options{Relay: true, Workers: 3})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	call, err := h.Dial(ctx, callharness.CallOptions{Video: true})
	require.NoError(t, err)
	sent := make(chan error, 1)
	go func() { sent <- call.SendMedia(ctx, 30*time.Second) }()
	time.Sleep(time.Second)
	var detection []time.Duration
	previousDead := -1
	killedWorkers := map[int]bool{}
	for i := range 20 {
		before, err := h.Status(ctx)
		require.NoError(t, err)
		require.Len(t, before.Calls, 1)
		owner, err := strconv.Atoi(before.Calls[0].Owner)
		require.NoError(t, err)
		killedWorkers[owner] = true
		killed := time.Now()
		require.NoError(t, h.Kill(owner))
		status := waitTakeovers(t, ctx, h, i+1)
		res := status.Takeovers[i]
		require.False(t, res.Lost)
		require.Empty(t, res.Error)
		assert.Equal(t, "takeover", res.Kind)
		assert.Equal(t, "takeover", status.Calls[0].LastMoveKind)
		assert.NotEqual(t, strconv.Itoa(owner), status.Calls[0].Owner)
		assert.NotNil(t, status.Calls[0].LastMove)
		assert.EqualValues(t, i+1, status.Calls[0].MoveCount)
		detection = append(detection, res.DetectedAt.Sub(killed))
		// Keep the previous source dead until the next takeover, so one
		// caller actually cycles through all three slots instead of always
		// picking the lexically first of two equally idle targets.
		if previousDead >= 0 {
			require.NoError(t, h.RestartWorker(previousDead))
		}
		previousDead = owner
		// Let the resumed video respond to PLI and decode before the next kill.
		time.Sleep(700 * time.Millisecond)
	}
	require.Len(t, killedWorkers, 3)
	require.NoError(t, h.RestartWorker(previousDead))
	require.NoError(t, <-sent)
	// As in #6, check consent for a full minute after the final takeover.
	if !testing.Short() {
		require.NoError(t, call.SendMedia(ctx, 60*time.Second))
	}
	report, err := call.Hangup(ctx)
	require.NoError(t, err)
	assertCleanCall(t, report)
	require.Len(t, report.Moves, 20)
	gaps := make([]time.Duration, 0, 20)
	for i, move := range report.Moves {
		require.Equal(t, "takeover", move.Kind)
		require.Empty(t, move.Error)
		assert.Zero(t, move.DecryptionFailuresAfterResume)
		gap := time.Duration(0)
		require.Len(t, move.Tracks, 2)
		for _, track := range move.Tracks {
			assert.Positive(t, track.PacketsAfter)
			skipped := track.SkippedSequenceNumbers
			// Replay starts at the post-margin snapshot index, which may lag
			// the last received packet. Check the original full-margin invariant
			// on new source media, excluding inserted replay packets.
			if track.Kind == "video" && move.Recovery.ReplayPackets > 0 {
				skipped = move.Recovery.SourceVideoSkippedSequenceNumbers
			}
			assert.GreaterOrEqual(t, skipped, 8192, "kill %d %s", i+1, track.Kind)
			assert.Less(t, track.Gap, 2*time.Second, "kill %d %s", i+1, track.Kind)
			if track.Kind == "video" {
				assert.Positive(t, track.FirstDecodableFrameAfter)
				assert.Less(t, track.FirstDecodableFrameAfter, 500*time.Millisecond)
			}
			gap = max(gap, track.Gap)
		}
		gaps = append(gaps, gap)
	}
	require.NotNil(t, report.Track("video"))
	assert.GreaterOrEqual(t, report.SentVideo.KeyframeRequests, 20, "PLI for every resumed video stream")
	assertVideoDecodes(t, report)
	sort.Slice(gaps, func(i, j int) bool { return gaps[i] < gaps[j] })
	sort.Slice(detection, func(i, j int) bool { return detection[i] < detection[j] })
	t.Logf("TAKEOVER_METRICS kills=20 median_gap=%s max_gap=%s detection_median=%s detection_max=%s decryption_failures=%d", (gaps[9]+gaps[10])/2, gaps[19], (detection[9]+detection[10])/2, detection[19], report.DecryptionFailures.Total())
	t.Log(report.Summary())
	if !testing.Short() {
		assert.GreaterOrEqual(t, report.Consent.ObservedFor, 60*time.Second)
		assert.Positive(t, report.Consent.ResponsesAfter)
	}
}

// This tests stale-state continuity and caller-observable recovery. Because
// echoed sequence numbers follow the caller, it does not independently prove
// the crypto margin prevents index reuse. FirstKeyframeTakeover guards that
// margin when the snapshot predates the first packets and exercises pending PLI.
func TestRelayStaleSnapshotTakeover(t *testing.T) {
	testTakeoverAtGate(t, false, false)
}

func TestRelayMidKeyframeTakeover(t *testing.T) {
	testTakeoverAtGate(t, true, false)
}

func TestRelayFirstKeyframeTakeover(t *testing.T) { testTakeoverAtGate(t, true, true) }

func testTakeoverAtGate(t *testing.T, midKeyframe, firstKeyframe bool) {
	t.Helper()
	h := startHarness(t, callharness.Options{Relay: true, Workers: 2})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	call, err := h.Dial(ctx, callharness.CallOptions{Video: true})
	require.NoError(t, err)
	var pendingPLI chan bool
	if firstKeyframe {
		require.NoError(t, h.WaitForSnapshot(ctx, 0, call.SessionID()))
		before, statusErr := h.Status(ctx)
		require.NoError(t, statusErr)
		// Hold background copies after the initial handshake snapshot. The first
		// echoed packet now triggers an immediate copy, but must not replace the
		// deliberately unanchored state this scenario needs to exercise.
		require.NoError(t, workerprobe.SetBeforeSnapshot(before.Workers[0].Address, func(gate context.Context, _ string) { <-gate.Done() }))
		pendingPLI = make(chan bool, 1)
		require.NoError(t, workerprobe.SetAfterResume(before.Workers[1].Address, func(id string, pending bool) {
			if id == call.SessionID() {
				pendingPLI <- pending
			}
		}))
	}
	sent := make(chan error, 1)
	go func() { sent <- call.SendMedia(ctx, 4*time.Second) }()
	if !firstKeyframe {
		time.Sleep(time.Second)
	}
	if midKeyframe {
		require.NoError(t, h.KillMidKeyframe(ctx, 0, call.SessionID(), 2))
	} else {
		require.NoError(t, h.KillBeforeSnapshot(ctx, 0, call.SessionID()))
	}
	status := waitTakeovers(t, ctx, h, 1)
	require.Equal(t, "1", status.Calls[0].Owner)
	if firstKeyframe {
		select {
		case pending := <-pendingPLI:
			require.True(t, pending, "resume must defer PLI until learning video SSRC")
		case <-ctx.Done():
			t.Fatal("pending PLI observation missing")
		}
	}
	require.NoError(t, <-sent)
	report, err := call.Hangup(ctx)
	require.NoError(t, err)
	assertCleanCall(t, report)
	require.Len(t, report.Moves, 1)
	assert.Positive(t, report.SentVideo.KeyframeRequests, "resume requested a keyframe")
	move := report.Moves[0]
	assert.Zero(t, move.DecryptionFailuresAfterResume)
	for _, track := range move.Tracks {
		assert.Less(t, track.Gap, 2*time.Second)
		minJump := 8192
		if firstKeyframe {
			minJump -= 100
		}
		// An early keyframe kill can precede the caller's first audio echo.
		// There is no observable sequence jump on a stream first seen after
		// resume. Still require resumed packets and clean decryption below.
		if !firstKeyframe || report.Track(track.Kind).FirstArrival < move.End {
			skipped := track.SkippedSequenceNumbers
			if track.Kind == "video" && move.Recovery.ReplayPackets > 0 {
				skipped = move.Recovery.SourceVideoSkippedSequenceNumbers
			}
			assert.GreaterOrEqual(t, skipped, minJump)
		}
		assert.Positive(t, track.PacketsAfter)
		if track.Kind == "video" {
			assert.Positive(t, track.FirstDecodableFrameAfter)
			assert.Less(t, track.FirstDecodableFrameAfter, 2*time.Second)
			t.Logf("GATED_TAKEOVER mid_keyframe=%t first_keyframe=%t gap=%s sequence_jump=%d first_decodable_after_resume=%s first_decodable_after_kill=%s detection=%s decryption_failures=%d", midKeyframe, firstKeyframe, track.Gap, track.SkippedSequenceNumbers, track.FirstDecodableFrameAfter, move.End-move.Start+track.FirstDecodableFrameAfter, move.DetectionTime, report.DecryptionFailures.Total())
		}
	}
	if midKeyframe {
		assert.Positive(t, report.Track("video").Video.IncompleteFrames, "the kill interrupted a frame at the caller")
	}
	assertVideoDecodes(t, report)
	t.Log(report.Summary())
}
