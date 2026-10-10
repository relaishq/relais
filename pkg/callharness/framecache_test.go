package callharness_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"testing"
	"time"

	"crypto/rand"
	"encoding/hex"
	"github.com/relais/pkg/callharness"
	"github.com/relais/pkg/framecache"
	"github.com/relais/pkg/sessionstore"
	"github.com/relais/pkg/storage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// PLI rewinds an encoded keyframe on the next 33 ms tick, with no encoder
// delay. Real browser latency is measured separately in #13.
// Ten independent crashes per mode compare the two recovery paths. The
// source's PictureID plus encoded payload disambiguates a cached keyframe
// from the identical IVF keyframe sent anew in response to a PLI.
func TestRelayFrameCacheModes(t *testing.T) {
	forSessionStores(t, testRelayFrameCacheModes)
}

func testRelayFrameCacheModes(t *testing.T, store sessionstore.Store) {
	frames := selectedFrameCache(t, store)
	for _, mode := range []struct {
		name       string
		cache, pli bool
	}{{"cache", true, false}, {"keyframe", false, true}, {"both", true, true}} {
		t.Run(mode.name, func(t *testing.T) {
			var times, liveTimes []time.Duration
			paths := map[string]int{}
			liveReasons := map[string]int{}
			within := 0
			for trial := range 10 {
				t.Run(fmt.Sprint(trial), func(t *testing.T) {
					h := startHarness(t, callharness.Options{Relay: true, SessionStore: store, FrameCache: frames, Workers: 2, DisableFrameCache: !mode.cache, DisableResumePLI: !mode.pli})
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					call, err := h.Dial(ctx, callharness.CallOptions{Video: true})
					require.NoError(t, err)
					sent := make(chan error, 1)
					go func() { sent <- call.SendMedia(ctx, 3*time.Second) }()
					time.Sleep(550*time.Millisecond + time.Duration(trial)*7*time.Millisecond)
					require.NoError(t, h.Kill(0))
					status := waitTakeovers(t, ctx, h, 1)
					require.False(t, status.Takeovers[0].Lost)
					require.NoError(t, <-sent)
					report, err := call.Hangup(ctx)
					require.NoError(t, err)
					assertCleanCall(t, report)
					assertVideoDecodes(t, report)
					assert.Zero(t, report.Track("video").Video.NonMonotonicTimestamps)
					require.Len(t, report.Moves, 1)
					recovery := report.Moves[0].Recovery
					stats, err := h.ReplayStats(1)
					require.NoError(t, err)
					require.Positive(t, recovery.MediaResumedAt)
					require.NotEmpty(t, recovery.Path)
					t.Logf("RECOVERY_TRIAL mode=%s trial=%d path=%s replay_packets=%d skipped=%v truncated=%v first_decoded_from_kill=%s first_live_from_kill=%s live_path=%s from_media_resume=%s interval=%s keyframe_interval=%s within=%t picture_id=%d", mode.name, trial, recovery.Path, recovery.ReplayPackets, stats.Skipped, stats.Truncated, recovery.FirstDecodedAfterKill, recovery.FirstDecodedLiveAfterKill, recovery.LivePath, recovery.FirstDecodedAfterMediaResume, recovery.FrameInterval, recovery.KeyframeInterval, recovery.WithinFrameInterval, recovery.PictureID)
					if mode.cache {
						require.GreaterOrEqual(t, recovery.SourceVideoSkippedSequenceNumbers, 8192, "source margin survives replay insertion")
					}
					if mode.name == "cache" {
						// The bounded burst may be skipped or interrupted by its deadline.
						// Cache-only then waits for the caller's natural keyframe. Every
						// skip and truncation is counted by the worker, so a Live result
						// without one would be a silently lost burst and fails the test.
						if recovery.Path == "Live" {
							require.True(t, len(stats.Skipped) > 0 || len(stats.Truncated) > 0, "Live requires a worker-reported replay skip or truncation")
						} else {
							require.Equal(t, "Cache", recovery.Path)
						}
						assert.Zero(t, report.SentVideo.KeyframeRequests)
					}
					if mode.name == "keyframe" {
						require.Equal(t, "Keyframe", recovery.Path)
					}
					if mode.pli {
						assert.Positive(t, report.SentVideo.KeyframeRequests)
					}
					// A generous liveness bound applies under -race too. The one-frame
					// target is measured below, never silently replaced by this bound.
					bound := 500 * time.Millisecond
					if raceDetector {
						bound = time.Second
					}
					if mode.name == "cache" && recovery.Path != "Cache" {
						bound = recovery.KeyframeInterval + 500*time.Millisecond
					}
					assert.Less(t, recovery.FirstDecodedAfterMediaResume, bound)
					times = append(times, recovery.FirstDecodedAfterKill)
					if recovery.FirstDecodedLiveAfterKill > 0 {
						liveTimes = append(liveTimes, recovery.FirstDecodedLiveAfterKill)
					}
					paths[recovery.Path]++
					if recovery.Path == "Live" {
						why := "no-replay-packets"
						if len(stats.Truncated) > 0 {
							why = fmt.Sprint("truncated:", stats.Truncated)
						} else if len(stats.Skipped) > 0 {
							why = fmt.Sprint("skipped:", stats.Skipped)
						}
						liveReasons[why]++
					}
					if recovery.WithinFrameInterval {
						within++
					}
				})
			}
			sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
			require.Len(t, times, 10)
			sort.Slice(liveTimes, func(i, j int) bool { return liveTimes[i] < liveTimes[j] })
			n := len(times)
			median := (times[(n-1)/2] + times[n/2]) / 2
			liveMedian, liveMax := time.Duration(0), time.Duration(0)
			if l := len(liveTimes); l > 0 {
				liveMedian = (liveTimes[(l-1)/2] + liveTimes[l/2]) / 2
				liveMax = liveTimes[l-1]
			}
			t.Logf("FRAMECACHE_METRICS mode=%s takeovers=%d common_start=kill median=%s max=%s live_median=%s live_max=%s live_observed=%d/%d interval=%s keyframe_interval=1s within_interval=%d/%d attribution=%v live_reasons=%v", mode.name, n, median, times[n-1], liveMedian, liveMax, len(liveTimes), n, time.Second/30, within, n, paths, liveReasons)
		})
	}
}

func TestRelayFrameCacheMidKeyframe(t *testing.T) {
	forSessionStores(t, testRelayFrameCacheMidKeyframe)
}

func testRelayFrameCacheMidKeyframe(t *testing.T, store sessionstore.Store) {
	frames := selectedFrameCache(t, store)
	h := startHarness(t, callharness.Options{Relay: true, SessionStore: store, FrameCache: frames, Workers: 2, DisableResumePLI: true, ReplayMaxBurstDuration: 50 * time.Millisecond})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	call, err := h.Dial(ctx, callharness.CallOptions{Video: true})
	require.NoError(t, err)
	sent := make(chan error, 1)
	go func() { sent <- call.SendMedia(ctx, 3*time.Second) }()
	time.Sleep(500 * time.Millisecond)
	require.NoError(t, h.KillMidKeyframe(ctx, 0, call.SessionID(), 2))
	waitTakeovers(t, ctx, h, 1)
	require.NoError(t, <-sent)
	report, err := call.Hangup(ctx)
	require.NoError(t, err)
	assertCleanCall(t, report)
	assertVideoDecodes(t, report)
	assert.Zero(t, report.Track("video").Video.NonMonotonicTimestamps)
	recovery := report.Moves[0].Recovery
	require.Equal(t, "Cache", recovery.Path)
	// The interrupted second keyframe is PictureID 30. The preceding complete
	// group starts at PictureID 0; only that complete keyframe may be replayed.
	require.EqualValues(t, 0, recovery.PictureID)
	require.Len(t, recovery.CachedPictureIDs, 30, "the whole previous complete group was replayed")
	for i, id := range recovery.CachedPictureIDs {
		require.EqualValues(t, i, id)
	}
	require.NotContains(t, recovery.CachedPictureIDs, uint16(30), "no packet of the interrupted keyframe may be replayed")
	assert.Positive(t, report.Track("video").Video.IncompleteFrames)
	assert.Zero(t, report.SentVideo.KeyframeRequests)
	t.Logf("MID_KEYFRAME_CACHE first_decoded=%s interval=%s path=%s picture_id=%d", recovery.FirstDecodedAfterMediaResume, recovery.FrameInterval, recovery.Path, recovery.PictureID)
}

// Recovery must not wait for a new source packet or for a PLI response. The
// caller sends nothing after the kill; only the shared cached group can decode.
func TestRelayFrameCacheWithoutSource(t *testing.T) {
	h := startHarness(t, callharness.Options{Relay: true, Workers: 2, DisableResumePLI: true})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	call, err := h.Dial(ctx, callharness.CallOptions{Video: true})
	require.NoError(t, err)
	require.NoError(t, call.SendMedia(ctx, 450*time.Millisecond))
	require.NoError(t, h.Kill(0))
	status := waitTakeovers(t, ctx, h, 1)
	require.False(t, status.Takeovers[0].Lost)
	time.Sleep(100 * time.Millisecond)
	report, err := call.Hangup(ctx)
	require.NoError(t, err)
	assertCleanCall(t, report)
	assertVideoDecodes(t, report)
	require.Equal(t, "Cache", report.Moves[0].Recovery.Path)
	assert.Zero(t, report.SentVideo.KeyframeRequests)
	assert.Zero(t, report.Track("video").Video.NonMonotonicTimestamps)
	t.Logf("NO_SOURCE_CACHE first_decoded=%s interval=%s", report.Moves[0].Recovery.FirstDecodedAfterMediaResume, report.Moves[0].Recovery.FrameInterval)
}

// A three-second natural keyframe interval and denser 640x480 media exercise
// replay pressure and whole-group skipping. Other calls share the target's
// UDP reader; their gaps must not grow with the burst's packet count.
func TestRelayFrameCacheLargeGroup(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		if os.Getenv("RELAIS_HARNESS_REQUIRE_FFMPEG") == "1" {
			t.Fatal(err)
		}
		t.Skip("ffmpeg required to create the dense VP8 fixture")
	}
	cmd := exec.Command(ffmpeg, "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=640x480:rate=30:duration=4", "-pix_fmt", "yuv420p", "-c:v", "libvpx", "-b:v", "1500k", "-g", "90", "-keyint_min", "90", "-auto-alt-ref", "0", "-lag-in-frames", "0", "-deadline", "realtime", "-cpu-used", "8", "-error-resilient", "1", "-f", "ivf", "pipe:1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	fixture, err := cmd.Output()
	require.NoError(t, err, stderr.String())
	for _, scenario := range []struct {
		name     string
		killAt   time.Duration
		replay   bool
		maxBurst time.Duration
		cacheOff bool
	}{
		{"default-cap", 600 * time.Millisecond, false, 0, false},
		{"replay", 600 * time.Millisecond, true, 200 * time.Millisecond, false},
		{"cache-off", 600 * time.Millisecond, false, 0, true},
		{"over-cap", 1400 * time.Millisecond, false, 0, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			h := startHarness(t, callharness.Options{Relay: true, Workers: 2, DisableFrameCache: scenario.cacheOff, ReplayMaxBurstDuration: scenario.maxBurst})
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			defer cancel()
			target, err := h.Dial(ctx, callharness.CallOptions{Video: true, VideoData: fixture})
			require.NoError(t, err)
			var others []*callharness.Call
			for range 3 {
				call, err := h.Dial(ctx, callharness.CallOptions{Video: true, Worker: 1})
				require.NoError(t, err)
				others = append(others, call)
			}
			done := make(chan error, 4)
			for _, call := range append(others, target) {
				go func() { done <- call.SendMedia(ctx, 3*time.Second) }()
			}
			time.Sleep(scenario.killAt)
			require.NoError(t, h.Kill(0))
			require.Eventually(t, func() bool {
				status, err := h.Status(ctx)
				require.NoError(t, err)
				if len(status.Takeovers) != 1 {
					return false
				}
				require.False(t, status.Takeovers[0].Lost)
				return !status.Workers[0].Recovering
			}, 2*time.Second, 5*time.Millisecond)
			for range 4 {
				require.NoError(t, <-done)
			}
			report, err := target.Hangup(ctx)
			require.NoError(t, err)
			assertCleanCall(t, report)
			assertVideoDecodes(t, report)
			recovery := report.Moves[0].Recovery
			if scenario.replay {
				require.Positive(t, recovery.ReplayPackets)
				require.Equal(t, "Cache", recovery.Path)
			} else {
				require.Zero(t, recovery.ReplayPackets)
				require.Equal(t, "Keyframe", recovery.Path)
			}
			maxAudio, maxVideo := time.Duration(0), time.Duration(0)
			burstStart := recovery.MediaResumedAt
			// All callers have their own recorder epoch. Align to wall time via
			// move.Start plus the epochs' difference when selecting the burst window.
			for i, call := range others {
				other, err := call.Hangup(ctx)
				require.NoError(t, err)
				assertCleanCall(t, other)
				assertVideoDecodes(t, other)
				audio, video := time.Duration(0), time.Duration(0)
				offset := report.StartedAt.Sub(other.StartedAt)
				for _, track := range other.Tracks {
					for j := 1; j < len(track.Arrivals); j++ {
						at := track.Arrivals[j] - offset
						if at < burstStart-50*time.Millisecond || at > burstStart+250*time.Millisecond {
							continue
						}
						gap := track.Arrivals[j] - track.Arrivals[j-1]
						if track.Kind == "audio" {
							audio = max(audio, gap)
						} else {
							video = max(video, gap)
						}
					}
				}
				maxAudio, maxVideo = max(maxAudio, audio), max(maxVideo, video)
				bound := 100 * time.Millisecond
				if raceDetector {
					bound = 250 * time.Millisecond
				}
				assert.Less(t, audio, bound)
				assert.Less(t, video, bound)
				t.Logf("REPLAY_OTHER_CALL scenario=%s call=%d audio_gap=%s video_gap=%s", scenario.name, i, audio, video)
			}
			t.Logf("LARGE_REPLAY scenario=%s max_burst=%s replay_packets=%d first_from_kill=%s live_from_kill=%s keyframe_interval=%s other_audio_max=%s other_video_max=%s", scenario.name, scenario.maxBurst, recovery.ReplayPackets, recovery.FirstDecodedAfterKill, recovery.FirstDecodedLiveAfterKill, recovery.KeyframeInterval, maxAudio, maxVideo)
		})
	}
}

// The Redis session-store variant also selects Redis media retention. Workers
// get the caller-owned Store through the production Options.FrameCache seam.
func selectedFrameCache(t *testing.T, store sessionstore.Store) framecache.Store {
	t.Helper()
	if store == nil {
		return nil
	}
	var token [16]byte
	_, err := rand.Read(token[:])
	require.NoError(t, err)
	frames, err := framecache.NewRedis(context.Background(), storage.RedisConfig{Addr: os.Getenv("RELAIS_TEST_REDIS_ADDR"), Prefix: "harness:frames:" + hex.EncodeToString(token[:]) + ":"}, make([]byte, 32), framecache.Limits{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, frames.Close()) })
	return frames
}
