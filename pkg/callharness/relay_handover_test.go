package callharness_test

import (
	"context"
	"encoding/json"
	"sort"
	"testing"
	"time"

	"github.com/relais/pkg/callharness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Twenty alternating planned moves on different private sockets, followed
// by a full minute of consent (including a media-free stretch).
func TestRelayPlannedHandover(t *testing.T) {
	h := startHarness(t, callharness.Options{Relay: true, Workers: 2})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	call, err := h.Dial(ctx, callharness.CallOptions{Video: true})
	require.NoError(t, err)
	initial, err := h.Status(ctx)
	require.NoError(t, err)
	require.Len(t, initial.Calls, 1)
	assert.Equal(t, "0", initial.Calls[0].Owner)
	assert.Nil(t, initial.Calls[0].LastMove)
	assert.Zero(t, initial.Calls[0].MoveCount)
	sent := make(chan error, 1)
	go func() { sent <- call.SendMedia(ctx, 8*time.Second) }()
	time.Sleep(time.Second)
	pairs := make([][2]int, 0, 20)
	for i := range 20 {
		to := (i + 1) % 2
		require.NoError(t, call.Handover(callharness.HandoverOptions{To: to}), "move %d", i+1)
		pairs = append(pairs, [2]int{i % 2, to})
		time.Sleep(250 * time.Millisecond)
	}
	status, err := h.Status(ctx)
	require.NoError(t, err)
	require.Len(t, status.Calls, 1)
	assert.Equal(t, call.SessionID(), status.Calls[0].ID)
	assert.Equal(t, "0", status.Calls[0].Owner)
	assert.NotEqual(t, status.Workers[0].Address, status.Workers[1].Address, "different private sockets")
	assert.Equal(t, status.Workers[0].Address, status.Calls[0].Address)
	assert.EqualValues(t, 21, status.Calls[0].Epoch)
	assert.EqualValues(t, 20, status.Calls[0].MoveCount)
	require.NotNil(t, status.Calls[0].LastMove)
	lastMove := *status.Calls[0].LastMove
	require.NoError(t, <-sent)
	if !testing.Short() {
		require.NoError(t, call.SendMedia(ctx, time.Until(lastMove.Add(20*time.Second))))
		time.Sleep(stunOnlyPause)
		require.NoError(t, call.SendMedia(ctx, time.Until(lastMove.Add(consentWindow))))
	}
	report, err := call.Hangup(ctx)
	require.NoError(t, err)
	t.Log(report.Summary())
	assertSignaling(t, report, "audio", "video")
	assertCleanCall(t, report)
	require.Len(t, report.Moves, 20)
	gaps := make([]time.Duration, 0, 20)
	for i, move := range report.Moves {
		assert.Empty(t, move.Error)
		assert.Equal(t, pairs[i], [2]int{move.From, move.To})
		require.Len(t, move.Tracks, 2)
		gap := time.Duration(0)
		for _, track := range move.Tracks {
			assert.Positive(t, track.PacketsAfter)
			assert.Zero(t, track.SkippedSequenceNumbers, "move %d %s", i+1, track.Kind)
			if track.Kind == "video" {
				assert.Less(t, track.FirstDecodableFrameAfter, maxHandoverGap, "move %d first decodable frame", i+1)
			}
			assert.Less(t, track.Gap, maxHandoverGap, "move %d %s", i+1, track.Kind)
			gap = max(gap, track.Gap)
		}
		gaps = append(gaps, gap)
	}
	sort.Slice(gaps, func(i, j int) bool { return gaps[i] < gaps[j] })
	median := (gaps[9] + gaps[10]) / 2
	logRelayMoveDetails(t, "MOVE_DETAILS", report.Moves)
	t.Logf("RELAY_METRICS moves=20 median_gap=%s max_gap=%s decryption_failures=%d consent_observed=%s consent_answered=%d consent_silence=%s", median, gaps[19], report.DecryptionFailures.Total(), report.Consent.ObservedFor, report.Consent.ResponsesAfter, report.Consent.LongestWithoutResponse)
	if !testing.Short() {
		assert.GreaterOrEqual(t, report.Consent.ObservedFor, consentWindow)
		assert.GreaterOrEqual(t, report.Consent.ResponsesAfter, uint64(consentWindow/(2*callerCheckInterval)))
		assert.Less(t, report.Consent.LongestWithoutResponse, maxConsentSilence)
	}
	assertVideoDecodes(t, report)
}

func TestRelayDrainTenCalls(t *testing.T) {
	h := startHarness(t, callharness.Options{Relay: true, Workers: 3})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	calls := make([]*callharness.Call, 10)
	sent := make(chan error, len(calls))
	for i := range calls {
		call, err := h.Dial(ctx, callharness.CallOptions{Video: true, Worker: 0})
		require.NoError(t, err)
		calls[i] = call
	}
	for _, call := range calls {
		go func() { sent <- call.SendMedia(ctx, 4*time.Second) }()
	}
	time.Sleep(time.Second)
	start := time.Now()
	require.NoError(t, h.Drain(0))
	duration := time.Since(start)
	assert.Less(t, duration, time.Second, "ten calls moved in total")
	status, err := h.Status(ctx)
	require.NoError(t, err)
	assert.True(t, status.Workers[0].Draining)
	assert.Zero(t, status.Workers[0].Calls)
	require.Len(t, status.Calls, 10)
	for _, call := range status.Calls {
		assert.NotEqual(t, "0", call.Owner)
		assert.NotNil(t, call.LastMove)
		assert.EqualValues(t, 1, call.MoveCount)
	}
	assert.Equal(t, 5, status.Workers[1].Calls)
	assert.Equal(t, 5, status.Workers[2].Calls)
	_, err = h.Dial(ctx, callharness.CallOptions{Worker: 0})
	require.Error(t, err, "draining worker gets no calls")
	for range calls {
		require.NoError(t, <-sent)
	}
	maxGap := time.Duration(0)
	for i, call := range calls {
		report, err := call.Hangup(ctx)
		require.NoError(t, err)
		assertCleanCall(t, report)
		logRelayMoveDetails(t, "DRAIN_DETAILS", report.Moves)
		require.Len(t, report.Moves, 1)
		for _, track := range report.Moves[0].Tracks {
			assert.Positive(t, track.PacketsAfter)
			assert.Zero(t, track.SkippedSequenceNumbers, "call %d %s", i, track.Kind)
			if track.Kind == "video" {
				assert.Less(t, track.FirstDecodableFrameAfter, maxHandoverGap, "call %d first decodable frame", i)
			}
			assert.Less(t, track.Gap, maxHandoverGap, "call %d %s", i, track.Kind)
			maxGap = max(maxGap, track.Gap)
		}
		assertVideoDecodes(t, report)
	}
	t.Logf("DRAIN_METRICS calls=10 total=%s max_call_gap=%s", duration, maxGap)
}

func logRelayMoveDetails(t *testing.T, label string, moves []callharness.MoveReport) {
	t.Helper()
	type detail struct {
		Move       int
		AudioLost  int
		VideoLost  int
		FirstFrame time.Duration
		Held       int
	}
	records := make([]detail, 0, len(moves))
	for i, move := range moves {
		d := detail{Move: i + 1, Held: move.Result.HeldPackets}
		for _, track := range move.Tracks {
			if track.Kind == "audio" {
				d.AudioLost = track.SkippedSequenceNumbers
			}
			if track.Kind == "video" {
				d.VideoLost = track.SkippedSequenceNumbers
				d.FirstFrame = track.FirstDecodableFrameAfter
			}
		}
		records = append(records, d)
	}
	raw, err := json.Marshal(records)
	require.NoError(t, err)
	t.Logf("%s %s", label, raw)
}
