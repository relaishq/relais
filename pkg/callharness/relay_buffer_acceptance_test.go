//go:build !race

package callharness

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/workerprobe"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

// The strict timing lane observes the full freshness stability window.
// Race coverage uses TestRelayBufferTakeover and the agent replay regressions.
func TestAcceptanceRelayBufferTakeover(t *testing.T) {
	relayBufferStores(t, func(t *testing.T, store sessionstore.Store) {
		for _, enabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("buffer=%t", enabled), func(t *testing.T) {
				sys := startBufferHarness(t, store, enabled)
				policy := StrictAcceptancePolicy()
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				call, err := sys.h.Dial(ctx, CallOptions{Video: true, Freshness: policy, InitialSequenceNumbers: &RTPSequenceNumbers{Audio: 1000, Video: 2000}})
				require.NoError(t, err)
				sent := make(chan error, 1)
				go func() { sent <- call.SendMedia(ctx, policy.EvaluationWindow()+2500*time.Millisecond) }()
				time.Sleep(1250 * time.Millisecond)
				sys.h.RecordProcessKill("0", time.Now())
				require.NoError(t, workerprobe.Kill(sys.workers[0].LocalAddr()))
				require.NoError(t, <-sent)
				workerFailures, err := workerInputRejections(sys.workers[1], call.SessionID())
				require.NoError(t, err)
				report, err := call.Hangup(ctx)
				require.NoError(t, err)
				require.Len(t, report.Moves, 1)
				move := report.Moves[0]
				event := move.Measurement
				t.Logf("RELAY_BUFFER_ACCEPTANCE store=%s buffer=%t lost_audio_ms=%.3f lost_video_frames=%d first_new_content=%q audio_pause_ms=%.3f video_pause_ms=%.3f audio_freshness_ms=%.3f video_freshness_ms=%.3f audio_freshness_trusted=%t video_freshness_trusted=%t caller_decrypt_failures=%d worker_decrypt_failures=%d\n%s", t.Name(), enabled, float64(event.LostAudio)/float64(time.Millisecond), event.LostVideoFrames, event.FirstNewContent, float64(event.AudioPause)/float64(time.Millisecond), float64(event.VideoPause)/float64(time.Millisecond), float64(event.Audio.BackToBaseline)/float64(time.Millisecond), float64(event.Video.BackToBaseline)/float64(time.Millisecond), event.Audio.Verdict.Trusted, event.Video.Verdict.Trusted, report.DecryptionFailures.Total(), workerFailures, event.Summary())
				video := report.Track("video").Video
				t.Logf("RELAY_BUFFER_DECODER buffer=%t nonmonotonic=%d undecodable=%d full_decode_frames=%d full_decode_errors=%q", enabled, video.NonMonotonicTimestamps, video.UndecodableFrames, video.FullDecode.FramesDecoded, video.FullDecode.Errors)
				require.Empty(t, move.Error)
				require.True(t, report.ConnectedThroughout())
				require.Zero(t, report.DecryptionFailures.Total())
				require.Zero(t, workerFailures)
				require.True(t, event.AudioLoss.Trusted, event.Summary())
				require.True(t, event.VideoLoss.Trusted, event.Summary())
				require.True(t, event.FirstNewContentVerdict.Trusted, event.Summary())
				if enabled {
					require.Zero(t, event.LostAudio, event.Summary())
					require.Zero(t, event.LostVideoFrames, event.Summary())
					RequireFreshnessRecovered(t, event, policy)
					require.Equal(t, "outage media", event.FirstNewContent, event.Summary())
				} else {
					require.Positive(t, event.LostAudio, event.Summary())
					require.Positive(t, event.LostVideoFrames, event.Summary())
					require.Equal(t, "keyframe", event.FirstNewContent, event.Summary())
				}
			})
		}
	})
}
