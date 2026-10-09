package callharness

import (
	"context"
	"net"
	"slices"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestConsentWindowEndsAtHangup: the consent window runs to hangup (or to
// the connection closing, if earlier), and the silent stretch after the
// last answer counts toward the longest time without one.
func TestConsentWindowEndsAtHangup(t *testing.T) {
	sec := func(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }
	rec := newRecorder()
	rec.connected, rec.connectedAt = true, sec(0.2)
	rec.consent = []consentSample{
		{at: sec(1), requests: 1}, {at: sec(1), requests: 1, responses: 1},
		{at: sec(3), requests: 2}, {at: sec(3), requests: 2, responses: 2},
	}
	rec.hungUp, rec.hungUpAt = true, sec(10)

	report := rec.consentReport()
	assert.Equal(t, sec(9.8), report.ObservedFor, "window to hangup")
	assert.Equal(t, sec(7), report.LongestWithoutResponse, "the silent tail from the last answer to hangup")
	assert.EqualValues(t, 2, report.ResponsesAfter)

	rec.connectionStates = []StateChange{{At: sec(0.2), State: "connected"}, {At: sec(5), State: "closed"}}
	report = rec.consentReport()
	assert.Equal(t, sec(4.8), report.ObservedFor, "window to the connection closing")
	assert.Equal(t, sec(2), report.LongestWithoutResponse, "the tail ends when the connection closed")
}

// TestConsentCountsOnlySentRequests: a binding request whose write fails is
// not counted as sent, and a late answer to it is not counted either.
func TestConsentCountsOnlySentRequests(t *testing.T) {
	rec := newRecorder()
	socket, err := newCallerSocket(rec)
	require.NoError(t, err)
	observer := socket.observer
	request := []byte{0x00, 0x01, 0x00, 0x00, 0x21, 0x12, 0xa4, 0x42, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
	to := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9}

	_, err = observer.WriteTo(request, to)
	require.NoError(t, err)
	require.NoError(t, socket.close())
	_, err = observer.WriteTo(request, to)
	require.Error(t, err, "write on the closed socket")

	rec.mu.Lock()
	defer rec.mu.Unlock()
	require.Len(t, rec.consent, 1, "only the request that was written counts")
	assert.EqualValues(t, 1, rec.consent[0].requests)
	assert.Empty(t, observer.unanswered, "the failed request awaits no answer")
}

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
