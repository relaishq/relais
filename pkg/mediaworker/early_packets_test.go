package mediaworker

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/srtp/v3"
	"github.com/relais/internal/workerprobe"
	"github.com/stretchr/testify/require"
)

func TestEarlyPacketsRetainOrderAndCountOverflow(t *testing.T) {
	for _, tc := range []struct {
		name    string
		packets int
		rtcp    bool
		fence   bool
	}{
		{name: "RTP and SRTCP", packets: 3, rtcp: true},
		{name: "overflow drops oldest", packets: 66},
		{name: "fenced startup", packets: 3, fence: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Cleanup(workerprobe.Enable())
			w := newTestWorker(t)
			workerprobe.Register(w.LocalAddr(), nil)
			t.Cleanup(func() { workerprobe.Remove(w.LocalAddr()) })
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			require.NoError(t, workerprobe.SetBeforeSRTP(w.LocalAddr(), func(ctx context.Context, _ string) {
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
				}
			}))
			call, client, endpoint := handshakeDTLSCaller(t, w)
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("worker did not reach the post-handshake gate")
			}
			s := w.session(call.id)
			state, ok := client.ConnectionState()
			require.True(t, ok)
			cfg := srtp.Config{Profile: srtp.ProtectionProfileAeadAes128Gcm}
			require.NoError(t, cfg.ExtractSessionKeysFromDTLS(&state, true))
			caller := testContext(t, cfg.Keys.LocalMasterKey, cfg.Keys.LocalMasterSalt, cfg.Profile)
			var observed []uint64 // read and written under s.mu
			require.NoError(t, workerprobe.SetAfterDecrypt(w.LocalAddr(), func(_ string, _ uint32, index uint64) {
				observed = append(observed, index)
			}))
			var control []byte
			if tc.rtcp {
				plain, err := (&rtcp.ReceiverReport{SSRC: 456}).Marshal()
				require.NoError(t, err)
				control, err = caller.EncryptRTCP(nil, plain, nil)
				require.NoError(t, err)
			}
			for seq := 1; seq <= tc.packets; seq++ {
				_, err := endpoint.conn.WriteToUDPAddrPort(testEncrypt(t, caller, 456, uint16(seq)), w.MediaAddr())
				require.NoError(t, err)
				if tc.rtcp && seq == 1 {
					_, err = endpoint.conn.WriteToUDPAddrPort(control, w.MediaAddr())
					require.NoError(t, err)
				}
			}
			total := tc.packets
			if tc.rtcp {
				total++
			}
			wantDrops := max(0, total-64)
			require.Eventually(t, func() bool {
				s.mu.Lock()
				defer s.mu.Unlock()
				return len(s.earlyPackets) == min(total, 64) && w.metrics.earlyPacketDrops.Load() == uint64(wantDrops)
			}, 5*time.Second, time.Millisecond, "the real UDP reader queued the early ciphertext")
			s.mu.Lock()
			keysInstalled := s.srtpIn != nil
			before := slices.Clone(observed)
			if tc.fence {
				s.fenced.Store(true)
			}
			s.mu.Unlock()
			require.False(t, keysInstalled)
			require.Empty(t, before, "no early packet was processed before keys")
			unblock()
			require.Eventually(t, func() bool {
				s.mu.Lock()
				defer s.mu.Unlock()
				return s.srtpIn != nil && s.earlyPackets == nil && s.earlyDrain == nil
			}, 5*time.Second, time.Millisecond)
			if tc.fence {
				s.handlePacket(testEncrypt(t, caller, 456, 4))
				s.mu.Lock()
				got, queued := slices.Clone(observed), len(s.earlyPackets)
				s.mu.Unlock()
				require.Empty(t, got)
				require.Zero(t, queued, "a fenced session never queues later input")
				return
			}
			// Later input must follow the drained queue, with no duplicates.
			s.handlePacket(testEncrypt(t, caller, 456, uint16(tc.packets+1)))
			var want []uint64
			for seq := 1 + wantDrops; seq <= tc.packets+1; seq++ {
				want = append(want, uint64(seq))
			}
			s.mu.Lock()
			got := slices.Clone(observed)
			s.mu.Unlock()
			require.Equal(t, want, got)
			require.Zero(t, s.decryptFailures.Load())
			if tc.rtcp {
				s.handlePacket(control)
				require.EqualValues(t, 1, s.decryptFailures.Load(), "early SRTCP was processed: replay is rejected")
				target := newTestWorker(t)
				workerprobe.Register(target.LocalAddr(), nil)
				t.Cleanup(func() { workerprobe.Remove(target.LocalAddr()) })
				var resumeGateCalls atomic.Uint64
				require.NoError(t, workerprobe.SetBeforeSRTP(target.LocalAddr(), func(context.Context, string) { resumeGateCalls.Add(1) }))
				saved, err := w.ExportSession(call.id)
				require.NoError(t, err)
				_, err = target.ResumeSession(saved, ResumeOptions{})
				require.NoError(t, err)
				resumed := target.session(call.id)
				resumed.mu.Lock()
				ready, unused := resumed.srtpIn != nil, resumed.earlyPackets == nil && resumed.earlyDrain == nil
				resumed.mu.Unlock()
				require.True(t, ready)
				require.True(t, unused)
				require.Zero(t, resumeGateCalls.Load(), "resume bypasses the startup gate")
			}
			require.Contains(t, metricsBody(w), fmt.Sprintf("relais_worker_early_packet_drops_total %d\n", wantDrops))
			require.EqualValues(t, wantDrops, w.metrics.earlyPacketDrops.Load())
			if !tc.rtcp { // Export already removed the source in the resume case.
				require.NoError(t, w.EndSession(call.id))
			}
			require.EqualValues(t, wantDrops, w.metrics.earlyPacketDrops.Load(), "overflow count survives session removal")
		})
	}
}
