package callharness

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/agenthost"
	"github.com/relaishq/relais/internal/workerprobe"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

func streamRegs(packets [][]byte, chunk, limit int) ([]agenthost.Registration, []chan agent.Event, chan struct{}, chan time.Duration) {
	gate := make(chan struct{})
	done := make(chan time.Duration, 2)
	events := []chan agent.Event{make(chan agent.Event, 4096), make(chan agent.Event, 4096)}
	regs := make([]agenthost.Registration, 2)
	for i := range regs {
		regs[i] = agenthost.Registration{Name: "pacer/v1", Bounds: agenthost.Bounds{MaxMailboxBytes: limit}, Factory: func(agent.FactoryArgs) agent.Agent { return &pacerHarnessAgent{events: events[i]} }, Tasks: map[string]agent.TaskHandler{"tts/v1": func(ctx context.Context, run *agent.TaskRun) error {
			select {
			case <-gate:
			case <-ctx.Done():
				return ctx.Err()
			}
			start := time.Now()
			for offset := 0; offset < len(packets); offset += chunk {
				end := min(offset+chunk, len(packets))
				if err := run.Emit(ctx, uint64(offset/chunk+1), agent.Chunk{Audio: &agent.EncodedAudio{Format: "audio/opus", Packets: packets[offset:end]}}); err != nil {
					return err
				}
			}
			done <- time.Since(start)
			return nil
		}}}
	}
	return regs, events, gate, done
}
func collectPackets(t *testing.T, ch <-chan pacedPacket, n int, timeout time.Duration) []pacedPacket {
	t.Helper()
	all := make([]pacedPacket, 0, n)
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for len(all) < n {
		select {
		case p := <-ch:
			all = append(all, p)
		case <-timer.C:
			require.Equal(t, n, len(all), "streaming speech must finish without ending the call")
			return all
		}
	}
	return all
}
func TestPacerStreamingCadence(t *testing.T) {
	if testing.Short() {
		t.Skip("streaming cadence matrix; bounded-window unit proof runs in short mode")
	}
	for _, tc := range []struct {
		name     string
		packet   []byte
		duration time.Duration
		n        int
	}{{"10ms", []byte{0xf0, 0xff, 0xfe}, 10 * time.Millisecond, 150}, {"2.5ms", []byte{0x80, 0xff, 0xfe}, 2500 * time.Microsecond, 400}, {"20ms-live-tasks", []byte{0xf8, 0xff, 0xfe}, 20 * time.Millisecond, 80}} {
		t.Run(tc.name, func(t *testing.T) {
			forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
				packets := make([][]byte, tc.n)
				for i := range packets {
					packets[i] = tc.packet
				}
				// Emit publishes at most every 100 ms. Batch packet durations while
				// retaining the same 10 ms/2.5 ms/20 ms packet-level pacer proof.
				regs, events, gate, _ := streamRegs(packets, int(100*time.Millisecond/tc.duration), 0)
				if tc.name == "20ms-live-tasks" {
					for i := range regs {
						regs[i].Factory = func(agent.FactoryArgs) agent.Agent {
							return &multiTaskAgent{pacerHarnessAgent: pacerHarnessAgent{events: events[i]}}
						}
						regs[i].Tasks["idle/v1"] = func(ctx context.Context, _ *agent.TaskRun) error { <-ctx.Done(); return ctx.Err() }
					}
				}
				sys := startAgentHarness(t, store, agentHarnessSettings{v2: regs})
				ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
				defer cancel()
				call, err := sys.h.Dial(ctx, CallOptions{})
				require.NoError(t, err)
				received := receivePacer(call)
				close(gate)
				all := collectPackets(t, received, tc.n, 4*time.Second)
				nominal := time.Duration(tc.n-1) * tc.duration
				actual := all[len(all)-1].at.Sub(all[0].at)
				t.Logf("CADENCE nominal=%s actual=%s error=%.3f%%", nominal, actual, 100*float64(actual-nominal)/float64(nominal))
				require.InDelta(t, float64(nominal), float64(actual), float64(nominal)/50+float64(15*time.Millisecond))
				for i := 1; i < len(all); i++ {
					require.Equal(t, all[i-1].packet.SequenceNumber+1, all[i].packet.SequenceNumber)
				}
				_, _, err = sys.workers[0].SessionAgent(call.SessionID())
				require.NoError(t, err)
				_, err = call.Hangup(ctx)
				require.NoError(t, err)
			})
		})
	}
}
func TestPacerEightyPacketCadence(t *testing.T) {
	if testing.Short() {
		t.Skip("80-packet cadence; deterministic clock unit proof remains")
	}
	forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
		regs, _, gate, _ := pacerRegistrations(t, 80, false, false)
		sys := startAgentHarness(t, store, agentHarnessSettings{v2: regs})
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		defer cancel()
		call, err := sys.h.Dial(ctx, CallOptions{})
		require.NoError(t, err)
		received := receivePacer(call)
		close(gate)
		all := collectPackets(t, received, 80, 3*time.Second)
		span := all[79].at.Sub(all[0].at)
		t.Logf("CADENCE_80 nominal=1.58s actual=%s error=%.3f%%", span, 100*float64(span-1580*time.Millisecond)/float64(1580*time.Millisecond))
		require.InDelta(t, float64(1580*time.Millisecond), float64(span), float64(1580*time.Millisecond)/50)
		_, err = call.Hangup(ctx)
		require.NoError(t, err)
	})
}
func TestPacerSaturatedMailbox(t *testing.T) {
	if testing.Short() {
		t.Skip("12-second speech with mailbox backpressure; admission unit proof remains")
	}
	forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
		fixture := pacerFixtures(t, 80)
		packets := make([][]byte, 600)
		for i := range packets {
			packets[i] = fixture[i%len(fixture)]
		}
		regs, _, gate, done := streamRegs(packets, 50, 128<<10)
		sys := startAgentHarness(t, store, agentHarnessSettings{v2: regs})
		ctx, cancel := context.WithTimeout(context.Background(), 18*time.Second)
		defer cancel()
		call, err := sys.h.Dial(ctx, CallOptions{})
		require.NoError(t, err)
		received := receivePacer(call)
		close(gate)
		all := collectPackets(t, received, 600, 15*time.Second)
		for i, p := range all {
			require.Equal(t, packets[i], p.packet.Payload, "packet %d", i)
			if i > 0 {
				require.Equal(t, all[i-1].packet.SequenceNumber+1, p.packet.SequenceNumber, "sequence %d", i)
				require.GreaterOrEqual(t, uint32(p.packet.Timestamp-all[i-1].packet.Timestamp), uint32(960), "timestamp %d", i)
			}
		}
		select {
		case duration := <-done:
			// Twelve publications need only 1.1 s under the Emit rate limit.
			// Exceeding 3 s proves that the 128 KiB mailbox also blocks them.
			require.Greater(t, duration, 3*time.Second, "Emit must wait for mailbox space from paced drain")
		case <-ctx.Done():
			t.Fatal("producer did not complete")
		}
		_, _, err = sys.workers[0].SessionAgent(call.SessionID())
		require.NoError(t, err)
		_, err = call.Hangup(ctx)
		require.NoError(t, err)
	})
}

type multiTaskAgent struct {
	pacerHarnessAgent
	barge bool
}

func (a *multiTaskAgent) Handle(ctx context.Context, e agent.Event) (agent.Effects, error) {
	effects, err := a.pacerHarnessAgent.Handle(ctx, e)
	if e.Kind == agent.Started {
		for _, step := range []string{"stt", "llm"} {
			effects.StartTasks = append(effects.StartTasks, agent.TaskSpec{Step: step, Kind: "idle/v1", Turn: 1, Class: agent.ReadOnly, Resume: agent.Retry, Request: []byte("x")})
		}
	}
	if e.Kind == agent.AudioInput && a.barge {
		turn := agent.Turn(2)
		effects.ClearAudioBefore = &turn
		effects.CancelBefore = &turn
		effects.StartTasks = []agent.TaskSpec{{Step: "next-stt", Kind: "next/v1", Turn: 2, Class: agent.ReadOnly}}
	}
	return effects, err
}
func TestPacerBargeInThreeLiveTasks(t *testing.T) {
	if testing.Short() {
		t.Skip("live-task barge-in matrix; cleared-window unit proof remains")
	}
	forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
		fixture := pacerFixtures(t, 80)
		regs, events, gate, _ := streamRegs(fixture, 1, 0)
		started := make(chan time.Time, 1)
		for i := range regs {
			regs[i].Factory = func(agent.FactoryArgs) agent.Agent {
				return &multiTaskAgent{pacerHarnessAgent: pacerHarnessAgent{events: events[i]}, barge: true}
			}
			regs[i].Tasks["idle/v1"] = func(ctx context.Context, _ *agent.TaskRun) error { <-ctx.Done(); return ctx.Err() }
			regs[i].Tasks["next/v1"] = func(context.Context, *agent.TaskRun) error { started <- time.Now(); return nil }
		}
		sys := startAgentHarness(t, store, agentHarnessSettings{v2: regs})
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		defer cancel()
		call, err := sys.h.Dial(ctx, CallOptions{})
		require.NoError(t, err)
		received := receivePacer(call)
		close(gate)
		for range 6 {
			nextPaced(t, received)
		}
		sent := time.Now()
		require.NoError(t, call.SendMedia(ctx, 20*time.Millisecond))
		var accepted time.Time
		select {
		case accepted = <-started:
		case <-time.After(40 * time.Millisecond):
			t.Fatal("barge-in is blocked behind audio refills")
		}
		t.Logf("BARGE_IN committed next task in %s", accepted.Sub(sent))
		require.Less(t, accepted.Sub(sent), 40*time.Millisecond)
		timer := time.NewTimer(60 * time.Millisecond)
		defer timer.Stop()
		after := 0
	drain:
		for {
			select {
			case p := <-received:
				if p.at.After(accepted) {
					after++
				}
			case <-timer.C:
				break drain
			}
		}
		require.LessOrEqual(t, after, 1, "clear must stop old-turn audio within one packet")
		_, err = call.Hangup(ctx)
		require.NoError(t, err)
	})
}

type restartAgent struct {
	pacerHarnessAgent
	resumed, started bool
}

func (a *restartAgent) Handle(ctx context.Context, e agent.Event) (agent.Effects, error) {
	if e.Kind == agent.Resumed {
		a.resumed = true
	}
	if e.Kind == agent.AudioInput && a.resumed && !a.started {
		a.started = true
		return agent.Effects{StartTasks: []agent.TaskSpec{{Step: "tts2", Kind: "tts/v1", Turn: 2, Class: agent.ReadOnly, Resume: agent.Retry, Request: []byte("again")}}}, nil
	}
	return a.pacerHarnessAgent.Handle(ctx, e)
}
func TestPacerResumeTimestamp(t *testing.T) {
	if testing.Short() {
		t.Skip("move/takeover store-clock timestamp matrix; deterministic rebase proof remains")
	}
	for _, takeover := range []bool{false, true} {
		t.Run(fmt.Sprint("takeover=", takeover), func(t *testing.T) {
			forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
				regs, events, gate, _ := pacerRegistrations(t, 20, false, false)
				for i := range regs {
					regs[i].Factory = func(agent.FactoryArgs) agent.Agent {
						return &restartAgent{pacerHarnessAgent: pacerHarnessAgent{events: events[i]}}
					}
				}
				sys := startAgentHarness(t, store, agentHarnessSettings{v2: regs, remoteTarget: true})
				ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
				defer cancel()
				call, err := sys.h.Dial(ctx, CallOptions{})
				require.NoError(t, err)
				received := receivePacer(call)
				close(gate)
				before := collectPackets(t, received, 20, 2*time.Second)
				last := before[len(before)-1]
				if takeover {
					require.NoError(t, workerprobe.Kill(sys.workers[0].LocalAddr()))
				} else {
					_, err = sys.plane.Move(ctx, call.SessionID(), "1")
					require.NoError(t, err)
				}
				awaitPacerEvent(t, events[1], agent.Resumed)
				pause := time.NewTimer(150 * time.Millisecond)
				<-pause.C
				require.NoError(t, call.SendMedia(ctx, 20*time.Millisecond))
				first := nextPaced(t, received)
				actual := time.Duration(int32(first.packet.Timestamp-last.packet.Timestamp)) * time.Second / 48000
				elapsed := first.at.Sub(last.at)
				t.Logf("RESUME_CLOCK rtp=%s wall=%s seq_delta=%d", actual, elapsed, uint16(first.packet.SequenceNumber-last.packet.SequenceNumber))
				require.InDelta(t, float64(elapsed), float64(actual), float64(40*time.Millisecond))
				require.Greater(t, int32(first.packet.Timestamp-last.packet.Timestamp), int32(960))
				_, err = call.Hangup(ctx)
				require.NoError(t, err)
			})
		})
	}
}
