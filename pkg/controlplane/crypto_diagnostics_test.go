package controlplane_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/relaishq/relais/pkg/callharness"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/relay"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

type corruptedCryptoPacket struct{ before, after []byte }

type corruptCryptoConn struct {
	net.PacketConn
	mu        sync.Mutex
	corrupted map[string]corruptedCryptoPacket
}

func (c *corruptCryptoConn) ReadFrom(raw []byte) (int, net.Addr, error) {
	n, addr, err := c.PacketConn.ReadFrom(raw)
	if err != nil || n < 13 || raw[0]>>6 != 2 {
		return n, addr, err
	}
	kind, offset := "", 12
	switch {
	case raw[1] >= 192 && raw[1] <= 223:
		kind, offset = "SRTCP", 8
	case raw[1]&0x7f == 111:
		kind = "audio"
	case raw[1]&0x7f == 96:
		kind = "video"
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, done := c.corrupted[kind]; kind != "" && !done {
		before := append([]byte(nil), raw[:n]...)
		raw[offset] ^= 1
		c.corrupted[kind] = corruptedCryptoPacket{before: before, after: append([]byte(nil), raw[:n]...)}
	}
	return n, addr, err
}

// Corrupt real-worker ciphertext at the real caller socket. Both the existing
// Pion counters and the independent metadata observer must catch every failure,
// including SRTCP. Match the original ciphertext to the sender trace too.
func TestHTTPCallerCryptoDiagnostics(t *testing.T) {
	forHTTPRecoveryStores(t, func(t *testing.T, store sessionstore.Store) {
		conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
		require.NoError(t, err)
		t.Cleanup(func() { _ = conn.Close() })
		wire := &corruptCryptoConn{PacketConn: conn, corrupted: make(map[string]corruptedCryptoPacket)}
		var mu sync.Mutex
		traces := make(map[string]mediaworker.PacketTrace)
		s := newHTTPSystemWithSettings(t, nil, nil, nil, httpSystemSettings{
			store: store, buffer: &relay.BufferConfig{},
			callerSocket: func() (net.PacketConn, error) { return wire, nil },
			packetTrace: func(packet mediaworker.PacketTrace) {
				mu.Lock()
				defer mu.Unlock()
				traces[packet.CiphertextSHA256] = packet
			},
		})
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		call, err := s.harness.Dial(ctx, callharness.CallOptions{Video: true, Worker: 0, SRTPDiagnostics: true})
		require.NoError(t, err)
		media, stop := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- call.SendMedia(media, 5*time.Second) }()
		defer func() { stop(); <-done }()
		time.Sleep(350 * time.Millisecond)
		require.NoError(t, s.workers[0].RequestKeyframe(ctx, call.SessionID()))
		require.Eventually(t, func() bool {
			wire.mu.Lock()
			defer wire.mu.Unlock()
			return len(wire.corrupted) == 3
		}, time.Second, time.Millisecond)
		time.Sleep(200 * time.Millisecond)
		report, err := call.Hangup(ctx)
		require.NoError(t, err)
		require.Equal(t, callharness.DecryptionFailures{AuthTag: 3}, report.DecryptionFailures)
		diagnostics := report.SRTPDiagnostics
		require.NotNil(t, diagnostics)
		require.True(t, diagnostics.Ready, diagnostics.Unavailable)
		require.Equal(t, report.DecryptionFailures, diagnostics.Failures)
		require.Contains(t, report.Summary(), "SRTP diagnostics: ready=true")
		require.Zero(t, diagnostics.Skipped)
		require.Len(t, diagnostics.Samples, 3)
		wire.mu.Lock()
		defer wire.mu.Unlock()
		mu.Lock()
		defer mu.Unlock()
		samples := make(map[string]callharness.SRTPFailureSample)
		for _, sample := range diagnostics.Samples {
			samples[sample.CiphertextSHA256] = sample
		}
		for kind, packet := range wire.corrupted {
			digest := sha256.Sum256(packet.before)
			trace, found := traces[hex.EncodeToString(digest[:])]
			require.True(t, found, "sender fingerprint for %s", kind)
			require.Equal(t, s.workers[0].LocalAddr(), trace.Worker)
			require.EqualValues(t, 1, trace.Epoch)
			require.Equal(t, call.SessionID(), trace.SessionID)
			digest = sha256.Sum256(packet.after)
			sample, found := samples[hex.EncodeToString(digest[:])]
			require.True(t, found, "failure fingerprint for %s", kind)
			require.Equal(t, trace.Protocol, sample.Protocol)
			require.Equal(t, trace.SSRC, sample.SSRC)
			require.Equal(t, trace.Index, sample.EstimatedIndex)
			require.Positive(t, sample.At)
			require.Nil(t, sample.AuthenticatedIndex, "corruption must not verify under another ROC")
			if kind != "SRTCP" {
				var header rtp.Header
				_, err := header.Unmarshal(packet.before)
				require.NoError(t, err)
				require.Equal(t, header.SequenceNumber, sample.Sequence)
				require.Equal(t, header.Timestamp, sample.Timestamp)
			}
		}
	})
}

// failureRecorder runs a helper up to its first failed assertion and keeps
// the order of what it logged and reported.
type failureRecorder struct {
	testing.TB
	mu     sync.Mutex
	events []string
}

func (r *failureRecorder) Helper() {}
func (r *failureRecorder) Logf(format string, args ...any) {
	r.record("log: " + fmt.Sprintf(format, args...))
}
func (r *failureRecorder) Errorf(format string, args ...any) {
	r.record("error: " + fmt.Sprintf(format, args...))
}
func (r *failureRecorder) FailNow() { runtime.Goexit() }
func (r *failureRecorder) record(event string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

// A diagnostics-enabled flowingCall with corrupted caller ciphertext must print
// each failure sample before its zero-failure assertion stops the call (#474).
func TestFlowingCallPrintsSRTPFailureSamples(t *testing.T) {
	conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	wire := &corruptCryptoConn{PacketConn: conn, corrupted: make(map[string]corruptedCryptoPacket)}
	s := newHTTPSystemWithSettings(t, nil, nil, nil, httpSystemSettings{
		buffer: &relay.BufferConfig{}, diagnostics: true,
		callerSocket: func() (net.PacketConn, error) { return wire, nil },
	})
	recorder := &failureRecorder{TB: t}
	returned := make(chan bool, 1)
	go func() {
		ok := false
		defer func() { returned <- ok }()
		flowingCall(recorder, s, func(context.Context, *callharness.Call) {})
		ok = true
	}()
	require.False(t, <-returned, "corrupted ciphertext must fail flowingCall")
	recorder.mu.Lock()
	events := slices.Clone(recorder.events)
	recorder.mu.Unlock()
	for _, event := range events {
		t.Log(event)
	}
	failure := slices.IndexFunc(events, func(event string) bool { return strings.HasPrefix(event, "error: ") })
	require.NotEqual(t, -1, failure)
	require.Contains(t, events[failure], "Should be zero")
	logged := make(map[string]bool)
	for _, event := range events[:failure] {
		encoded, found := strings.CutPrefix(event, "log: SRTP failure sample: ")
		if !found {
			continue
		}
		var sample callharness.SRTPFailureSample
		require.NoError(t, json.Unmarshal([]byte(encoded), &sample))
		require.NotZero(t, sample.SSRC)
		require.NotEmpty(t, sample.Error)
		logged[sample.CiphertextSHA256] = true
	}
	wire.mu.Lock()
	defer wire.mu.Unlock()
	require.NotEmpty(t, wire.corrupted)
	for kind, packet := range wire.corrupted {
		digest := sha256.Sum256(packet.after)
		require.True(t, logged[hex.EncodeToString(digest[:])], "printed sample for corrupted %s", kind)
	}
}
