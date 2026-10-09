package callharness_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/relais/pkg/callharness"
)

const (
	baselineCallDuration      = 30 * time.Second
	shortBaselineCallDuration = 5 * time.Second
	// shortCallDuration is for scenarios other than the baseline.
	shortCallDuration = 3 * time.Second

	// maxBaselineMediaGap is generous on purpose: echoed Opus arrives every
	// 20 ms and VP8 every 33 ms, and the headroom keeps the test stable under
	// the race detector on a busy CI runner.
	maxBaselineMediaGap = 500 * time.Millisecond

	// maxTimeToFirstDecodedFrame: the caller's first video frame is a
	// keyframe, so the echo normally decodes within tens of milliseconds of
	// connecting. If the worker misses that keyframe, the next one comes a
	// second later; the rest is headroom.
	maxTimeToFirstDecodedFrame = 2500 * time.Millisecond

	// requireFFmpegEnv makes a skipped full VP8 decode (no ffmpeg on PATH)
	// fail the test instead of being logged. CI sets it.
	requireFFmpegEnv = "RELAIS_HARNESS_REQUIRE_FFMPEG"
)

// answeredCodecs is the one codec the answer may carry for each media type.
var answeredCodecs = map[string]string{
	"audio": "opus/48000/2",
	"video": "VP8/90000",
}

// TestBaselineEchoCall is the baseline scenario: a caller dials with one
// offer/answer exchange, sends Opus audio and VP8 video on one bundled
// connection for 30 s (5 s with -short), and gets both echoed back, with no
// move of any kind. The echoed video must decode.
func TestBaselineEchoCall(t *testing.T) {
	duration := baselineCallDuration
	if testing.Short() {
		duration = shortBaselineCallDuration
	}

	report := runCall(t, callharness.CallOptions{Video: true}, duration, nil)

	assertSignaling(t, report, "audio", "video")
	assertCleanCall(t, report)
	assert.Len(t, report.Tracks, 2, "received tracks")
	assertAudioEcho(t, report)
	assertVideoEcho(t, report)
}

// TestAudioOnlyCall checks that a caller that offers only audio still gets
// its audio echoed.
func TestAudioOnlyCall(t *testing.T) {
	report := runCall(t, callharness.CallOptions{}, shortCallDuration, nil)

	assertSignaling(t, report, "audio")
	assertCleanCall(t, report)
	assert.Len(t, report.Tracks, 1, "received tracks")
	assertAudioEcho(t, report)
}

// TestBrowserLikeOfferCall makes a call with an offer shaped like a
// browser's: many codecs, RTX, RED, FEC, header extensions and an RTX
// ssrc-group. The answer picks only Opus and VP8, and the call echoes both.
func TestBrowserLikeOfferCall(t *testing.T) {
	report := runCall(t, callharness.CallOptions{Video: true, BrowserLikeOffer: true}, shortCallDuration, nil)

	for _, offered := range []string{
		"VP9/90000", "H264/90000", "AV1/90000", "rtx/90000", "ulpfec/90000", "red/48000/2",
		"telephone-event/48000", "a=extmap:", "a=ssrc-group:FID",
	} {
		assert.Contains(t, report.Offer, offered, "the caller's offer")
	}
	assertSignaling(t, report, "audio", "video")
	assertCleanCall(t, report)
	assertAudioEcho(t, report)
	assertVideoEcho(t, report)
}

// TestKeyframeRequestsAreRelayed checks that when the caller's receiver asks
// for a keyframe on the echoed video (PLI, then FIR), the caller's own video
// sender is asked for a keyframe in turn. The caller answers each request
// with a keyframe, and the echo keeps decoding.
func TestKeyframeRequestsAreRelayed(t *testing.T) {
	report := runCall(t, callharness.CallOptions{Video: true}, shortCallDuration,
		func(_ context.Context, call *callharness.Call) {
			time.Sleep(time.Second)
			require.NoError(t, call.RequestKeyframe(callharness.PLI))
			time.Sleep(500 * time.Millisecond)
			require.NoError(t, call.RequestKeyframe(callharness.FIR))
		})

	assert.Equal(t, 2, report.KeyframeRequestsSent, "keyframe requests the caller sent")
	assert.Equal(t, report.KeyframeRequestsSent, report.SentVideo.KeyframeRequests,
		"keyframe requests relayed to the caller's video SSRC")
	assert.Zero(t, report.SentAudio.KeyframeRequests, "keyframe requests on the caller's audio SSRC")
	assertCleanCall(t, report)
	assertVideoEcho(t, report)
}

// TestChromeOfferIsAnswered posts an offer in the shape Chrome sends (audio,
// video and a data channel; every codec Chrome offers; header extensions;
// an RTX ssrc-group) and checks the answer: Opus and VP8 only, keyframe
// feedback only, the data channel rejected and left out of BUNDLE.
func TestChromeOfferIsAnswered(t *testing.T) {
	raw, err := os.ReadFile("testdata/chrome-offer.sdp")
	require.NoError(t, err)
	offer := strings.ReplaceAll(string(raw), "\n", "\r\n") // browsers send CRLF

	h, err := callharness.Start(callharness.Options{})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, h.Close()) })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	answer, err := h.ExchangeOffer(ctx, offer)
	require.NoError(t, err)
	t.Logf("answer to the Chrome-shaped offer:\n%s", answer.SDP)

	assert.True(t, answer.ICELite, "answer advertises a=ice-lite")
	assert.True(t, answer.RTCPMux, "answer has a=rtcp-mux")
	assert.Equal(t, []string{"0", "1"}, answer.Bundle, "BUNDLE group")
	assert.Len(t, answer.HostCandidates(), 1, "answer host candidates")

	require.Len(t, answer.Media, 3, "answer m-lines")
	audio, video, data := answer.Media[0], answer.Media[1], answer.Media[2]

	assert.Equal(t, "audio", audio.Kind)
	assert.Equal(t, "0", audio.MID)
	assert.True(t, audio.Accepted(), "audio accepted")
	assert.Equal(t, []string{"111"}, audio.Formats, "audio formats")
	assert.Equal(t, []string{"opus/48000/2"}, audio.Codecs, "audio codecs")
	assert.Empty(t, audio.RTCPFeedback, "audio feedback")

	assert.Equal(t, "video", video.Kind)
	assert.Equal(t, "1", video.MID)
	assert.True(t, video.Accepted(), "video accepted")
	assert.Equal(t, []string{"96"}, video.Formats, "video formats")
	assert.Equal(t, []string{"VP8/90000"}, video.Codecs, "video codecs")
	assert.ElementsMatch(t, []string{"96 nack pli", "96 ccm fir"}, video.RTCPFeedback, "video feedback")

	assert.Equal(t, "application", data.Kind)
	assert.Equal(t, "2", data.MID)
	assert.False(t, data.Accepted(), "data channel rejected")

	assert.NotContains(t, answer.SDP, "a=extmap", "answer negotiates no header extensions")
	assert.NotContains(t, answer.SDP, "ssrc-group", "answer has no RTX")
}

// runCall starts the system, makes one call, sends media for duration while
// during (if any) runs alongside, hangs up and returns the caller's report.
func runCall(t *testing.T, opts callharness.CallOptions, duration time.Duration,
	during func(ctx context.Context, call *callharness.Call),
) *callharness.Report {
	t.Helper()

	h, err := callharness.Start(callharness.Options{})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, h.Close()) })

	ctx, cancel := context.WithTimeout(context.Background(), duration+30*time.Second)
	t.Cleanup(cancel)

	call, err := h.Dial(ctx, opts)
	require.NoError(t, err)

	sent := make(chan error, 1)
	go func() { sent <- call.SendMedia(ctx, duration) }()
	if during != nil {
		during(ctx, call)
	}
	require.NoError(t, <-sent)

	report, err := call.Hangup(ctx)
	require.NoError(t, err)
	t.Logf("%s call\n%s", duration, report.Summary())

	return report
}

// assertSignaling checks the one offer/answer exchange: ICE-lite, rtcp-mux,
// a single host candidate, and one bundled m-line with exactly one codec
// for each kind of media.
func assertSignaling(t *testing.T, report *callharness.Report, kinds ...string) {
	t.Helper()

	assert.Equal(t, 1, report.OfferAnswerExchanges, "offer/answer exchanges")
	assert.True(t, report.Answer.ICELite, "answer advertises a=ice-lite")
	assert.True(t, report.Answer.RTCPMux, "answer has a=rtcp-mux")
	assert.Len(t, report.Answer.Bundle, len(kinds), "answer BUNDLE group")
	assert.Len(t, report.Answer.Candidates, 1, "answer candidates")
	assert.Len(t, report.Answer.HostCandidates(), 1, "answer host candidates")
	for _, kind := range kinds {
		media := report.Answer.AcceptedMedia(kind)
		if !assert.NotNil(t, media, "answer accepts %s", kind) {
			continue
		}
		assert.Equal(t, []string{answeredCodecs[kind]}, media.Codecs, "answered %s codecs", kind)
		assert.Contains(t, report.Answer.Bundle, media.MID, "%s is bundled", kind)
	}
}

// assertCleanCall checks that the connection reached "connected" and stayed
// there, with nothing the caller had to repair.
func assertCleanCall(t *testing.T, report *callharness.Report) {
	t.Helper()

	assert.True(t, report.ConnectedThroughout(), "connection states: %v", report.ConnectionStates)
	assert.Zero(t, report.DecryptionFailures.Total(), "SRTP decryption failures")
	assert.Zero(t, report.Renegotiations, "renegotiations")
	assert.Zero(t, report.ICERestarts, "ICE restarts")
}

// assertAudioEcho checks that the caller's own audio comes back, complete,
// on the worker's own track.
func assertAudioEcho(t *testing.T, report *callharness.Report) {
	t.Helper()

	echo := report.Track("audio")
	require.NotNil(t, echo, "echoed audio track")
	assert.NotEqual(t, report.SentAudio.SSRC, echo.SSRC, "audio echo uses the worker's own SSRC")
	assert.EqualValues(t, 111, echo.PayloadType, "audio echo payload type")
	assert.Positive(t, report.SentAudio.Frames, "sent audio frames")
	assert.GreaterOrEqual(t, echo.Packets, report.SentAudio.Frames*95/100, "echoed audio packets vs sent")
	assert.Zero(t, echo.UnmatchedPayloads, "echoed audio payloads that the caller never sent")
	assert.Zero(t, echo.SequenceDiscontinuities, "audio echo is one continuous stream")
	assert.Less(t, echo.MediaGap, maxBaselineMediaGap, "audio media gap")
}

// assertVideoEcho checks that the caller's own video comes back on the
// worker's own track and that every echoed frame decodes.
func assertVideoEcho(t *testing.T, report *callharness.Report) {
	t.Helper()

	echo := report.Track("video")
	require.NotNil(t, echo, "echoed video track")
	require.NotNil(t, echo.Video, "echoed video report")
	video := echo.Video

	assert.NotEqual(t, report.SentVideo.SSRC, echo.SSRC, "video echo uses the worker's own SSRC")
	assert.EqualValues(t, 96, echo.PayloadType, "video echo payload type")
	assert.Zero(t, echo.SequenceDiscontinuities, "video echo is one continuous stream")
	assert.Less(t, echo.MediaGap, maxBaselineMediaGap, "video media gap")

	// Every frame comes back whole, in order, and byte for byte as sent.
	assert.Positive(t, report.SentVideo.Frames, "sent video frames")
	assert.GreaterOrEqual(t, video.Frames, report.SentVideo.Frames*95/100, "echoed video frames vs sent")
	assert.Zero(t, video.IncompleteFrames, "incomplete video frames")
	assert.Zero(t, video.FrameGaps, "video frame gaps")
	assert.Zero(t, video.UnmatchedFrames, "echoed video frames that the caller never sent")

	// The frames decode: keyframes in Go, and every frame after the first
	// keyframe belongs to an unbroken run from one.
	assert.Positive(t, video.KeyframesDecoded, "keyframes decoded")
	assert.Zero(t, video.KeyframeDecodeErrors, "keyframe decode errors: %s", video.LastDecodeError)
	assert.Zero(t, video.UndecodableFrames, "undecodable video frames")
	assert.Equal(t, video.Frames-video.FramesBeforeFirstKeyframe, video.DecodableFrames, "decodable video frames")
	assert.Equal(t, 320, video.Width, "decoded width")
	assert.Equal(t, 240, video.Height, "decoded height")
	firstFrame := report.TimeToFirstDecodedFrame()
	assert.Positive(t, firstFrame, "time to first decoded frame")
	assert.Less(t, firstFrame, maxTimeToFirstDecodedFrame, "time to first decoded frame")

	// ffmpeg decodes every frame, interframes included.
	full := video.FullDecode
	if !full.Ran() {
		if os.Getenv(requireFFmpegEnv) != "" {
			t.Errorf("full VP8 decode skipped (%s), but %s is set", full.Skipped, requireFFmpegEnv)
		} else {
			t.Logf("full VP8 decode SKIPPED: %s (install ffmpeg to run it)", full.Skipped)
		}

		return
	}
	assert.Empty(t, full.Errors, "ffmpeg decode errors")
	assert.Equal(t, video.Frames-video.FramesBeforeFirstKeyframe, full.FramesIn, "frames given to ffmpeg")
	assert.Equal(t, full.FramesIn, full.FramesDecoded, "frames ffmpeg decoded")
}
