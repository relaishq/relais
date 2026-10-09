package callharness_test

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/relais/pkg/callharness"
)

// Calls through the relay. The relay owns the one public address every
// answer advertises; the media workers sit behind it on private sockets.

const (
	relayCallDuration      = 10 * time.Second
	shortRelayCallDuration = 5 * time.Second

	// relayRestarts relay restarts happen during one call, relayRestartEvery
	// apart. That is longer than the Pion caller's 2 s consent-check
	// interval, so each restart recovers before the next, and it is off the
	// 2 s grid, so the restarts land at different points between two checks
	// and the gaps sample the range a caller can see.
	relayRestarts     = 4
	relayRestartEvery = 2500 * time.Millisecond

	// maxRelayRestartGap bounds the caller's media gap across a relay
	// restart. A restarted relay drops the caller's media until the caller's
	// next ICE consent check rebuilds its flow from the session-owner store;
	// the Pion caller sends one every 2 s. The rest is headroom for the race
	// detector, and it stays under Pion's 5 s disconnected timeout, which the
	// connection-state check would catch anyway.
	maxRelayRestartGap = 4 * time.Second

	// restartSettle is how long after a relay restart packets that were
	// already in flight may still arrive; the restart gap starts within it.
	restartSettle = 100 * time.Millisecond
)

// TestNegativeWorkersAreRejected: a negative worker count is a mistake in
// either topology, not one worker.
func TestNegativeWorkersAreRejected(t *testing.T) {
	for _, relay := range []bool{false, true} {
		h, err := callharness.Start(callharness.Options{Workers: -1, Relay: relay})
		if !assert.Error(t, err, "Workers: -1, Relay: %t", relay) {
			assert.NoError(t, h.Close())
		}
	}
}

// TestEchoCallThroughRelay is the baseline call through the relay: Opus and
// VP8 echoed back cleanly, with the relay's address as the only address the
// caller ever sees.
func TestEchoCallThroughRelay(t *testing.T) {
	duration := relayCallDuration
	if testing.Short() {
		duration = shortRelayCallDuration
	}
	h := startHarness(t, callharness.Options{Relay: true})

	report := runCallOn(t, h, callharness.CallOptions{Video: true}, duration, nil)

	assertSignaling(t, report, "audio", "video")
	assertCleanCall(t, report)
	assertAudioEcho(t, report)
	assertVideoEcho(t, report)
	assertThroughRelay(t, h, report)
}

// TestConcurrentCallsOnTwoWorkersThroughRelay makes two calls at once, each
// owned by a different worker behind the one relay address. Each caller
// gets its own media back cleanly: a packet the relay delivered to the wrong
// worker, or a worker's packet delivered to the wrong caller, would fail
// SRTP decryption (each session has its own keys) and show up as missing
// echo or decryption failures.
func TestConcurrentCallsOnTwoWorkersThroughRelay(t *testing.T) {
	h := startHarness(t, callharness.Options{Relay: true, Workers: 2})
	ctx, cancel := context.WithTimeout(context.Background(), shortRelayCallDuration+30*time.Second)
	t.Cleanup(cancel)

	calls := make([]*callharness.Call, 2)
	for worker := range calls {
		call, err := h.Dial(ctx, callharness.CallOptions{Video: true, Worker: worker})
		require.NoError(t, err, "dial worker %d", worker)
		calls[worker] = call

		owner, ok := h.SessionOwner(call.SessionID())
		require.True(t, ok, "call %d has an owner in the session-owner store", worker)
		require.Equal(t, worker, owner, "call %d's owner", worker)
	}

	sent := make(chan error, len(calls))
	for _, call := range calls {
		go func() { sent <- call.SendMedia(ctx, shortRelayCallDuration) }()
	}
	for range calls {
		require.NoError(t, <-sent)
	}

	for worker, call := range calls {
		report, err := call.Hangup(ctx)
		require.NoError(t, err)
		t.Logf("call on worker %d\n%s", worker, report.Summary())

		assertSignaling(t, report, "audio", "video")
		assertCleanCall(t, report)
		assertAudioEcho(t, report)
		assertVideoEcho(t, report)
		assertThroughRelay(t, h, report)
	}
}

// TestRelayRestartMidCall restarts the relay several times during a call.
// The new relay starts with an empty flow table on the same address; the
// call carries on once the caller's next consent check rebuilds its flow
// from the session-owner store. The caller's media gap across each restart
// is measured and logged, and the connection must stay up throughout with
// no ICE restart, renegotiation or decryption failure.
func TestRelayRestartMidCall(t *testing.T) {
	h := startHarness(t, callharness.Options{Relay: true})
	duration := relayRestarts*relayRestartEvery + 2*time.Second
	ctx, cancel := context.WithTimeout(context.Background(), duration+30*time.Second)
	t.Cleanup(cancel)

	dialed := time.Now() // the report's time zero, give or take microseconds
	call, err := h.Dial(ctx, callharness.CallOptions{Video: true})
	require.NoError(t, err)

	sent := make(chan error, 1)
	go func() { sent <- call.SendMedia(ctx, duration) }()

	type restart struct {
		at   time.Duration // since dialing
		down time.Duration // no relay listening
	}
	var restarts []restart
	for range relayRestarts {
		time.Sleep(relayRestartEvery)
		at := time.Since(dialed)
		down, err := h.RestartRelay()
		require.NoError(t, err)
		restarts = append(restarts, restart{at: at, down: down})
	}
	require.NoError(t, <-sent)

	report, err := call.Hangup(ctx)
	require.NoError(t, err)
	t.Logf("call with %d relay restarts\n%s", relayRestarts, report.Summary())

	assert.True(t, report.ConnectedThroughout(), "connection states: %v", report.ConnectionStates)
	assert.Zero(t, report.DecryptionFailures.Total(), "SRTP decryption failures")
	assert.Zero(t, report.Renegotiations, "renegotiations")
	assert.Zero(t, report.ICERestarts, "ICE restarts")
	assertThroughRelay(t, h, report)

	for _, kind := range []string{"audio", "video"} {
		track := report.Track(kind)
		require.NotNil(t, track, "echoed %s track", kind)
		for i, r := range restarts {
			gap, resumedAt, ok := restartGap(track.Arrivals, r.at)
			require.True(t, ok, "%s echo resumed after relay restart %d at %s", kind, i+1, r.at)
			t.Logf("relay restart %d at %s: relay down %s; caller %s gap %s, media back at %s",
				i+1, r.at.Round(time.Millisecond), r.down.Round(time.Microsecond), kind,
				gap.Round(time.Millisecond), resumedAt.Round(time.Millisecond))
			assert.Less(t, gap, maxRelayRestartGap, "%s gap across relay restart %d", kind, i+1)
		}
	}
}

// startHarness starts the system with opts and closes it when the test ends.
func startHarness(t *testing.T, opts callharness.Options) *callharness.Harness {
	t.Helper()

	h, err := callharness.Start(opts)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, h.Close()) })

	return h
}

// runCallOn is runCall on a harness the test started.
func runCallOn(t *testing.T, h *callharness.Harness, opts callharness.CallOptions, duration time.Duration,
	during func(ctx context.Context, call *callharness.Call),
) *callharness.Report {
	t.Helper()

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

// assertThroughRelay checks that the caller only ever dealt with the relay:
// the answer's one host candidate is the relay's public address, and so is
// the remote end of the caller's selected candidate pair. The caller drops
// media from any other address, so the echo itself came through the relay.
func assertThroughRelay(t *testing.T, h *callharness.Harness, report *callharness.Report) {
	t.Helper()

	relayAddr := h.RelayAddr().String()
	hosts := report.Answer.HostCandidates()
	require.Len(t, hosts, 1, "answer host candidates")
	fields := strings.Fields(hosts[0]) // foundation component transport priority ip port typ host
	require.GreaterOrEqual(t, len(fields), 6, "candidate %q", hosts[0])
	assert.Equal(t, relayAddr, net.JoinHostPort(fields[4], fields[5]), "the answer advertises the relay")
	assert.Equal(t, relayAddr, report.RemoteAddr, "the caller's selected pair ends at the relay")
}

// restartGap returns the caller's media gap across a relay restart at `at`:
// the longest interval between consecutive arrivals that began within
// restartSettle of the restart (packets already in flight still land just
// after it), and when media came back.
func restartGap(arrivals []time.Duration, at time.Duration) (gap, resumedAt time.Duration, ok bool) {
	for i := 1; i < len(arrivals); i++ {
		start := arrivals[i-1]
		if start < at-restartSettle || start > at+restartSettle {
			continue
		}
		if d := arrivals[i] - start; d > gap {
			gap, resumedAt, ok = d, arrivals[i], true
		}
	}

	return gap, resumedAt, ok
}
