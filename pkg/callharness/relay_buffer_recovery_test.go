package callharness

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relais/internal/workerprobe"
	"github.com/relais/pkg/mediaworker"
	"github.com/relais/pkg/relay"
	"github.com/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

// Stop the source before takeover so an enabled but empty relay ring has no
// input to schedule. Only actual cached-picture delivery can recover a picture.
func TestRelayBufferEmptyRingReplaysCachedPicture(t *testing.T) {
	relayBufferStores(t, func(t *testing.T, store sessionstore.Store) {
		sys := startBufferFailureSystem(t, store, bufferFailureConfig{
			cache: true, workers: 2, buffer: relay.BufferConfig{MaxSessionBytes: 1},
		})
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		call, err := sys.h.Dial(ctx, CallOptions{Video: true, Worker: 0})
		require.NoError(t, err)
		require.NoError(t, call.SendMedia(ctx, 1250*time.Millisecond))
		require.Eventually(t, func() bool { return sys.workers[0].ReplayStats().AppendPending == 0 }, time.Second, time.Millisecond)
		require.Positive(t, sys.r.Stats().BufferDrops)
		killed := time.Now()
		sys.h.RecordProcessKill("0", killed)
		require.NoError(t, workerprobe.Kill(sys.workers[0].LocalAddr()))
		status := waitBufferTakeovers(t, ctx, sys.h, 1)
		require.Zero(t, status.Takeovers[0].Result.RelayReplayPackets, "the enabled ring has no input to deliver")
		require.False(t, status.Takeovers[0].Result.RelayReplayComplete)
		time.Sleep(300 * time.Millisecond)
		failures, err := sys.workers[1].SessionDecryptFailures(call.SessionID())
		require.NoError(t, err)
		require.Zero(t, failures)
		report, err := call.Hangup(ctx)
		require.NoError(t, err)
		require.Zero(t, report.DecryptionFailures.Total())
		require.Len(t, report.Moves, 1)
		recovery := report.Moves[0].Recovery
		require.Equal(t, "Cache", recovery.Path)
		require.Positive(t, recovery.ReplayPackets)
		require.Positive(t, recovery.FirstDecodedAfterKill)
		decoded := decodeBufferUnique(t, ctx, call, report, false)
		t.Logf("EMPTY_RING_CACHE relay_packets=0 cached_packets=%d first_decoded=%s unique_decoded=%d decrypt_failures=0", recovery.ReplayPackets, recovery.FirstDecodedAfterKill, decoded.FramesDecoded)
	})
}

// The first adoption skips recovery, but its reply is lost until the gate
// expires. Confirmation must repair the gap even when its receipt is empty.
func TestRelayBufferIncompleteReplayConfirmationAfterExpiry(t *testing.T) {
	relayBufferStores(t, func(t *testing.T, store sessionstore.Store) {
		var sys *bufferFailureSystem
		var liveAfterExpiry, keyframes atomic.Uint64
		var mu sync.Mutex
		var resumes []mediaworker.ResumeRequest
		fault := &bufferExpiredResumeFault{expired: func() bool { return sys.r.Stats().HoldTimeouts > 0 }, adopted: true, liveAfterExpiry: liveAfterExpiry.Load}
		sys = startBufferFailureSystem(t, store, bufferFailureConfig{
			cache: true, workers: 2, holdTimeout: 500 * time.Millisecond,
			buffer: relay.BufferConfig{MaxSessionBytes: 4 << 10}, targetTransport: fault,
			workerHandler: func(i int, next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if i == 1 && r.URL.Path == "/resume" {
						body, err := io.ReadAll(r.Body)
						require.NoError(t, err)
						r.Body = io.NopCloser(bytes.NewReader(body))
						var req mediaworker.ResumeRequest
						require.NoError(t, json.Unmarshal(body, &req))
						mu.Lock()
						resumes = append(resumes, req)
						mu.Unlock()
					}
					if i == 1 && strings.HasSuffix(r.URL.Path, "/keyframe") {
						keyframes.Add(1)
					}
					next.ServeHTTP(w, r)
				})
			},
		})
		require.NoError(t, workerprobe.SetAfterDecrypt(sys.workers[1].LocalAddr(), func(_ string, _ uint32, _ uint64) {
			if sys.r.Stats().HoldTimeouts > 0 {
				liveAfterExpiry.Add(1)
			}
		}))
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		call, err := sys.h.Dial(ctx, CallOptions{Video: true, Worker: 0})
		require.NoError(t, err)
		mediaCtx, stop := context.WithCancel(ctx)
		sent := make(chan error, 1)
		go func() { sent <- call.SendMedia(mediaCtx, 8*time.Second) }()
		defer func() { stop(); require.ErrorIs(t, <-sent, context.Canceled) }()
		time.Sleep(650 * time.Millisecond)
		sys.h.RecordProcessKill("0", time.Now())
		require.NoError(t, workerprobe.Kill(sys.workers[0].LocalAddr()))
		status := waitBufferTakeovers(t, ctx, sys.h, 1)
		mu.Lock()
		observed := append([]mediaworker.ResumeRequest(nil), resumes...)
		mu.Unlock()
		require.Len(t, observed, 2)
		require.True(t, observed[0].RelayInputReplay, "first adoption defers its own resume recovery")
		require.False(t, observed[0].RelayReplay, "the first ring is already incomplete")
		require.NotEmpty(t, observed[0].DuplicateWindows)
		require.True(t, observed[1].RelayInputReplay, "confirmation retains the first attempt's recovery flags despite the empty expiry receipt")
		require.False(t, observed[1].RelayReplay)
		require.Empty(t, observed[1].DuplicateWindows)
		require.Equal(t, observed[0].Lease.Epoch, observed[1].Lease.Epoch, "confirmation acknowledges the original adoption")
		result := status.Takeovers[0].Result
		require.True(t, result.HoldExpired)
		require.False(t, result.RelayReplayComplete)
		require.Zero(t, result.RelayReplayPackets)
		require.True(t, result.RelayReplayRecoveryPLI, "confirmation must retain the first adoption's recovery obligation")
		require.EqualValues(t, 1, keyframes.Load(), "request post-drain recovery once")
		time.Sleep(300 * time.Millisecond)
		failures, err := sys.workers[1].SessionDecryptFailures(call.SessionID())
		require.NoError(t, err)
		require.Zero(t, failures)
		stop()
		report, err := call.Hangup(ctx)
		require.NoError(t, err)
		require.Zero(t, report.DecryptionFailures.Total())
		require.Positive(t, report.SentVideo.KeyframeRequests)
		require.Positive(t, report.Moves[0].Recovery.FirstDecodedPLIAfterKill)
		t.Logf("EXPIRED_REPLAY_CONFIRMATION adopted_input=true receipt_empty=true recovery_requests=%d decoded_pli=%s decrypt_failures=0", keyframes.Load(), report.Moves[0].Recovery.FirstDecodedPLIAfterKill)
	})
}
