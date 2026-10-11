package relay

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRelayBufferMetricsScrape(t *testing.T) {
	for _, failure := range []string{"none", "live-drop", "send", "expiry"} {
		t.Run(failure, func(t *testing.T) {
			size := packetCost(make([]byte, MaxHeaderLen+len(bufferRTP(t, 42, 10))))
			sys := startTestRelay(t, Config{Buffer: &BufferConfig{MaxSessionBytes: 2 * size}, MaxHeldBytes: 2 * size, HoldTimeout: time.Hour})
			a, b := sys.worker(t, sessionA), sys.worker(t, sessionB)
			caller := newTestCaller(t)
			sys.connect(t, caller, a, sessionA)
			for _, seq := range []uint16{10, 11, 12} {
				packet := bufferRTP(t, 42, seq)
				caller.send(t, packet, sys.relay.PublicAddr())
				a.expect(t, caller.addr(), packet)
			}
			body := relayMetricsBody(sys.relay)
			require.Contains(t, body, "relais_relay_buffer_enabled 1\n")
			require.Contains(t, body, "relais_relay_buffer_sessions 1\n")
			require.Contains(t, body, "relais_relay_buffer_packets 2\n")
			require.Contains(t, body, fmt.Sprintf("relais_relay_buffer_bytes %d\n", 2*size))
			require.Contains(t, body, "relais_relay_buffer_drops_total 1\n")
			plan, err := sys.relay.BeginReplay(context.Background(), sessionA, a.addr(), map[uint32]uint64{42: 10})
			require.NoError(t, err)
			require.True(t, plan.Complete)
			require.Equal(t, []ReplayWindow{{SSRC: 42, First: 11, Last: 12}}, plan.Windows)
			transferTestLease(t, sys.cfg.Owners, sessionA, b.addr())
			require.NoError(t, sys.relay.MoveSession(sessionA, a.addr(), b.addr()))
			switch failure {
			case "live-drop":
				caller.send(t, bufferRTP(t, 42, 13), sys.relay.PublicAddr())
				require.Eventually(t, func() bool { return sys.relay.Stats().HoldDrops == 1 }, time.Second, time.Millisecond)
			case "send":
				require.NoError(t, sys.relay.workers.SetWriteDeadline(time.Now().Add(-time.Second)))
				t.Cleanup(func() { require.NoError(t, sys.relay.workers.SetWriteDeadline(time.Time{})) })
			case "expiry":
				sys.relay.forwardMu.Lock()
				h := sys.relay.holds[sessionA]
				sys.relay.forwardMu.Unlock()
				sys.relay.expireHold(h)
			}
			result, err := sys.relay.ReplaySession(context.Background(), sessionA, b.addr())
			if failure == "expiry" {
				require.ErrorIs(t, err, ErrHoldExpired)
			} else {
				require.NoError(t, err)
			}
			body = relayMetricsBody(sys.relay)
			for name, value := range map[string]int{"replay_packets_total": result.Packets, "replay_drops_total": result.Dropped, "replay_send_failures_total": result.SendFailures} {
				require.Contains(t, body, fmt.Sprintf("relais_relay_%s %d\n", name, value))
			}
			if failure == "expiry" {
				require.Contains(t, body, "relais_relay_replay_expiries_total 1\n")
				require.Equal(t, 2, result.Dropped)
			} else {
				require.Contains(t, body, "relais_relay_replays_total 1\n")
			}
			require.Contains(t, body, "relais_relay_held_packets 0\n")
			if result.Packets > 0 {
				require.Equal(t, plan.Windows, result.Windows)
				result.Windows[0].Last = 999
				again, err := sys.relay.ReplaySession(context.Background(), sessionA, b.addr())
				require.NoError(t, err)
				require.Equal(t, plan.Windows, again.Windows, "callers cannot mutate the retained receipt")
			} else {
				require.Empty(t, result.Windows)
			}
		})
	}
}

func TestRelayBufferMetricDisabled(t *testing.T) {
	require.Contains(t, relayMetricsBody(startTestRelay(t, Config{}).relay), "relais_relay_buffer_enabled 0\n")
}
