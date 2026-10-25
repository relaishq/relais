//go:build bridge_measurement

package mediaworker

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/agenthost"
	"github.com/relaishq/relais/internal/redistestenv"
	legacy "github.com/relaishq/relais/pkg/agent"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/relaishq/relais/pkg/storage"
	"github.com/stretchr/testify/require"
)

type bridgeBurstSaves struct {
	mu      sync.Mutex
	samples []time.Duration
}

type bridgeBurstAgent struct {
	state        agent.State
	saves        *bridgeBurstSaves
	requestBytes int
}

func (a *bridgeBurstAgent) Handle(_ context.Context, e agent.Event) (agent.Effects, error) {
	switch e.Kind {
	case agent.Started:
		// Session observers outlive the ten caller turns, whose floors are 1..10.
		return agent.Effects{StartTasks: []agent.TaskSpec{
			{Step: "s", Kind: "bridge.state/v1", Turn: 1000, Class: agent.ReadOnly, Resume: agent.Retry, Request: make([]byte, a.requestBytes/2)},
			{Step: "a", Kind: "bridge.audio/v1", Turn: 1000, Class: agent.ReadOnly, Resume: agent.Retry, Request: make([]byte, a.requestBytes/2)},
		}}, nil
	case agent.Control:
		a.state.Bytes[0]++
	}
	return agent.Effects{}, nil
}
func (a *bridgeBurstAgent) Save(context.Context) (agent.State, error) {
	start := time.Now()
	state := cloneV2State(a.state)
	a.saves.mu.Lock()
	a.saves.samples = append(a.saves.samples, time.Since(start))
	a.saves.mu.Unlock()
	return state, nil
}
func (a *bridgeBurstAgent) Restore(_ context.Context, s agent.State) error {
	a.state = cloneV2State(s)
	return nil
}

// Real-clock producer, actor, pacer and Redis checkpoints. ResumeSession runs
// the real registration/init/restore/adoption path. Both fixture workers use
// the same owner tenure: authenticated cross-epoch reconciliation is slice B.
func measureBridgeBurst(t *testing.T, controlled bool) bridgeMeasuredResult {
	t.Helper()
	rig, scope := newBridgeRig(t, false)
	source := rig.s
	redis, err := sessionstore.NewRedis(context.Background(), storage.RedisConfig{Addr: redistestenv.Address(t), Prefix: "bridge-burst:" + rand.Text() + ":"}, make([]byte, 32))
	require.NoError(t, err)
	counted := &bridgeMeasuredStore{Store: redis, component: redis}
	lease, err := redis.Claim(context.Background(), source.id, source.worker.localAddr, time.Hour)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, redis.Release(context.Background(), lease))
		require.NoError(t, redis.Close())
	})
	handler := func(ctx context.Context, _ *agent.TaskRun) error { <-ctx.Done(); return ctx.Err() }
	var factories atomic.Int64
	saves := &bridgeBurstSaves{}
	cpu := bridgeProcessCPU()
	profile := BridgeOutputLimits{}.Defaults()
	factory := agenthost.Factory(agenthost.Registration{Name: "bridge", Tasks: map[string]agent.TaskHandler{"bridge.state/v1": handler, "bridge.audio/v1": handler}, Factory: func(agent.FactoryArgs) agent.Agent {
		factories.Add(1)
		return &bridgeBurstAgent{state: agent.State{Version: 1, Bytes: make([]byte, 1024), History: make([]byte, 8192)}, requestBytes: profile.ObserverRequestBytes, saves: saves}
	}})
	source.worker.cfg.Agent = AgentConfig{Factory: factory, Registrations: map[string]legacy.Factory{"bridge": factory}, BridgeOutput: profile, QueueCapacity: 32, CallbackTimeout: 250 * time.Millisecond}.defaults()
	source.worker.cfg.SplitCheckpoints = true
	source.worker.cfg.SnapshotInterval = 100 * time.Millisecond
	source.worker.cfg.Relay.Owners = counted
	source.worker.cfg.Relay.LeaseTTL = time.Hour
	source.lease = lease
	source.storeClockAt = time.Time{}
	source.state.Agent = agentState{}
	require.NoError(t, source.initAgent(false, ResumeOptions{}))
	require.NoError(t, source.worker.BindBridgeOutput(scope))
	var current atomic.Pointer[session]
	current.Store(source)
	fixtureCtx, cancel := context.WithCancel(context.Background())
	var auxiliary sync.WaitGroup
	var wireMu sync.Mutex
	var wire []bridgeWireSample
	attach := func(w *Worker, wireConn *dtlsWireConn, lookup func() *session) {
		w.cfg.PacketTrace = func(p PacketTrace) {
			if p.Protocol != "SRTP" {
				return
			}
			s := lookup()
			q := s.agent.v2.playback.bridgeQueue
			if len(q) == 0 {
				t.Errorf("SRTP trace without bridge queue")
				return
			}
			sample := bridgeWireSample{id: binary.BigEndian.Uint64(s.plainBuf[13:21]), turn: q[0].Turn, at: p.SentAt, timestamp: p.Timestamp, backlog: len(q) > 1, lateness: max(0, p.SentAt.Sub(s.agent.v2.playback.nextAt))}
			wireMu.Lock()
			wire = append(wire, sample)
			wireMu.Unlock()
		}
		auxiliary.Add(1)
		go func() {
			defer auxiliary.Done()
			for {
				select {
				case <-fixtureCtx.Done():
					return
				case <-wireConn.packets:
				}
			}
		}()
	}
	attach(source.worker, rig.wire, func() *session { return source })
	startBridgeActor(t, source)
	require.Eventually(t, func() bool { source.mu.Lock(); defer source.mu.Unlock(); return source.state.Agent.V2.Started }, 5*time.Second, time.Millisecond)
	require.Equal(t, int64(profile.ObserverRequestBytes), source.moveRequestBytes.Load())
	auxiliary.Add(1)
	go func() { defer auxiliary.Done(); source.checkpoint.run(source.ctx) }()
	auxiliary.Add(1)
	go func() {
		defer auxiliary.Done()
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-fixtureCtx.Done():
				return
			case <-ticker.C:
				s := current.Load()
				s.mu.Lock()
				sent := s.agent.v2.bridge.state.Sent
				s.mu.Unlock()
				e := s.worker.AdvanceBridgeReplayHorizon(scope, sent)
				if e != nil && !errors.Is(e, ErrBridgeOutputMoving) && s.ctx.Err() == nil {
					t.Errorf("producer horizon: %v", e)
					return
				}
			}
		}
	}()
	var target *Worker
	t.Cleanup(func() {
		cancel()
		source.cancel()
		if target != nil {
			if s := target.session(source.id); s != nil {
				s.fenced.Store(true)
				s.cancel()
				s.close()
			}
			target.running.Wait()
		}
		auxiliary.Wait()
	})
	result := bridgeMeasuredResult{ControlledMove: controlled, Burst: true, Name: "160-byte-burst", Produced: 400, ExportCostLimit: int64((1 << 20) - profile.ActorReserveBytes - profile.ExportHeadroomBytes)}
	start := time.Now()
	sourceQueue := make(chan bridgeMeasuredPacket, 32)
	var produced atomic.Int64
	var blocked atomic.Int64
	var sourcePeak atomic.Int64
	var readyMu sync.Mutex
	ready := map[uint64]time.Time{}
	first := map[agent.Turn]uint64{}
	producerDone := make(chan struct{})
	turnDone := make(chan struct{}, 1)
	go func() {
		defer close(producerDone)
		defer close(sourceQueue)
		turnStart := start
		for i := 0; i < 400; i++ {
			if i > 0 && i%40 == 0 {
				select {
				case <-turnDone:
				case <-fixtureCtx.Done():
					return
				}
				turnStart = time.Now()
			}
			if d := time.Until(turnStart.Add(time.Duration(i%40) * 5 * time.Millisecond)); d > 0 {
				time.Sleep(d)
			}
			packet := bridgeMeasuredPacket{seq: uint64(i + 1), turn: agent.Turn(i/40 + 1), ready: time.Now(), payload: bridgePacket(160)}
			binary.BigEndian.PutUint64(packet.payload[1:9], packet.seq)
			readyMu.Lock()
			ready[packet.seq] = packet.ready
			if first[packet.turn] == 0 {
				first[packet.turn] = packet.seq
			}
			readyMu.Unlock()
			produced.Add(1)
			at := time.Now()
			select {
			case sourceQueue <- packet:
			case <-fixtureCtx.Done():
				return
			}
			blocked.Add(int64(time.Since(at)))
			for old := sourcePeak.Load(); int64(len(sourceQueue)) > old; old = sourcePeak.Load() {
				if sourcePeak.CompareAndSwap(old, int64(len(sourceQueue))) {
					break
				}
			}
		}
	}()
	observerDone := make(chan struct{})
	go func() {
		defer close(observerDone)
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-fixtureCtx.Done():
				return
			case <-tick.C:
				s := current.Load()
				s.mu.Lock()
				cost := s.bridgeExportCostLocked()
				result.ExportCostMax = max(result.ExportCostMax, cost)
				result.MemoryPeak = max(result.MemoryPeak, s.agent.v2.bridge.memory.Load())
				if cost > result.ExportCostLimit {
					result.CapViolations++
				}
				if !s.checkpoint.lastCheckpoint.IsZero() {
					result.CheckpointMaxAgeMS = max(result.CheckpointMaxAgeMS, float64(time.Since(s.checkpoint.lastCheckpoint))/float64(time.Millisecond))
				}
				q := s.agent.v2.playback
				result.PacerQueue = max(result.PacerQueue, len(q.bridgeQueue))
				result.PacerDebtMS = max(result.PacerDebtMS, float64(q.duration+max(0, time.Until(q.nextAt)))/float64(time.Millisecond))
				s.mu.Unlock()
			}
		}
	}()
	t.Cleanup(func() { cancel(); <-producerDone; <-observerDone })
	canceled := map[uint64]bool{}
	var latencies []time.Duration
	turn := agent.Turn(0)
	controlSeq := uint64(0)
	for packet := range sourceQueue {
		s := current.Load()
		if packet.turn != turn {
			if turn > 0 {
				require.NoError(t, s.worker.EndBridgeOutput(scope, turn, packet.seq-1))
			}
			interruption, e := s.worker.InterruptBridgeOutput(context.Background(), scope, fmt.Sprint("burst-clear-", packet.turn), packet.turn, true)
			require.NoError(t, e)
			for _, id := range interruption.Canceled {
				canceled[id] = true
			}
			_, e = s.worker.BeginBridgeOutput(scope, packet.turn)
			require.NoError(t, e)
			turn = packet.turn
		}
		wait := time.Now()
		for {
			credit, e := s.worker.BridgeOutputCredit(scope)
			require.NoError(t, e)
			if credit.PacketSlots > 0 && credit.PayloadBytes >= 160 {
				break
			}
			require.Less(t, time.Since(wait), 5*time.Second, "bounded producer credit wait")
			time.Sleep(time.Millisecond)
		}
		result.CreditWaitMS += float64(time.Since(wait)) / float64(time.Millisecond)
		receipt, e := s.worker.SubmitBridgeOutput(context.Background(), scope, packet.turn, packet.seq, packet.payload)
		require.NoError(t, e)
		latencies = append(latencies, time.Since(packet.ready))
		if receipt.Outcome == "admitted" {
			result.Admitted++
		} else {
			result.Refused++
			if receipt.Reason == "output_late" {
				result.Late++
			}
		}
		if packet.seq%50 == 0 {
			controlSeq++
			_, e = s.worker.AgentControl(context.Background(), s.id, agent.ControlMessage{Sequence: controlSeq, ID: fmt.Sprint("burst-gap-", packet.seq), Name: "input_gap"})
			require.NoError(t, e)
			result.Controls++
			result.InputGaps++
		}
		if packet.seq == 165 {
			s.mu.Lock()
			before := len(s.agent.v2.playback.bridgeQueue)
			cost := s.bridgeExportCostLocked()
			// Tighten the test cut, not the operational credit cap. There must be
			// actual retirement before the planned move can pause.
			cut := cost - 2400
			s.agent.v2.bridge.limits.ExportHeadroomBytes = (1 << 20) - profile.ActorReserveBytes - int(cut)
			require.NoError(t, s.agent.v2.bridge.limits.Validate())
			result.MoveOverage = cost - s.bridgeLookaheadBudget()
			targetCfg := s.worker.cfg
			targetCfg.Agent.BridgeOutput = s.agent.v2.bridge.limits
			s.mu.Unlock()
			var targetWire *dtlsWireConn
			target, targetWire = bridgeResumeWorker(targetCfg, s.worker.localAddr)
			attach(target, targetWire, func() *session { return target.session(source.id) })
			moveAt := time.Now()
			producedBefore := produced.Load()
			data, release, e := s.export()
			result.MoveExportMS = float64(time.Since(moveAt)) / float64(time.Millisecond)
			require.NoError(t, e)
			snap, e := decodeSnapshot(data)
			require.NoError(t, e)
			require.NotNil(t, snap.AgentMoveOnly.Bridge)
			require.EqualValues(t, packet.seq, snap.State.Agent.V2.Bridge.Through)
			s.mu.Lock()
			after := len(s.agent.v2.playback.bridgeQueue)
			s.mu.Unlock()
			require.Less(t, after, before, "drain-to-fit must retire queue entries")
			id, e := target.ResumeSession(data, ResumeOptions{Lease: lease})
			require.NoError(t, e)
			dst := target.session(id)
			require.NotNil(t, dst)
			require.NotNil(t, dst.agent.v2.bridge)
			require.Equal(t, int64(profile.ObserverRequestBytes), dst.moveRequestBytes.Load(), "production restoreTasks must own both observer requests")
			dst.mu.Lock()
			require.EqualValues(t, packet.seq, dst.agent.v2.bridge.state.Through)
			dst.agent.v2.bridge.limits.ExportHeadroomBytes = profile.ExportHeadroomBytes
			dst.mu.Unlock()
			current.Store(dst)
			release()
			s.cancel()
			result.RealMove = true
			result.MoveTotalMS = float64(time.Since(moveAt)) / float64(time.Millisecond)
			result.MoveProduced = produced.Load() - producedBefore
			require.Positive(t, result.MoveProduced, "producer must still run during the move")
			require.EqualValues(t, 2, factories.Load(), "resume must select the registration and construct a fresh agent")
			t.Logf("burst move over_target=%d queue=%d->%d elapsed_ms=%.3f produced=%d exact_through=%d", result.MoveOverage, before, after, result.MoveTotalMS, result.MoveProduced, packet.seq)
		}
		if packet.seq%40 == 0 {
			turnDone <- struct{}{}
		}
	}
	<-producerDone
	s := current.Load()
	require.NoError(t, s.worker.EndBridgeOutput(scope, turn, 400))
	require.Eventually(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return len(s.agent.v2.playback.bridgeQueue) == 0 }, 5*time.Second, 5*time.Millisecond)
	cancel()
	<-observerDone
	result.Canceled = len(canceled)
	result.ProducerBlockedMS = float64(blocked.Load()) / float64(time.Millisecond)
	result.SourceQueue = int(sourcePeak.Load())
	result.RawP95MS = float64(bridgeRawP95(latencies)) / float64(time.Millisecond)
	result.CheckpointWrites = counted.writes.Load()
	result.CheckpointBytes = counted.bytes.Load()
	result.TrackedIDs = len(latencies)
	if s.ctx.Err() != nil || s.agent.failed.Load() || source.agent.failed.Load() {
		result.Closures++
	}
	wireMu.Lock()
	samples := append([]bridgeWireSample(nil), wire...)
	wireMu.Unlock()
	seen := map[uint64]bool{}
	var firstLatency []time.Duration
	var previous *bridgeWireSample
	readyMu.Lock()
	for _, sample := range samples {
		if seen[sample.id] || canceled[sample.id] {
			result.DuplicateWireIDs++
		}
		seen[sample.id] = true
		if sample.id == first[sample.turn] {
			firstLatency = append(firstLatency, sample.at.Sub(ready[sample.id]))
		}
		result.WireSchedulerDebtMS = max(result.WireSchedulerDebtMS, float64(sample.lateness)/float64(time.Millisecond))
		if previous != nil && previous.backlog {
			gap := sample.at.Sub(previous.at)
			media := time.Duration(uint32(sample.timestamp-previous.timestamp)) * time.Second / 48000
			result.WireGapMS = max(result.WireGapMS, float64(gap)/float64(time.Millisecond))
			result.GapBeyondDebtMS = max(result.GapBeyondDebtMS, float64(gap-media-sample.lateness+previous.lateness)/float64(time.Millisecond))
		}
		copy := sample
		previous = &copy
	}
	readyMu.Unlock()
	for id := uint64(1); id <= 400; id++ {
		if !seen[id] && !canceled[id] {
			result.MissingWireIDs++
		}
	}
	result.Sent = len(samples)
	result.FirstWireSamples = len(firstLatency)
	result.FirstWireP95MS = float64(bridgeRawP95(firstLatency)) / float64(time.Millisecond)
	result.CPUSeconds = bridgeProcessCPU() - cpu
	saves.mu.Lock()
	result.SaveCalls = uint64(len(saves.samples))
	result.SaveP95MS = float64(bridgeRawP95(saves.samples)) / float64(time.Millisecond)
	for _, d := range saves.samples {
		result.SaveWallMS += float64(d) / float64(time.Millisecond)
		result.SaveMaxMS = max(result.SaveMaxMS, float64(d)/float64(time.Millisecond))
	}
	saves.mu.Unlock()
	result.Qualified = bridgeLatencyQualifies(result)
	encoded, e := json.Marshal(result)
	require.NoError(t, e)
	t.Log(string(encoded))
	return result
}
