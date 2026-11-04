//go:build !windows

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/relaishq/relais/internal/redistestenv"
	"github.com/relaishq/relais/internal/workerlink"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/relay"
	"github.com/stretchr/testify/require"
)

// diagPacket is one clear RTP header seen on the caller's raw socket, with
// the ciphertext digest that matches the worker's SRTP_EGRESS trace.
type diagPacket struct {
	At        int64 // wall clock, Unix nanoseconds
	SSRC      uint32
	PT        uint8
	Seq       uint16
	TS        uint32
	Marker    bool
	SHA256    string `json:",omitempty"`
	Bytes     int
	Direction string
}

// diagCallerSocket records both directions of the caller's raw socket. The
// worker → caller direction is still ciphertext, so only headers are known.
type diagCallerSocket struct {
	*processRawSocket
	mu  sync.Mutex
	log []diagPacket
}

func (s *diagCallerSocket) record(b []byte, direction string) {
	if len(b) < 12 || b[0]>>6 != 2 || (b[1] >= 192 && b[1] <= 223) {
		return
	}
	var h rtp.Header
	if _, err := h.Unmarshal(b); err != nil {
		return
	}
	p := diagPacket{At: time.Now().UnixNano(), SSRC: h.SSRC, PT: h.PayloadType, Seq: h.SequenceNumber, TS: h.Timestamp, Marker: h.Marker, Bytes: len(b), Direction: direction}
	if direction == "in" {
		sum := sha256.Sum256(b)
		p.SHA256 = hex.EncodeToString(sum[:])
	}
	s.mu.Lock()
	s.log = append(s.log, p)
	s.mu.Unlock()
}

func (s *diagCallerSocket) ReadFrom(b []byte) (int, net.Addr, error) {
	n, addr, err := s.processRawSocket.ReadFrom(b)
	if err == nil {
		s.record(b[:n], "in")
	}
	return n, addr, err
}

func (s *diagCallerSocket) WriteTo(b []byte, addr net.Addr) (int, error) {
	s.record(b, "out")
	return s.processRawSocket.WriteTo(b, addr)
}

// TestDiag312DialOutStaleWithheldOutput reproduces #312: the criterion 7
// stale-reconnect fixture, but the transparent leg withholds 40 of worker 0's
// outbound VP8 packets (payload type 96) instead of Opus. The audio subtest is
// the merged fixture's control. Each run writes diag312.json (caller raw RTP
// headers in both directions, withheld packet headers and phase marks) into
// the kept process log directory, next to the workers' SRTP_EGRESS traces.
// It asserts nothing about decode; it reports what the caller received.
//
//	RELAIS_DIAG_312=1 RELAIS_TEST_DIAL_OUT_PROCESS=1 RELAIS_TEST_KEEP_PROCESS_LOGS=1 \
//	  go test -count=1 -run 'TestDiag312DialOutStaleWithheldOutput/video' ./cmd/crash-run/
func TestDiag312DialOutStaleWithheldOutput(t *testing.T) {
	if os.Getenv("RELAIS_DIAG_312") != "1" {
		t.Skip("diagnostic for #312; set RELAIS_DIAG_312=1 after make build")
	}
	for _, tc := range []struct {
		name string
		pt   uint8
	}{{"video", 96}, {"audio", 111}} {
		t.Run(tc.name, func(t *testing.T) { diag312StaleRun(t, tc.pt) })
	}
}

func diag312StaleRun(t *testing.T, pt uint8) {
	marks := map[string]int64{}
	mark := func(name string) { marks[name] = time.Now().UnixNano() }
	var armed atomic.Bool
	helloSeen := make(chan controlplane.LinkHello, 1)
	releaseHello := make(chan struct{})
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(releaseHello) }) })
	var leg *processStaleLeg
	var raw *diagCallerSocket
	var storePath *processStorePartition
	r := newConfiguredProcessRig(t, false, false, func(r *dialProcessRig) {
		r.packetTrace = true
		storePath = newProcessStorePartition(t, redistestenv.Address(t))
		r.workerRedis = storePath.listener.Addr().String()
		leg = newProcessStaleLeg(t, r.topology.relayLeg)
		leg.mu.Lock()
		leg.pt = pt
		leg.mu.Unlock()
		r.legOverride = leg.conn.LocalAddr().String()
		r.callerSocket = func() (net.PacketConn, error) {
			raw = &diagCallerSocket{processRawSocket: processCallerSocket(t)}
			return raw, nil
		}
		r.gate.workerVisit = func(ctx context.Context, m workerlink.Message) error {
			if m.Kind == "hello" && armed.CompareAndSwap(true, false) {
				var hello controlplane.LinkHello
				if err := json.Unmarshal(m.Body, &hello); err != nil {
					return err
				}
				helloSeen <- hello
				select {
				case <-releaseHello:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return nil
		}
	})
	c := r.caller(t, 30*time.Second)
	mark("withhold")
	leg.mu.Lock()
	leg.withhold = true
	leg.mu.Unlock()
	require.Eventually(t, func() bool { leg.mu.Lock(); defer leg.mu.Unlock(); return len(leg.packets) == 40 }, 2*time.Second, 5*time.Millisecond)
	mark("withheld40")
	old, err := r.store.Get(r.ctx, c.SessionID())
	require.NoError(t, err)
	mark("partition")
	storePath.partition(true)
	r.gate.heartbeatsBlocked.Store(true)
	r.gate.fault(false)
	s, err := r.topology.waitControl(r.ctx, 2*time.Second, "partition takeover", func(s controlplane.Status) bool {
		return len(s.Takeovers) == 1 && len(s.Calls) == 1 && s.Calls[0].Owner == "1" && !pairWorker(s, "0").Recovering
	})
	require.NoError(t, err)
	mark("takeover_seen")
	armed.Store(true)
	r.gate.restore()
	select {
	case <-helloSeen:
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not produce reconnect hello")
	}
	mark("hello")
	leg.mu.Lock()
	packets := append([][]byte(nil), leg.packets...)
	leg.mu.Unlock()
	mark("stale_resend_1")
	for _, p := range packets {
		_, err = leg.conn.WriteToUDPAddrPort(p, leg.relay)
		require.NoError(t, err)
	}
	release.Do(func() { close(releaseHello) })
	r.gate.heartbeatsBlocked.Store(false)
	r.ready(t)
	mark("rejoined")
	storePath.partition(false)
	mark("move_back")
	require.NoError(t, c.MoveExternal(r.ctx, 0))
	time.Sleep(700 * time.Millisecond)
	mark("hangup")
	report, err := c.Hangup(r.ctx)
	require.NoError(t, err)

	var withheld []diagPacket
	staleAccepted := 0
	raw.mu.Lock()
	for _, p := range packets {
		_, payload, e := relay.ParseHeader(p)
		require.NoError(t, e)
		staleAccepted += raw.seen[string(payload)]
		var h rtp.Header
		_, e = h.Unmarshal(payload)
		require.NoError(t, e)
		sum := sha256.Sum256(payload)
		withheld = append(withheld, diagPacket{SSRC: h.SSRC, PT: h.PayloadType, Seq: h.SequenceNumber, TS: h.Timestamp, Marker: h.Marker, SHA256: hex.EncodeToString(sum[:]), Bytes: len(payload), Direction: "withheld"})
	}
	raw.mu.Unlock()
	raw.mu.Lock()
	callerLog := append([]diagPacket(nil), raw.log...)
	raw.mu.Unlock()
	video := report.Track("video").Video
	summary := map[string]any{
		"withheld_pt": pt, "old_epoch": old.Epoch, "new_epoch": s.Calls[0].Epoch,
		"stale_accepted": staleAccepted, "decryption_failures": report.DecryptionFailures.Total(),
		"video_frames": video.Frames, "repeated_frames": video.RepeatedFrames, "frame_gaps": video.FrameGaps,
		"non_monotonic_rtp_frames": video.NonMonotonicTimestamps, "incomplete_frames": video.IncompleteFrames,
		"undecodable_frames": video.UndecodableFrames, "unmatched_frames": video.UnmatchedFrames,
		"ffmpeg_in": video.FullDecode.FramesIn, "ffmpeg_decoded": video.FullDecode.FramesDecoded,
		"ffmpeg_errors": video.FullDecode.Errors, "ffmpeg_skipped": video.FullDecode.Skipped,
	}
	data, err := json.MarshalIndent(map[string]any{"summary": summary, "marks": marks, "caller": callerLog, "withheld": withheld}, "", " ")
	require.NoError(t, err)
	out := filepath.Join(r.topology.dir, "diag312.json")
	require.NoError(t, os.WriteFile(out, data, 0o600))
	t.Logf("DIAG312 pt=%d stale_accepted=%d decryption_failures=%d frames=%d repeated=%d gaps=%d non_monotonic_rtp=%d ffmpeg=%d/%d errors=%q out=%s",
		pt, staleAccepted, report.DecryptionFailures.Total(), video.Frames, video.RepeatedFrames, video.FrameGaps, video.NonMonotonicTimestamps,
		video.FullDecode.FramesDecoded, video.FullDecode.FramesIn, video.FullDecode.Errors, out)
}
