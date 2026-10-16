package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/require"
)

// Reviewer probe: a compliant client bursts small messages on the reliable
// ordered channel without waiting for each echo. Every message is 4 bytes,
// far below max-message-size and the SCTP receive window.
func TestReviewBurstSmallMessagesKeepsSession(t *testing.T) {
	for _, n := range []int{8, 32, 200} {
		t.Run(fmt.Sprintf("audio/n=%d", n), func(t *testing.T) {
			sys := newDCSystem(t, true, 0)
			c := dialDC(t, sys, true)
			c.roundtrip(t, 1)
			for j := 0; j < n; j++ {
				data := make([]byte, 4)
				binary.BigEndian.PutUint32(data, uint32(1000+j))
				require.NoError(t, c.channels[0].Send(data))
			}
			got := 0
			deadline := time.After(5 * time.Second)
			for got < n {
				select {
				case seq := <-c.received[0]:
					require.Equal(t, uint32(1000+got), seq, "burst echo must preserve every numbered message")
					got++
				case <-c.closed[0]:
					t.Fatalf("reliable channel closed after %d/%d echoes; pc=%s sessions=%d stats=%+v",
						got, n, c.pc.ConnectionState(), sys.workers[0].SessionCount(), sys.workers[0].DataChannelStats())
				case <-deadline:
					t.Fatalf("timeout after %d/%d echoes; pc=%s sessions=%d", got, n, c.pc.ConnectionState(), sys.workers[0].SessionCount())
				}
			}
			before := c.audio.Load()
			require.Eventually(t, func() bool { return c.audio.Load() > before+3 }, 2*time.Second, 10*time.Millisecond)
			require.Equal(t, webrtc.DataChannelStateOpen, c.channels[0].ReadyState())
			stats := sys.workers[0].DataChannelStats()
			require.Zero(t, stats.Oversized)
			require.Zero(t, stats.PendingOverflow)
			require.Zero(t, stats.ReassemblyStalls)
		})
	}
}

// Reviewer probe: the fifth channel admission over a session's lifetime.
func TestReviewFifthChannelKeepsAudio(t *testing.T) {
	sys := newDCSystem(t, true, 0)
	c := dialDC(t, sys, true)
	c.roundtrip(t, 1)
	for i := 0; i < 5; i++ {
		dc, err := c.pc.CreateDataChannel(fmt.Sprintf("extra%d", i), nil)
		require.NoError(t, err)
		_ = dc
		time.Sleep(200 * time.Millisecond)
	}
	time.Sleep(500 * time.Millisecond)
	t.Logf("sessions=%d pc=%s stats=%+v", sys.workers[0].SessionCount(), c.pc.ConnectionState(), sys.workers[0].DataChannelStats())
	before := c.audio.Load()
	time.Sleep(500 * time.Millisecond)
	t.Logf("audio before=%d after=%d", before, c.audio.Load())
	require.Equal(t, 1, sys.workers[0].SessionCount(), "a refused fifth channel must not end the media session")
	require.Greater(t, c.audio.Load(), before+3, "audio must continue after the refused channel")
	stats, err := json.Marshal(sys.workers[0].DataChannelStats())
	require.NoError(t, err)
	require.Contains(t, string(stats), `"Rejected":3`)
	c.roundtrip(t, 3)
}

func TestReviewFragmentedOversizeKeepsAudio(t *testing.T) {
	sys := newDCSystem(t, true, 0)
	c := dialDCWithSettings(t, sys, true, "0", 1<<20, func(se *webrtc.SettingEngine) { se.SetSCTPMaxMessageSize(1 << 20) })
	c.roundtrip(t, 1)
	require.NoError(t, c.channels[0].Send(make([]byte, 128<<10)))
	select {
	case <-c.closed[0]:
	case <-time.After(4 * time.Second):
		t.Fatal("fragmented oversize did not close the data side")
	}
	require.Eventually(t, func() bool { return sys.workers[0].DataChannelStats().Oversized > 0 }, time.Second, 10*time.Millisecond)
	before := c.audio.Load()
	require.Eventually(t, func() bool { return c.audio.Load() > before+5 }, time.Second, 10*time.Millisecond)
	require.Equal(t, 1, sys.workers[0].SessionCount())
	require.Eventually(t, func() bool {
		for {
			select {
			case f := <-sys.upstream.frames:
				if len(f) > 2 && f[0] == 2 && strings.Contains(string(f), "datachannel_message_too_large") {
					return true
				}
			default:
				return false
			}
		}
	}, time.Second, 10*time.Millisecond)
}

func TestReviewCompleteLargeOversizeCounted(t *testing.T) {
	sys := newDCSystem(t, true, 0)
	c := dialDCWithSettings(t, sys, true, "0", 1<<20, func(se *webrtc.SettingEngine) { se.SetSCTPMaxMessageSize(1 << 20) })
	c.roundtrip(t, 1)
	require.NoError(t, c.channels[0].Send(make([]byte, (64<<10)+1)))
	select {
	case <-c.closed[0]:
	case <-time.After(4 * time.Second):
		t.Fatal("large complete message did not close")
	}
	require.EqualValues(t, 1, sys.workers[0].DataChannelStats().Oversized)
	// A complete oversize is already handled locally. It must not leave a
	// phantom reassembly stall that later closes the other healthy channel.
	timer := time.NewTimer(2300 * time.Millisecond)
	<-timer.C
	require.Equal(t, webrtc.DataChannelStateOpen, c.channels[1].ReadyState())
	require.EqualValues(t, 1, sys.workers[0].DataChannelStats().Oversized)
	require.Zero(t, sys.workers[0].DataChannelStats().ReassemblyStalls)

	before := c.audio.Load()
	require.Eventually(t, func() bool { return c.audio.Load() > before+3 }, time.Second, 10*time.Millisecond)
}
