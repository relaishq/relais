//go:build !windows

package main

import (
	"encoding/json"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

// Both modes run the same control death at an applied export, followed by two
// immediate exact moves. No sleep between recovery and either move is allowed.
func TestProcessImmediateMoveAfterUncertainExport(t *testing.T) {
	for _, scenario := range []int{0, 1, 2} {
		inbound, rebind := scenario == 1, scenario == 2
		t.Run("inbound="+strconv.FormatBool(inbound)+"/rebind="+strconv.FormatBool(rebind), func(t *testing.T) {
			var raw *processRawSocket
			r := newConfiguredProcessRig(t, rebind, inbound, func(r *dialProcessRig) {
				r.packetTrace = true
				r.callerSocket = func() (net.PacketConn, error) { raw = processCallerSocket(t); return raw, nil }
			})
			c := r.caller(t, 30*time.Second)
			r.gate.armed.Store(true)
			moved := make(chan error, 1)
			if rebind {
				require.NoError(t, r.mapping.Rebind(netip.AddrPort{}))
				moved <- nil
			} else {
				go func() { moved <- c.MoveExternal(r.ctx, 1) }()
			}
			select {
			case <-r.gate.entered:
			case <-time.After(4 * time.Second):
				t.Fatal("export did not apply")
			}
			require.NoError(t, r.topology.control.Child.SignalGroup(syscall.SIGKILL))
			select {
			case <-r.topology.control.Child.Done():
			case <-time.After(3 * time.Second):
				t.Fatal("control did not exit")
			}
			r.gate.unblock()
			select {
			case <-moved:
			case <-time.After(3 * time.Second):
				t.Fatal("interrupted move did not return")
			}
			r.startControl(t)
			s := r.ready(t)
			require.Zero(t, s.LostCount)
			require.Len(t, s.Calls, 1)
			require.Len(t, s.Takeovers, 1)
			recoveredAt := time.Now()
			target := 0
			if s.Calls[0].Owner == "0" {
				target = 1
			}
			require.NoError(t, c.MoveExternal(r.ctx, target))
			require.NoError(t, c.MoveExternal(r.ctx, 1-target))
			time.Sleep(time.Second)
			report, err := c.Hangup(r.ctx)
			require.NoError(t, err)
			raw.mu.Lock()
			headers := append([]rtp.Header(nil), raw.headers...)
			raw.mu.Unlock()
			data, err := json.MarshalIndent(headers, "", "  ")
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(r.topology.dir, "caller-rtp-headers.json"), data, 0600))
			last := map[uint32]rtp.Header{}
			backsteps := 0
			for _, h := range headers {
				if prev, ok := last[h.SSRC]; ok && int32(h.Timestamp-prev.Timestamp) < 0 {
					backsteps++
					t.Logf("RTP_BACKSTEP ssrc=%d seq=%d->%d timestamp=%d->%d marker=%t", h.SSRC, prev.SequenceNumber, h.SequenceNumber, prev.Timestamp, h.Timestamp, h.Marker)
				}
				last[h.SSRC] = h
			}
			video := report.Track("video").Video
			t.Logf("IMMEDIATE_MOVE inbound=%t elapsed=%s rtp_backsteps=%d non_monotonic_frames=%d decoded=%d/%d decode_errors=%v decryption_failures=%d logs=%s", inbound, time.Since(recoveredAt), backsteps, video.NonMonotonicTimestamps, video.FullDecode.FramesDecoded, video.FullDecode.FramesIn, video.FullDecode.Errors, report.DecryptionFailures.Total(), r.topology.dir)
			require.Zero(t, report.DecryptionFailures.Total())
			r.workerCounters(t)
			// Raw packet order includes permitted retransmission and replay. The
			// decoder must consume every unique frame without a DTS or decode error.
			require.True(t, video.FullDecode.Ran())
			require.Empty(t, video.FullDecode.Errors)
			require.Equal(t, video.FullDecode.FramesIn, video.FullDecode.FramesDecoded)
		})
	}
}
