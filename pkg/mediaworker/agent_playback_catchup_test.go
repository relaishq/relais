package mediaworker

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/relaishq/relais/internal/agenthost"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/stretchr/testify/require"
)

func catchUpPlaybackRig(t *testing.T, bridge bool, count int) acceptedEventRig {
	t.Helper()
	if bridge {
		rig, scope := newBridgeRig(t, false)
		for seq := 1; seq <= count; seq++ {
			require.Equal(t, "admitted", rig.s.admitBridgePacket(bridgeRequest(scope, uint64(seq), time.Now())).Outcome)
		}
		return rig
	}
	rig := newAcceptedEventRig(t, agenthost.Bounds{}, nil)
	s := rig.s
	packets := make([][]byte, count)
	for i := range packets {
		packets[i] = []byte{0xf8, 0xff, 0xfe}
	}
	r := agent.TaskResult{TaskID: "audio", Sequence: 1, Turn: 1, Outcome: agent.OK, Chunk: agent.Chunk{Audio: &agent.EncodedAudio{Format: "audio/opus", Packets: packets}}}
	intent := taskIntent{ID: r.TaskID, Turn: 1, Class: agent.ReadOnly, Phase: taskStarted, Published: 1}
	s.agent.v2.tasks = []*localTask{{intent: intent, results: []agent.TaskResult{r}, mailboxBytes: taskResultBytes(r), terminalBytes: taskTerminalBytes(intent)}}
	require.NoError(t, s.registerAudioLocked(r))
	installUnitPlayback(t, s, r)
	return rig
}

func TestPlaybackPacerCatchesUpRepeatedLateWakes(t *testing.T) {
	for _, bridge := range []bool{false, true} {
		name := "agent"
		if bridge {
			name = "bridge"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				rig := catchUpPlaybackRig(t, bridge, 51)
				s := rig.s
				p := &s.agent.v2.playback
				base := s.taskNow()
				_, err := s.paceOne()
				require.NoError(t, err)
				for wake := 1; wake <= 25; wake++ {
					// Every wake is 20 ms late; two packets recover the schedule.
					s.storeClock, s.storeClockAt = base.Add(time.Duration(wake)*40*time.Millisecond), time.Now()
					before := s.state.Audio.Packets
					delay, err := s.paceOne()
					require.NoError(t, err)
					require.EqualValues(t, 2, s.state.Audio.Packets-before)
					require.Equal(t, 20*time.Millisecond, delay)
					require.Equal(t, base.Add(time.Duration(1+2*wake)*20*time.Millisecond), p.nextAt)
					require.Equal(t, s.state.Audio.InitialTS+uint32(1+2*wake)*960, p.nextTS, "late wakes must not rebase media time")
				}
				require.EqualValues(t, 50, s.state.Audio.Packets-1, "sustain 50 packets per second after the first packet")
				require.Len(t, rig.wire.packets, 51)
				require.Empty(t, p.queue)
				require.Empty(t, p.bridgeQueue)
				require.Zero(t, p.duration)
				if bridge {
					require.Zero(t, s.agent.v2.bridge.state.RebaseTicks)
					require.Zero(t, s.agent.v2.bridge.state.RebaseThrough)
				}
			})
		})
	}
}

func TestPlaybackPacerBoundsCatchUpAtRebaseThreshold(t *testing.T) {
	for _, bridge := range []bool{false, true} {
		name := "agent"
		if bridge {
			name = "bridge"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				rig := catchUpPlaybackRig(t, bridge, 12)
				s := rig.s
				p := &s.agent.v2.playback
				base := s.taskNow()
				p.initialized, p.nextAt, p.nextTS = true, base, s.state.Audio.InitialTS
				s.storeClock, s.storeClockAt = base.Add(200*time.Millisecond), time.Now()
				for _, count := range []uint64{5, 5, 1} {
					before := s.state.Audio.Packets
					delay, err := s.paceOne()
					require.NoError(t, err)
					require.Equal(t, count, s.state.Audio.Packets-before, "one wake sends at most five overdue packets")
					require.Equal(t, base.Add(time.Duration(s.state.Audio.Packets)*20*time.Millisecond), p.nextAt)
					require.Equal(t, s.state.Audio.InitialTS+uint32(s.state.Audio.Packets)*960, p.nextTS)
					if s.state.Audio.Packets < 11 {
						require.Zero(t, delay, "remaining debt requests another wake")
					} else {
						require.Equal(t, 20*time.Millisecond, delay)
					}
				}
				_, err := s.paceOne()
				require.NoError(t, err)
				require.Len(t, rig.wire.packets, 11, "the future packet must wait")
				if bridge {
					require.Zero(t, s.agent.v2.bridge.state.RebaseTicks)
					require.Zero(t, s.agent.v2.bridge.state.RebaseThrough)
				}
			})
		})
	}
}

func TestPlaybackPacerRebasesOnceAfterLongStall(t *testing.T) {
	for _, bridge := range []bool{false, true} {
		name := "agent"
		if bridge {
			name = "bridge"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				rig := catchUpPlaybackRig(t, bridge, 3)
				s := rig.s
				p := &s.agent.v2.playback
				base := s.taskNow()
				p.initialized, p.nextAt, p.nextTS = true, base, s.state.Audio.InitialTS
				s.storeClock, s.storeClockAt = base.Add(220*time.Millisecond), time.Now()
				for _, count := range []uint64{1, 1, 2} {
					if count == 2 {
						s.storeClock = s.storeClock.Add(20 * time.Millisecond)
					}
					delay, err := s.paceOne()
					require.NoError(t, err)
					require.Equal(t, 20*time.Millisecond, delay)
					require.Equal(t, count, s.state.Audio.Packets)
					require.Equal(t, base.Add(220*time.Millisecond+time.Duration(count)*20*time.Millisecond), p.nextAt)
					require.Equal(t, s.state.Audio.InitialTS+10560+uint32(count)*960, p.nextTS, "skip exactly 220 ms of RTP time once")
					if bridge {
						require.EqualValues(t, 3, s.agent.v2.bridge.state.RebaseThrough)
						require.EqualValues(t, 10560-count*960, s.agent.v2.bridge.state.RebaseTicks)
					}
				}
				require.Len(t, rig.wire.packets, 2)
			})
		})
	}
}

func TestBridgeOutputPacerCatchesUpAcrossGap(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rig, scope := newBridgeRig(t, false)
		s := rig.s
		for seq := uint64(1); seq <= 7; seq++ {
			request := bridgeRequest(scope, seq, time.Now())
			if seq == 3 {
				request.ingress = time.Now().Add(-time.Second)
				require.Equal(t, "output_late", s.admitBridgePacket(request).Reason)
			} else {
				require.Equal(t, "admitted", s.admitBridgePacket(request).Outcome)
			}
		}
		p := &s.agent.v2.playback
		base := s.taskNow()
		p.initialized, p.nextAt, p.nextTS = true, base, s.state.Audio.InitialTS
		s.storeClock, s.storeClockAt = base.Add(100*time.Millisecond), time.Now()
		for _, count := range []uint64{5, 6} {
			delay, err := s.paceOne()
			require.NoError(t, err)
			require.Equal(t, count, s.state.Audio.Packets)
			require.Equal(t, base.Add(time.Duration(count)*20*time.Millisecond), p.nextAt, "the gap adds no wall-clock delay")
			require.Equal(t, s.state.Audio.InitialTS+uint32(count+1)*960, p.nextTS, "charge the gap's RTP time once")
			require.Equal(t, time.Duration(count-5)*20*time.Millisecond, delay)
		}
		require.Len(t, rig.wire.packets, 6)
		require.Empty(t, p.bridgeQueue)
		require.Zero(t, s.agent.v2.bridge.state.RebaseTicks)
		require.Zero(t, s.agent.v2.bridge.state.RebaseThrough)
	})
}

func TestAcceptedEventPlaybackRebasesAfterIdle(t *testing.T) {
	for _, idle := range []time.Duration{20 * time.Millisecond, 100 * time.Millisecond, 200 * time.Millisecond} {
		t.Run(idle.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				rig, _ := startAcceptedSpeech(t, agenthost.Bounds{}, [][]byte{{0xf8, 1}}, [][]byte{{0xf8, 2}, {0xf8, 3}, {0xf8, 4}})
				s := rig.s
				delivered, err := s.deliverTaskResult()
				require.NoError(t, err)
				require.True(t, delivered)
				_, err = s.paceOne()
				require.NoError(t, err)
				p := &s.agent.v2.playback
				require.Empty(t, p.queue)
				nextTS := p.nextTS
				s.storeClock, s.storeClockAt = p.nextAt.Add(idle), time.Now()
				delivered, err = s.deliverTaskResult()
				require.NoError(t, err)
				require.True(t, delivered)
				for sent := uint64(2); sent <= 4; sent++ {
					delay, err := s.paceOne()
					require.NoError(t, err)
					require.Equal(t, sent, s.state.Audio.Packets, "new speech after idle must not burst")
					require.Equal(t, nextTS+rtpClockTicks(idle)+uint32(sent-2)*960, s.state.Audio.LastTimestamp, "RTP time includes idle silence")
					require.Equal(t, 20*time.Millisecond, delay)
					_, err = s.paceOne()
					require.NoError(t, err)
					require.Equal(t, sent, s.state.Audio.Packets, "the successor waits for its deadline")
					s.storeClock = s.storeClock.Add(20 * time.Millisecond)
				}
				require.Empty(t, p.queue)
				require.Len(t, rig.wire.packets, 4)
			})
		})
	}
}

func TestBridgeOutputPlaybackRebasesAfterIdle(t *testing.T) {
	for _, idle := range []time.Duration{20 * time.Millisecond, 100 * time.Millisecond, 200 * time.Millisecond} {
		t.Run(idle.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				rig, scope := newBridgeRig(t, false)
				s := rig.s
				require.Equal(t, "admitted", s.admitBridgePacket(bridgeRequest(scope, 1, time.Now())).Outcome)
				_, err := s.paceOne()
				require.NoError(t, err)
				p := &s.agent.v2.playback
				require.Empty(t, p.bridgeQueue)
				nextTS := p.nextTS
				s.storeClock, s.storeClockAt = p.nextAt.Add(idle), time.Now()
				for seq := uint64(2); seq <= 4; seq++ {
					require.Equal(t, "admitted", s.admitBridgePacket(bridgeRequest(scope, seq, time.Now())).Outcome)
				}
				for sent := uint64(2); sent <= 4; sent++ {
					delay, err := s.paceOne()
					require.NoError(t, err)
					require.Equal(t, sent, s.state.Audio.Packets, "new speech after idle must not burst")
					require.Equal(t, nextTS+rtpClockTicks(idle)+uint32(sent-2)*960, s.state.Audio.LastTimestamp, "RTP time includes idle silence")
					require.Equal(t, 20*time.Millisecond, delay)
					_, err = s.paceOne()
					require.NoError(t, err)
					require.Equal(t, sent, s.state.Audio.Packets, "the successor waits for its deadline")
					s.storeClock = s.storeClock.Add(20 * time.Millisecond)
				}
				require.Empty(t, p.bridgeQueue)
				require.Len(t, rig.wire.packets, 4)
			})
		})
	}
}

func TestBridgeOutputIdleRebaseReplacesOldGapCoverage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rig, scope := newBridgeRig(t, false)
		s := rig.s
		require.Equal(t, "admitted", s.admitBridgePacket(bridgeRequest(scope, 1, time.Now())).Outcome)
		_, err := s.paceOne()
		require.NoError(t, err)
		require.Equal(t, "admitted", s.admitBridgePacket(bridgeRequest(scope, 2, time.Now())).Outcome)
		p := &s.agent.v2.playback
		s.storeClock, s.storeClockAt = p.nextAt.Add(300*time.Millisecond), time.Now()
		_, err = s.paceOne()
		require.NoError(t, err)
		require.Empty(t, p.bridgeQueue)
		require.Positive(t, s.agent.v2.bridge.state.RebaseTicks)
		nextTS := p.nextTS
		s.storeClock = p.nextAt.Add(100 * time.Millisecond)
		require.Equal(t, "output_late", s.admitBridgePacket(bridgeRequest(scope, 3, time.Now().Add(-time.Second))).Reason)
		require.Equal(t, "admitted", s.admitBridgePacket(bridgeRequest(scope, 4, time.Now())).Outcome)
		require.EqualValues(t, 4800, s.agent.v2.bridge.state.RebaseTicks, "idle starts fresh rebase coverage")
		require.EqualValues(t, 3, s.agent.v2.bridge.state.RebaseThrough)
		_, err = s.paceOne()
		require.NoError(t, err)
		require.Equal(t, nextTS+4800, s.state.Audio.LastTimestamp, "the idle rebase covers the gap once")
		require.Equal(t, nextTS+5760, p.nextTS)
		require.EqualValues(t, 3, s.state.Audio.Packets)
		require.Empty(t, p.bridgeQueue)
	})
}
