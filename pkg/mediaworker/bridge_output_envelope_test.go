package mediaworker

import (
	"context"
	"crypto/sha256"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/stretchr/testify/require"
)

type bridgeEnvelopeAgent struct {
	*acceptedEventAgent
	move func(context.Context) error
}

func (a *bridgeEnvelopeAgent) SaveMoveOnly(ctx context.Context) ([]byte, error) {
	if a.move != nil {
		if err := a.move(ctx); err != nil {
			return nil, err
		}
	}
	return make([]byte, a.state.MoveOnlyLength), nil
}
func (a *bridgeEnvelopeAgent) RestoreMoveOnly(context.Context, []byte) error { return nil }

// Fill the bounded mailbox through production emission admission, leaving its
// terminal reservations intact. The actor has not started, so results cannot drain.
func growBridgeActorEnvelope(t *testing.T, rig acceptedEventRig) {
	t.Helper()
	s := rig.s
	rig.app.handle = nil
	rig.app.state.HasMoveOnly = true
	rig.app.state.MoveOnlyLength = s.agent.v2.bridge.limits.AdapterMoveOnlyBytes
	s.agent.v2.instance = &bridgeEnvelopeAgent{acceptedEventAgent: rig.app}
	_, err := (acceptedEvents{s}).submit(agent.Event{Kind: agent.TimerFired, ID: "grow", Timer: &agent.TimerNotice{ID: "grow"}}, eventLive)
	require.NoError(t, err)
	mailbox := 0
	for _, task := range s.agent.v2.tasks {
		mailbox += task.terminalBytes
	}
	task := s.agent.v2.tasks[0]
	payload := 0
	for {
		r := agent.TaskResult{TaskID: task.intent.ID, Sequence: 1, Turn: task.intent.Turn, Outcome: agent.OK, Chunk: agent.Chunk{Data: make([]byte, payload+1)}}
		if taskResultBytes(r)+mailbox > s.agent.v2.bridge.limits.ObserverMailboxBytes {
			break
		}
		payload++
	}
	reply := make(chan error, 1)
	s.acceptEmission(taskEmission{id: task.intent.ID, ctx: context.Background(), attempt: task.attempt, sequence: 1, chunk: agent.Chunk{Data: make([]byte, payload)}, reply: reply})
	require.NoError(t, <-reply)
	require.LessOrEqual(t, mailbox+task.mailboxBytes, s.agent.v2.bridge.limits.ObserverMailboxBytes)
	require.InDelta(t, s.agent.v2.bridge.limits.ObserverMailboxBytes, mailbox+task.mailboxBytes, 4)
}

func TestBridgeOutputFullActorEnvelopeAfterCredit(t *testing.T) {
	for _, size := range []int{160, 1200} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			rig, scope := newBridgeRig(t, false)
			count := fillBridgeToCap(t, rig.s, scope, size)
			maximumBridgeObservers(t, rig)
			growBridgeActorEnvelope(t, rig)
			s := rig.s
			require.NoError(t, bridgeExportReservationFits(s))
			s.mu.Lock()
			require.NoError(t, s.reserveV2ExportScratch())
			cost := s.bridgeExportCostLocked()
			s.agent.paused = true
			s.mu.Unlock()
			image, err := s.saveV2MoveOnly(context.Background())
			require.NoError(t, err)
			require.Len(t, image.payload, s.agent.v2.bridge.limits.AdapterMoveOnlyBytes)
			require.Len(t, image.tasks[0].Results, 1)
			require.GreaterOrEqual(t, len(image.tasks[0].Results[0].Chunk.Data), 11<<10, "mailbox must carry a complete maximum-size state candidate plus terminal reserves")
			data, err := s.checkpoint.capture(checkpointExact, &movePayload{Bridge: image.bridge, Tasks: image.tasks, Bytes: image.payload, Audio: image.audio})
			require.NoError(t, err)
			require.NoError(t, validateV2Wire(data))
			t.Logf("full envelope packet_bytes=%d packets=%d media=%s owned=%d cost=%d exact_bytes=%d", size, count, time.Duration(count)*20*time.Millisecond, s.moveReserved, cost, len(data))
		})
	}
}
func TestBridgeOutputObserverMailboxBound(t *testing.T) {
	rig, _ := newBridgeRig(t, false)
	maximumBridgeObservers(t, rig)
	growBridgeActorEnvelope(t, rig)
	s := rig.s
	task := s.agent.v2.tasks[0]
	task.lastEmission = time.Time{}
	before := task.mailboxBytes
	reply := make(chan error, 1)
	s.acceptEmission(taskEmission{id: task.intent.ID, ctx: context.Background(), attempt: task.attempt, sequence: 2, chunk: agent.Chunk{Data: make([]byte, 1024)}, reply: reply})
	require.ErrorIs(t, <-reply, agent.ErrBackpressure)
	require.Equal(t, before, task.mailboxBytes)
	require.ErrorIs(t, s.validateV2StateMetadata(agent.State{Version: 1, HasMoveOnly: true, MoveOnlyLength: s.agent.v2.bridge.limits.AdapterMoveOnlyBytes + 1}), agent.ErrCapacity)
	require.NoError(t, s.ctx.Err())
}

func TestBridgeOutputObserverBoundTradeoff(t *testing.T) {
	for _, bound := range []int{8 << 10, 16 << 10, 24 << 10, 32 << 10} {
		for _, size := range []int{160, 1200} {
			t.Run(fmt.Sprintf("%d-%d", bound, size), func(t *testing.T) {
				rig, scope := newBridgeRig(t, false)
				s := rig.s
				s.agent.v2.bridge.limits.ObserverRequestBytes = bound
				count := fillBridgeToCap(t, s, scope, size)
				maximumBridgeObservers(t, rig)
				growBridgeActorEnvelope(t, rig)
				if count > 0 {
					require.NoError(t, bridgeExportReservationFits(s))
				} else {
					require.Error(t, bridgeExportReservationFits(s))
				}
				t.Logf("observer_bound=%d packet_bytes=%d cap=%d media=%s", bound, size, count, time.Duration(count)*20*time.Millisecond)
				// Comparison only: simulate a 2 MiB budget, scaling safety headroom to
				// 20%, without changing production defaults or the four-second ceiling.
				s.worker.splitLimits.MoveBytesPerSession = 2 << 20
				s.agent.v2.registration.Bounds.MaxMailboxBytes = 2 << 20
				s.agent.v2.bridge.limits.ExportHeadroomBytes = (2 << 20) / 5
				s.agent.v2.bridge.limits.AudioDuration = time.Minute
				extra := 0
				for seq := uint64(count + 1); seq < 1000; seq++ {
					credit, err := s.worker.BridgeOutputCredit(scope)
					require.NoError(t, err)
					if credit.PacketSlots == 0 || credit.PayloadBytes < size {
						break
					}
					r := bridgeRequest(scope, seq, time.Now())
					r.data = bridgePacket(size)
					r.digest = sha256.Sum256(r.data)
					require.Equal(t, "admitted", s.admitBridgePacket(r).Outcome)
					extra++
				}
				t.Logf("comparison_only budget=2097152 safety=%d packet_bytes=%d export_cap=%d media=%s (production AudioDuration remains 4s)", (2<<20)/5, size, count+extra, time.Duration(count+extra)*20*time.Millisecond)
			})
		}
	}
}

func TestBridgeOutputCreditAndIngressDuringMove(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rig, scope := newBridgeRig(t, false)
		s := rig.s
		s.worker.cfg.Agent.CallbackTimeout = 10 * time.Millisecond
		entered, releaseSave := make(chan struct{}), make(chan struct{})
		rig.app.state.HasMoveOnly = true
		rig.app.state.MoveOnlyLength = 1
		s.agent.v2.instance = &bridgeEnvelopeAgent{acceptedEventAgent: rig.app, move: func(context.Context) error { close(entered); <-releaseSave; return nil }}
		_, err := (acceptedEvents{s}).submit(agent.Event{Kind: agent.Started}, eventLive)
		require.NoError(t, err)
		s.mu.Lock()
		s.agent.v2.playback.initialized = true
		s.agent.v2.playback.nextTS = s.state.Audio.InitialTS
		s.agent.v2.playback.nextAt = s.taskClockLocked().Add(time.Hour)
		s.mu.Unlock()
		startBridgeActor(t, s)
		synctest.Wait()
		credit, err := s.worker.BridgeOutputCredit(scope)
		require.NoError(t, err)
		require.Positive(t, credit.PacketSlots)
		// A credit obtained before the hold can still arrive at ingress during drain.
		s.agent.v2.bridge.reservedPackets.Add(1)
		moved := make(chan []byte, 1)
		go func() {
			data, release, err := s.export()
			if err != nil {
				t.Error(err)
			}
			if release != nil {
				defer release()
			}
			moved <- data
		}()
		synctest.Wait()
		creditDone := make(chan struct{})
		go func() {
			defer close(creditDone)
			for range 25 {
				c, e := s.worker.BridgeOutputCredit(scope)
				require.NoError(t, e)
				require.Zero(t, c.PacketSlots)
				time.Sleep(time.Microsecond)
			}
		}()
		ingressDone := make(chan BridgeOutputReceipt, 1)
		go func() {
			r, e := s.worker.SubmitBridgeOutput(context.Background(), scope, 1, 1, bridgePacket(160))
			require.NoError(t, e)
			ingressDone <- r
		}()
		synctest.Wait()
		require.Equal(t, "admitted", (<-ingressDone).Outcome)
		<-creditDone
		s.agent.v2.bridge.reservedPackets.Add(-1)
		s.agent.v2.bridge.signalChange()
		<-entered // export and its SaveMoveOnly are now live concurrently with ingress
		_, err = s.worker.BridgeOutputCredit(scope)
		require.ErrorIs(t, err, ErrBridgeOutputMoving)
		r, e := s.worker.SubmitBridgeOutput(context.Background(), scope, 1, 2, bridgePacket(160))
		require.NoError(t, e)
		require.Equal(t, "moving", r.Reason)
		require.False(t, r.Consumed)
		close(releaseSave)
		data := <-moved
		snap, e := decodeSnapshot(data)
		require.NoError(t, e)
		require.EqualValues(t, 1, snap.State.Agent.V2.Bridge.Through)
		require.Len(t, snap.AgentMoveOnly.Bridge.Queue, 1)
		require.False(t, s.agent.failed.Load())
	})
}

func TestBridgeOutputActorEnvelopeProfileValidation(t *testing.T) {
	for _, p := range []BridgeOutputLimits{{ObserverMailboxBytes: -1}, {ObserverMailboxBytes: 64 << 10}, {AdapterMoveOnlyBytes: -1}, {AdapterMoveOnlyBytes: 12 << 10}} {
		require.ErrorIs(t, p.Validate(), agent.ErrCapacity)
	}
	rig, _ := newBridgeRig(t, false)
	rig.s.worker.cfg.Agent.BridgeOutput.ObserverRequestBytes = 32 << 10
	rig.s.worker.cfg.Agent.BridgeOutput.ExportHeadroomBytes = 512 << 10
	rig.s.state.Agent = agentState{}
	require.ErrorIs(t, rig.s.initAgent(false, ResumeOptions{}), agent.ErrCapacity, "reject a combination leaving no full-size output credit")
}
