package callharness_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/relais/pkg/callharness"
	"github.com/stretchr/testify/require"
)

// Opt-in so the fast PR matrix does not gain a minutes-long call. Nightly uses
// RELAIS_LONG_RUN_DURATION=2m; the one-time local acceptance run uses 30m.
func TestLongRunRecording(t *testing.T) {
	value := os.Getenv("RELAIS_LONG_RUN_DURATION")
	if value == "" {
		t.Skip("set RELAIS_LONG_RUN_DURATION=2m (CI) or 30m (local acceptance)")
	}
	duration, err := time.ParseDuration(value)
	require.NoError(t, err)
	require.GreaterOrEqual(t, duration, 2*time.Minute)
	h, err := callharness.Start(callharness.Options{Workers: 2})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, h.Close()) })
	ctx, cancel := context.WithTimeout(context.Background(), duration+time.Minute)
	defer cancel()
	call, err := h.Dial(ctx, callharness.CallOptions{Video: true, Recording: callharness.RecordingOptions{History: 15 * time.Second, DecodeEvery: 30}})
	require.NoError(t, err)
	result, err := call.SendLongRun(ctx, callharness.LongRunOptions{Duration: duration, EventEvery: 30 * time.Second, SampleEvery: 30 * time.Second, HeapAfterGC: true, ObserveMemory: func(p callharness.MemorySample) {
		t.Logf("LONG_RUN at=%s heap_bytes=%d packets=%d units=%d frames=%d payload_bytes=%d summaries=%d", p.At.Round(time.Second), p.HeapAlloc, p.Recording.Packets, p.Recording.ContentUnits, p.Recording.FrameMarks, p.Recording.HistoryBytes, p.Recording.EventSummaries)
	}, Event: func(_ context.Context, i int) error {
		return call.Handover(callharness.HandoverOptions{To: (i + 1) % 2})
	}})
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(result.Memory), 4)
	warm := result.Memory[0].HeapAlloc
	for _, p := range result.Memory[1:] {
		require.LessOrEqual(t, p.HeapAlloc, warm+16*1024*1024, "heap after warm-up must remain within 16 MiB")
		require.LessOrEqual(t, p.Recording.ContentUnits, 1500)
		require.LessOrEqual(t, p.Recording.FrameMarks, 600)
		require.Zero(t, p.Recording.OfflineFrames)
	}
	rep := result.Call
	video := rep.Track("video").Video
	t.Logf("LONG_RUN duration=%s sent_video=%d events=%d sample_every=%d sampled=%d decoded=%d rate=%.4f errors=%d dropped=%d", duration, rep.SentVideo.Frames, len(rep.Moves), video.OnlineDecode.Every, video.OnlineDecode.Sampled, video.OnlineDecode.Decoded, video.OnlineDecode.SampleRate(video.Frames), video.OnlineDecode.Errors, video.OnlineDecode.Dropped)
	require.Zero(t, rep.DecryptionFailures.Total())
	require.True(t, rep.ConnectedThroughout())
	require.GreaterOrEqual(t, len(rep.Moves), int(duration/(30*time.Second))-1)
	newConclusive := 0
	for _, m := range rep.Moves {
		require.False(t, m.Measurement.Failed, m.Measurement.Summary())
		require.True(t, m.Measurement.AudioLoss.Trusted, m.Measurement.Summary())
		require.True(t, m.Measurement.VideoLoss.Trusted, m.Measurement.Summary())
		if m.Measurement.FirstNewContentVerdict.Trusted {
			newConclusive++
		}
	}
	require.True(t, callharness.EnoughConclusive(newConclusive, len(rep.Moves)), "first new content coverage %d/%d", newConclusive, len(rep.Moves))
	require.True(t, video.OnlineDecode.Conclusive(), "online decoder: %+v", video.OnlineDecode)
}
