package callharness_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/relais/pkg/callharness"
)

const (
	baselineCallDuration      = 30 * time.Second
	shortBaselineCallDuration = 5 * time.Second

	// maxBaselineMediaGap is generous on purpose: echoed Opus arrives every
	// 20 ms, and the headroom keeps the test stable under the race detector
	// on a busy CI runner.
	maxBaselineMediaGap = 500 * time.Millisecond
)

// TestBaselineOpusEchoCall is the baseline scenario: a caller dials with one
// offer/answer exchange, sends Opus audio for 30 s (5 s with -short) and
// hears it echoed back, with no move of any kind.
func TestBaselineOpusEchoCall(t *testing.T) {
	duration := baselineCallDuration
	if testing.Short() {
		duration = shortBaselineCallDuration
	}

	h, err := callharness.Start(callharness.Options{})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, h.Close()) })

	ctx, cancel := context.WithTimeout(context.Background(), duration+30*time.Second)
	defer cancel()

	call, err := h.Dial(ctx)
	require.NoError(t, err)
	require.NoError(t, call.SendAudio(ctx, duration))
	report, err := call.Hangup(ctx)
	require.NoError(t, err)
	t.Logf("%s call\n%s", duration, report.Summary())

	// Signaling: one HTTP offer/answer exchange; the answer advertises
	// ICE-lite, BUNDLE, rtcp-mux and a single host candidate.
	assert.Equal(t, 1, report.OfferAnswerExchanges, "offer/answer exchanges")
	assert.True(t, report.Answer.ICELite, "answer advertises a=ice-lite")
	assert.NotEmpty(t, report.Answer.Bundle, "answer has a=group:BUNDLE")
	assert.True(t, report.Answer.RTCPMux, "answer has a=rtcp-mux")
	assert.Len(t, report.Answer.Candidates, 1, "answer candidates")
	assert.Len(t, report.Answer.HostCandidates(), 1, "answer host candidates")

	// The connection reaches "connected" and stays there, with nothing the
	// caller has to repair.
	assert.True(t, report.ConnectedThroughout(), "connection states: %v", report.ConnectionStates)
	assert.Zero(t, report.DecryptionFailures.Total(), "SRTP decryption failures")
	assert.Zero(t, report.Renegotiations, "renegotiations")
	assert.Zero(t, report.ICERestarts, "ICE restarts")

	// The caller's own audio comes back on the worker's own track.
	require.Len(t, report.Tracks, 1, "received tracks")
	echo := report.Track("audio")
	require.NotNil(t, echo, "echoed audio track")
	assert.NotEqual(t, report.SentAudio.SSRC, echo.SSRC, "echo uses the worker's own SSRC")
	assert.EqualValues(t, 111, echo.PayloadType, "echo payload type")
	assert.Positive(t, report.SentAudio.Packets, "sent packets")
	assert.GreaterOrEqual(t, echo.Packets, report.SentAudio.Packets*95/100, "echoed packets vs sent")
	assert.Zero(t, echo.UnmatchedPayloads, "echoed payloads that the caller never sent")
	assert.Zero(t, echo.SequenceDiscontinuities, "echo is one continuous stream")
	assert.Less(t, echo.MediaGap, maxBaselineMediaGap, "media gap")
}
