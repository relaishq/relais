//go:build bridge_measurement

package mediaworker

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/relaishq/relais/internal/redistestenv"
	"github.com/stretchr/testify/require"
)

// A real Redis ALL pause prevents both reads and writes from acknowledging.
// Only the dedicated test endpoint is used. The fixed 29 s command resumes
// itself, including on an assertion failure; no other process is signaled.
func TestBridgeOutputStoreStallRetention(t *testing.T) {
	if testing.Short() {
		t.Skip("29 s live Redis pause at both packet sizes")
	}
	for _, size := range []int{160, 1200} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			rig, scope, store, sent := newMeasuredBridge(t, true)
			s := rig.s
			client := redis.NewClient(&redis.Options{Addr: redistestenv.Address(t), MaxRetries: -1, ReadTimeout: 5 * time.Second})
			t.Cleanup(func() { require.NoError(t, client.Close()) })
			var baseline []byte
			var pausedAt time.Time
			var peakCost, peakMemory int64
			var peakRetained uint64
			var peakQueue int
			var creditWait time.Duration
			var credits bridgeCreditSamples
			const packets = 1550
			// Start five packets ahead, then maintain a 100 ms source backlog.
			start := time.Now()
			for seq := 1; seq <= packets; seq++ {
				if wait := time.Until(start.Add(time.Duration(max(0, seq-6)) * 20 * time.Millisecond)); wait > 0 {
					time.Sleep(wait)
				}
				if seq == 51 {
					// Drain the writer's previous capture before taking the durable baseline.
					s.checkpoint.snapshotMu.Lock()
					store.stalling.Store(true)
					err := client.Do(context.Background(), "CLIENT", "PAUSE", 29000, "ALL").Err()
					pausedAt = time.Now()
					baseline, _ = s.worker.BridgeOutputCheckpointed(scope)
					s.checkpoint.snapshotMu.Unlock()
					require.NoError(t, err)
					t.Cleanup(func() {
						// Failed assertions still let the owned server's auto-resume complete
						// before newMeasuredBridge releases its lease during teardown.
						if wait := time.Until(pausedAt.Add(29 * time.Second)); wait > 0 {
							time.Sleep(wait)
						}
						_ = client.Ping(context.Background()).Err()
					})
				}
				payload := bridgePacket(size)
				binary.BigEndian.PutUint64(payload[1:9], uint64(seq))
				credit, err := credits.read(s, scope)
				require.NoError(t, err)
				waited := time.Now()
				for (credit.PacketSlots == 0 || credit.PayloadBytes < size) && time.Since(waited) < time.Second {
					time.Sleep(time.Millisecond)
					credit, err = credits.read(s, scope)
					require.NoError(t, err)
				}
				creditWait = max(creditWait, time.Since(waited))
				s.mu.Lock()
				diagnostic := fmt.Sprintf("seq=%d cost=%d records=%d queue=%d sent=%d pruned=%d", seq, s.bridgeExportCostLocked(), len(s.agent.v2.bridge.records), len(s.agent.v2.playback.bridgeQueue), s.agent.v2.bridge.state.Sent, s.agent.v2.bridge.state.Pruned)
				s.mu.Unlock()
				require.Positive(t, credit.PacketSlots, "retention must not silence output: %s", diagnostic)
				require.GreaterOrEqual(t, credit.PayloadBytes, size, "%s", diagnostic)
				receipt, err := s.worker.SubmitBridgeOutput(context.Background(), scope, 1, uint64(seq), payload)
				require.NoError(t, err)
				require.Equal(t, "admitted", receipt.Outcome, "packet=%d receipt=%+v", seq, receipt)
				require.True(t, receipt.Consumed)
				s.mu.Lock()
				b := s.agent.v2.bridge
				cost, memory := s.bridgeExportCostLocked(), b.memory.Load()
				peakCost, peakMemory = max(peakCost, cost), max(peakMemory, memory)
				peakRetained = max(peakRetained, b.state.Through-b.state.Pruned)
				peakQueue = max(peakQueue, len(s.agent.v2.playback.bridgeQueue))
				limit := s.bridgeLookaheadBudget()
				s.mu.Unlock()
				require.LessOrEqual(t, cost, limit)
				require.LessOrEqual(t, memory, int64(s.agent.v2.registration.Bounds.MaxMailboxBytes))
				require.False(t, s.agent.failed.Load())
				require.NoError(t, s.ctx.Err())
				if !pausedAt.IsZero() && time.Since(pausedAt) < 29*time.Second {
					current, _ := s.worker.BridgeOutputCheckpointed(scope)
					require.True(t, bytes.Equal(baseline, current), "a paused store cannot invent output_checkpointed")
				}
			}
			require.Eventually(t, func() bool { return sent.Load() == packets }, 5*time.Second, 10*time.Millisecond)
			resumed := time.Now()
			require.NoError(t, client.Ping(context.Background()).Err())
			require.GreaterOrEqual(t, resumed.Sub(pausedAt), 29*time.Second)
			// Let a timed-out pre-resume request drain before enforcing ordinary errors.
			require.Eventually(t, func() bool {
				s.mu.Lock()
				defer s.mu.Unlock()
				b := s.agent.v2.bridge
				return b.durable != nil && b.durable.Sent == packets && b.state.Pruned == packets && len(b.records) == 0 && len(b.settled) == 0
			}, 10*time.Second, 10*time.Millisecond)
			store.stalling.Store(false)
			measured := s.agent.v2.instance.(*bridgeMeasuredAgent)
			measured.wireMu.Lock()
			seen := map[uint64]bool{}
			var last time.Time
			var previous *bridgeWireSample
			var maxGap, gapBeyondDebt time.Duration
			for _, sample := range measured.wire {
				require.False(t, seen[sample.id], "duplicate wire identity %d", sample.id)
				seen[sample.id] = true
				if !last.IsZero() {
					maxGap = max(maxGap, sample.at.Sub(last))
				}
				if previous != nil && previous.backlog {
					gap := sample.at.Sub(previous.at)
					media := time.Duration(uint32(sample.timestamp-previous.timestamp)) * time.Second / 48000
					gapBeyondDebt = max(gapBeyondDebt, gap-media-sample.lateness+previous.lateness)
				}
				owned := sample
				previous = &owned
				last = sample.at
			}
			measured.wireMu.Unlock()
			require.Len(t, seen, packets)
			t.Logf("credit_polls=%d spurious_zero_credit=%d zero_cost_max=%d gap_beyond_debt=%s", credits.Polls, credits.SpuriousZero, credits.ZeroCostMax, gapBeyondDebt)
			for _, diagnostic := range credits.Diagnostics {
				t.Log(diagnostic)
			}
			require.Zero(t, credits.SpuriousZero, "no zero credit below budget without a hold")
			require.LessOrEqual(t, gapBeyondDebt, playbackRebaseAfter, "no backlogged starvation beyond pacer debt")
			s.mu.Lock()
			steadyMemory := s.agent.v2.bridge.memory.Load()
			s.mu.Unlock()
			require.Zero(t, steadyMemory)
			require.GreaterOrEqual(t, peakQueue, 5)
			t.Logf("size=%d pause=29s sent=%d refusals=0 closures=0 tokens=64 peak_queue=%d peak_retained=%d modeled_peak=%d bridge_memory_peak=%d wire_gap_max=%s gap_beyond_debt=%s credit_polls=%d spurious_zero_credit=%d credit_wait_max=%s catchup=%s steady_records=0 steady_packed=0", size, sent.Load(), peakQueue, peakRetained, peakCost, peakMemory, maxGap, gapBeyondDebt, credits.Polls, credits.SpuriousZero, creditWait, time.Since(resumed))
		})
	}
}
