package callharness

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestConsentObservedWhileWorkerEndsCall has the worker end a live call on
// its own, as its consent timer would: it sends a DTLS close_notify, and
// Pion closes the caller's PeerConnection by itself while the caller is
// sending media and observing its consent checks. With the race detector,
// this shows the harness touches nothing of the PeerConnection from the
// background while Pion closes it. The consent checks before the end were
// observed on the caller's socket.
func TestConsentObservedWhileWorkerEndsCall(t *testing.T) {
	h, err := Start(Options{})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, h.Close()) })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	call, err := h.Dial(ctx, CallOptions{Video: true})
	require.NoError(t, err)

	sent := make(chan error, 1)
	go func() { sent <- call.SendMedia(ctx, 4*time.Second) }()
	time.Sleep(2500 * time.Millisecond) // past the caller's first consent check after connecting

	require.NoError(t, h.workers.list[0].EndSession(call.SessionID()), "the worker ends the call")
	require.Eventually(t, func() bool { return call.pc.ConnectionState() == webrtc.PeerConnectionStateClosed },
		5*time.Second, 10*time.Millisecond, "the worker's close_notify closes the caller's connection")
	<-sent // the media sender may notice the closed connection; either way it ends

	report, err := call.Hangup(ctx)
	require.Error(t, err, "the call is already gone on the worker, so the DELETE fails")
	t.Logf("call the worker ended\n%s", report.Summary())

	states := make([]string, 0, len(report.ConnectionStates))
	for _, change := range report.ConnectionStates {
		states = append(states, change.State)
	}
	assert.True(t, slices.Contains(states, "closed"), "connection states: %v", states)
	assert.Positive(t, report.Consent.RequestsSent, "consent checks sent")
	assert.Positive(t, report.Consent.ResponsesReceived, "consent checks answered")
	assert.LessOrEqual(t, report.Consent.ResponsesReceived, report.Consent.RequestsSent, "answers match requests")
}
