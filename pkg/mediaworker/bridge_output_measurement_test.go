//go:build bridge_measurement

package mediaworker

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/redistestenv"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/relaishq/relais/pkg/storage"
	"github.com/stretchr/testify/require"
)

const (
	bridgeLatencyTarget  = 150 * time.Millisecond
	bridgeRecoveryTarget = 500 * time.Millisecond
	bridgePacketDuration = 20 * time.Millisecond
	bridgeBurstRate      = 200
	bridgeBurstDuration  = 2 * time.Second
)

type bridgeMeasuredStore struct {
	sessionstore.Store
	component   sessionstore.ComponentStore
	stalling    atomic.Bool // controlled Redis pause; transient errors must retry
	writes      atomic.Uint64
	bytes       atomic.Uint64
	renewMu     sync.Mutex
	renewAt     time.Time
	renewalGaps []time.Duration
}

func (s *bridgeMeasuredStore) ReadComponents(ctx context.Context, id string) (sessionstore.ComponentTuple, error) {
	return s.component.ReadComponents(ctx, id)
}
func (s *bridgeMeasuredStore) CommitComponents(ctx context.Context, l sessionstore.Lease, c sessionstore.ComponentCommit) (sessionstore.ComponentTuple, error) {
	s.writes.Add(1)
	s.bytes.Add(uint64(len(c.Components.Setup) + len(c.Components.Transport) + len(c.Components.Work) + len(c.Components.History)))
	return s.component.CommitComponents(ctx, l, c)
}

// The component store methods below are discovered through its interface; all
// methods retain the real encrypted Redis adapter and existing fence checks.
func (s *bridgeMeasuredStore) ComponentKeyInventory(ctx context.Context) (map[byte]uint64, error) {
	return s.component.ComponentKeyInventory(ctx)
}

type bridgeMeasuredAgent struct {
	refusals atomic.Uint64
	agent.Agent
	mu     sync.Mutex
	saves  []time.Duration
	wireMu sync.Mutex
	wire   []bridgeWireSample
	ready  map[uint64]time.Time
	turns  map[uint64]agent.Turn
}
type bridgeWireSample struct {
	id        uint64
	turn      agent.Turn
	at        time.Time
	timestamp uint32
	backlog   bool
	lateness  time.Duration
}

func (a *bridgeMeasuredAgent) Handle(ctx context.Context, e agent.Event) (agent.Effects, error) {
	if e.Result != nil && e.Result.Outcome == agent.Capacity {
		a.refusals.Add(1)
	}
	return a.Agent.Handle(ctx, e)
}
func (a *bridgeMeasuredAgent) Save(ctx context.Context) (agent.State, error) {
	at := time.Now()
	state, err := a.Agent.Save(ctx)
	elapsed := time.Since(at)
	a.mu.Lock()
	a.saves = append(a.saves, elapsed)
	a.mu.Unlock()
	return state, err
}

type bridgeMeasuredPacket struct {
	seq     uint64
	turn    agent.Turn
	payload []byte
	ready   time.Time
}
type bridgeMeasuredPhase struct {
	name                                                                            string
	duration                                                                        time.Duration
	rate, size                                                                      int
	rapid, jitter, tails, upstream, overload, burst, controlledMove, sustainedBurst bool
}

// Observe the actual public response. Real-time producers and the stall fixture
// have spare packet/audio/metadata lanes; zero credit below the export allowance
// is only allowed during an explicit move/pause/fence hold. Burst/overload phases
// still exercise their intentional capacity backpressure separately.
type bridgeCreditSamples struct {
	Polls, SpuriousZero int
	ZeroCostMax         int64
	Diagnostics         []string
}

func (c *bridgeCreditSamples) read(s *session, scope BridgeOutputScope) (BridgeOutputCredit, error) {
	credit, err := s.worker.BridgeOutputCredit(scope)
	c.Polls++
	if err == nil && (credit.PacketSlots == 0 || credit.PayloadBytes == 0) {
		s.mu.Lock()
		b := s.agent.v2.bridge
		held := b.draining > 0 || s.agent.paused || s.worker.paused.Load() || s.fenced.Load()
		cost := s.bridgeExportCostLocked()
		if !held && cost < s.bridgeLookaheadBudget() {
			c.SpuriousZero++
			c.ZeroCostMax = max(c.ZeroCostMax, cost)
			if len(c.Diagnostics) < 8 {
				metadata, encodeErr := bridgeInterruptionCharge(b.state, s.playbackSnapshotLocked())
				c.Diagnostics = append(c.Diagnostics, fmt.Sprintf("through=%d sent=%d pruned=%d queue=%d cost=%d metadata=%d+%d/%d encode=%v memory_guard=%v audio=%d/%d duration=%s/%s reserved=%d/%d exceptions=%d proven=%v", b.state.Through, b.state.Sent, b.state.Pruned, len(s.agent.v2.playback.bridgeQueue), cost, metadata, bridgeTerminalReserve, bridgeMetadataPartition, encodeErr, s.bridgeCreditExhaustedLocked(), b.audioBytes, b.limits.AudioBytes, b.audioDuration, b.limits.AudioDuration, b.reservedPackets.Load(), b.reservedBytes.Load(), len(b.state.Exceptions), b.provenUnadmitted))
			}
		}
		s.mu.Unlock()
	}
	return credit, err
}

type bridgeMeasuredResult struct {
	CreditPolls         int       `json:"credit_polls"`
	SpuriousZeroCredit  int       `json:"spurious_zero_credit"`
	SustainedBurst      bool      `json:"sustained_burst"`
	FirstWireSamples    int       `json:"first_ready_to_wire_samples"`
	RealMove            bool      `json:"production_resume_move_verified"`
	MoveExportMS        float64   `json:"move_export_ms"`
	MoveTotalMS         float64   `json:"move_total_ms"`
	MoveOverage         int64     `json:"move_over_target_bytes"`
	MoveProduced        int64     `json:"produced_during_move"`
	ControlledMove      bool      `json:"controlled_exact_move_verified"`
	Burst               bool      `json:"burst"`
	FirstWireP95MS      float64   `json:"first_ready_to_wire_p95_ms"`
	CreditWaitMS        float64   `json:"credit_wait_ms"`
	ProducerBlockedMS   float64   `json:"producer_blocked_ms"`
	WireGapMS           float64   `json:"maximum_wire_gap_ms"`
	WireSchedulerDebtMS float64   `json:"maximum_wire_scheduler_debt_ms"`
	GapBeyondDebtMS     float64   `json:"maximum_wire_gap_beyond_debt_ms"`
	ExportCostMax       int64     `json:"maximum_projected_export_cost"`
	ExportCostLimit     int64     `json:"export_cost_limit"`
	MemoryPeak          int64     `json:"maximum_bridge_memory"`
	CapViolations       int       `json:"cap_violations"`
	DuplicateWireIDs    int       `json:"duplicate_wire_ids"`
	MissingWireIDs      int       `json:"missing_wire_ids"`
	ActorEvents         uint64    `json:"actor_events,omitempty"`
	PartialTails        int       `json:"partial_tails"`
	TrackedIDs          int       `json:"tracked_ids"`
	RefusedDurationMS   float64   `json:"refused_duration_ms"`
	TaskRefusals        int       `json:"task_refusals"`
	ControlRetries      int       `json:"control_retries"`
	Controls            int       `json:"controls"`
	InputGaps           int       `json:"input_gap_notices"`
	RecoverySamples     []float64 `json:"stall_recovery_ms"`
	SaveP95MS           float64   `json:"save_p95_ms"`
	SaveMaxMS           float64   `json:"save_max_ms"`
	Name                string    `json:"phase"`
	Produced            int       `json:"produced"`
	Admitted            int       `json:"admitted"`
	Refused             int       `json:"refused"`
	Late                int       `json:"late"`
	Canceled            int       `json:"intentional_cancellations"`
	Sent                int       `json:"sent"`
	Closures            int       `json:"closures"`
	RawP95MS            float64   `json:"raw_p95_ms"`
	StallDirectP95MS    float64   `json:"stall_direct_p95_ms"`
	RecoveryMS          float64   `json:"bridge_recovery_ms"`
	PacerDebtMS         float64   `json:"maximum_pacer_debt_ms"`
	PacerQueue          int       `json:"maximum_pacer_queue"`
	SourceQueue         int       `json:"maximum_source_queue"`
	QueueSlope          float64   `json:"source_queue_slope_packets_s"`
	CheckpointMaxAgeMS  float64   `json:"checkpoint_max_age_ms"`
	CheckpointWrites    uint64    `json:"checkpoint_writes"`
	CheckpointBytes     uint64    `json:"checkpoint_submitted_bytes"`
	SaveCalls           uint64    `json:"save_calls"`
	SaveWallMS          float64   `json:"save_wall_ms"`
	CPUSeconds          float64   `json:"process_cpu_seconds"`
	Qualified           bool      `json:"qualified"`
}

func bridgeRawP95(values []time.Duration) time.Duration {
	if len(values) == 0 {
		return 0
	}
	v := slices.Clone(values)
	slices.Sort(v)
	return v[int(math.Ceil(float64(len(v))*.95))-1]
}
func bridgeLatencyQualifies(r bridgeMeasuredResult) bool {
	latency := r.RawP95MS <= float64(bridgeLatencyTarget)/float64(time.Millisecond)
	if r.Burst {
		bounded := r.GapBeyondDebtMS <= float64(playbackRebaseAfter)/float64(time.Millisecond) && r.CapViolations == 0 && r.DuplicateWireIDs == 0 && r.MissingWireIDs == 0
		if r.SustainedBurst {
			latency = bounded && r.CreditWaitMS > 150 && r.ProducerBlockedMS > 0 && r.ExportCostLimit-r.ExportCostMax < 12000
		} else {
			latency = r.FirstWireP95MS <= float64(bridgeLatencyTarget)/float64(time.Millisecond) && r.GapBeyondDebtMS <= float64(playbackRebaseAfter)/float64(time.Millisecond) && r.ControlledMove && r.RealMove && r.MoveOverage > 0 && r.MoveProduced > 0 && r.FirstWireSamples >= 10 && r.CapViolations == 0 && r.DuplicateWireIDs == 0 && r.MissingWireIDs == 0
		}
	}
	return latency && r.SpuriousZeroCredit == 0 && r.Produced > 0 && r.Admitted+r.Refused == r.Produced && r.Refused == 0 && r.TaskRefusals == 0 && r.Late == 0 && r.Closures == 0 && r.RecoveryMS <= float64(bridgeRecoveryTarget)/float64(time.Millisecond) && r.Sent+r.Canceled == r.Admitted
}
func bridgeProcessCPU() float64 {
	var v syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &v) != nil {
		return 0
	}
	return float64(v.Utime.Sec+v.Stime.Sec) + float64(v.Utime.Usec+v.Stime.Usec)/1e6
}

func newMeasuredBridge(t *testing.T, longScope ...bool) (acceptedEventRig, BridgeOutputScope, *bridgeMeasuredStore, *atomic.Uint64) {
	t.Helper()
	rig, scope := newBridgeRig(t, false)
	s := rig.s
	if len(longScope) > 0 && longScope[0] {
		oldID := s.id
		s.id = strings.Repeat("s", 64)
		s.state.ID = s.id
		s.state.ICE.LocalUfrag = s.id
		delete(s.worker.sessions, oldID)
		s.worker.sessions[s.id] = s
		scope.SessionID = s.id
		scope.BotInstance = strings.Repeat("b", 64)
		scope.LineageID = strings.Repeat("l", 64)
		s.agent.v2.bridge.state = nil
		require.NoError(t, s.worker.BindBridgeOutput(scope))
		_, err := s.worker.BeginBridgeOutput(scope, 1)
		require.NoError(t, err)
	}
	addr := redistestenv.Address(t)
	if addr == "" {
		t.Fatal("sustained bridge measurement requires throwaway Redis")
	}
	redis, err := sessionstore.NewRedis(context.Background(), storage.RedisConfig{Addr: addr, Prefix: "bridge-measure:" + rand.Text() + ":"}, make([]byte, 32))
	require.NoError(t, err)
	lease, err := redis.Claim(context.Background(), s.id, s.worker.localAddr, time.Hour)
	require.NoError(t, err)
	counted := &bridgeMeasuredStore{Store: redis, component: redis}
	s.worker.cfg.Relay.Owners = counted
	s.lease = lease
	s.storeClockAt = time.Time{}
	s.worker.cfg.SnapshotInterval = 100 * time.Millisecond
	s.worker.cfg.Agent.CallbackTimeout = 5 * time.Second
	// Use the complete bridge working allowance after reserved host metadata,
	// and a full stable history. Periodic transport changes exercise hot writes.
	rig.app.state.Bytes = make([]byte, s.agent.v2.registration.Bounds.MaxStateBytes)
	rig.app.state.History = make([]byte, 8192)
	handler := func(ctx context.Context, _ *agent.TaskRun) error { <-ctx.Done(); return ctx.Err() }
	s.agent.v2.registration.Tasks = map[string]agent.TaskHandler{"bridge.state/v1": handler, "bridge.audio/v1": handler}
	// Observer ownership lives in checkpointed application state, so rollback
	// restores both the turn and the task IDs used by the next rotation.
	binary.BigEndian.PutUint64(rig.app.state.Bytes[24:32], 1)
	observerSlots := [][2]int{{32, 75}, {75, 118}}
	startObservers := func(e agent.Event) agent.Effects {
		turn := agent.Turn(binary.BigEndian.Uint64(rig.app.state.Bytes[24:32]))
		effects := agent.Effects{StartTasks: []agent.TaskSpec{{Step: "s", Kind: "bridge.state/v1", Turn: turn, Class: agent.ReadOnly}, {Step: "a", Kind: "bridge.audio/v1", Turn: turn, Class: agent.ReadOnly}}}
		for i, task := range effects.StartTasks {
			slot := observerSlots[i]
			copy(rig.app.state.Bytes[slot[0]:slot[1]], deriveTaskID(s.id, "bridge", e.ID, task.Step))
		}
		return effects
	}
	rig.app.handle = func(_ context.Context, e agent.Event) (agent.Effects, error) {
		switch e.Kind {
		case agent.Started:
			effects := startObservers(e)
			effects.SetTimers = []agent.TimerSpec{{ID: "checkpoint", After: 100 * time.Millisecond}}
			return effects, nil
		case agent.Control:
			if e.Control.Name == "input_gap" {
				value := binary.BigEndian.Uint64(rig.app.state.Bytes[16:24])
				binary.BigEndian.PutUint64(rig.app.state.Bytes[16:24], value+1)
				return agent.Effects{}, nil
			}
			if len(e.Control.Data) == 8 {
				binary.BigEndian.PutUint64(rig.app.state.Bytes[24:32], binary.BigEndian.Uint64(e.Control.Data))
				effects := agent.Effects{SetTimers: []agent.TimerSpec{{ID: "observers", After: 150 * time.Millisecond}}}
				for _, slot := range observerSlots {
					id := rig.app.state.Bytes[slot[0]:slot[1]]
					if id[0] != 0 {
						effects.CancelTasks = append(effects.CancelTasks, agent.TaskID(id))
					}
					clear(id)
				}
				return effects, nil
			}
		case agent.TimerFired:
			if e.Timer.ID == "observers" {
				return startObservers(e), nil
			}
			if e.Timer.ID == "checkpoint" {
				binary.BigEndian.PutUint64(rig.app.state.Bytes, uint64(e.At.UnixNano()))
				return agent.Effects{SetTimers: []agent.TimerSpec{{ID: "checkpoint", After: 100 * time.Millisecond}}}, nil
			}
		}
		return agent.Effects{}, nil
	}
	// Drain the fake wire continuously. Its bounded test channel must not act as
	// hidden host backpressure or censor the source's production timestamps.
	sent := &atomic.Uint64{}
	measuredAgent := &bridgeMeasuredAgent{Agent: s.agent.v2.instance, ready: make(map[uint64]time.Time), turns: make(map[uint64]agent.Turn)}
	s.worker.cfg.PacketTrace = func(p PacketTrace) {
		if p.Protocol == "SRTP" {
			sent.Add(1)
			q := s.agent.v2.playback.bridgeQueue
			if len(q) > 0 {
				id := binary.BigEndian.Uint64(s.plainBuf[13:21])
				measuredAgent.wireMu.Lock()
				measuredAgent.wire = append(measuredAgent.wire, bridgeWireSample{id: id, turn: q[0].Turn, at: p.SentAt, timestamp: p.Timestamp, backlog: len(q) > 1, lateness: max(0, p.SentAt.Sub(s.agent.v2.playback.nextAt))})
				measuredAgent.wireMu.Unlock()
			}
		}
	}
	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		for {
			select {
			case <-s.ctx.Done():
				return
			case <-rig.wire.packets:
			}
		}
	}()
	s.agent.v2.instance = measuredAgent
	close(s.agent.v2.ready)
	actorDone := make(chan struct{})
	go func() { defer close(actorDone); s.v2AgentLoop() }()
	// The existing periodic writer and lease renewal remain real Redis I/O.
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		renew := time.NewTicker(time.Second)
		defer renew.Stop()
		for {
			select {
			case <-s.ctx.Done():
				return
			case <-tick.C:
				if err := s.checkpoint.persist(s.ctx, checkpointOptional); err != nil && !errors.Is(err, errCheckpointSkipped) && !errors.Is(err, context.Canceled) {
					if counted.stalling.Load() {
						continue
					}
					t.Errorf("checkpoint: %v", err)
					return
				}
				s.mu.Lock()
				through := s.agent.v2.bridge.state.Sent
				s.mu.Unlock()
				// This fixture producer consumes every settled receipt and
				// never resubmits an ID below this explicit horizon.
				if err := advanceBridgeMeasurementHorizon(s, scope, through); err != nil {
					t.Errorf("producer horizon: %v", err)
					return
				}
			case <-renew.C:
				now := time.Now()
				counted.renewMu.Lock()
				if !counted.renewAt.IsZero() {
					counted.renewalGaps = append(counted.renewalGaps, now.Sub(counted.renewAt))
				}
				counted.renewAt = now
				counted.renewMu.Unlock()
				if _, err := counted.Renew(s.ctx, lease, time.Hour); err != nil && !errors.Is(err, context.Canceled) {
					if counted.stalling.Load() {
						continue
					}
					t.Errorf("renew: %v", err)
					return
				}
			}
		}
	}()
	t.Cleanup(func() {
		s.cancel()
		<-actorDone
		<-writerDone
		<-drainDone
		s.worker.running.Wait()
		require.NoError(t, redis.Release(context.Background(), lease))
		require.NoError(t, redis.Close())
	})
	require.Eventually(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.state.Agent.V2.Started }, 5*time.Second, time.Millisecond)
	return rig, scope, counted, sent
}

func measureBridgePhase(t *testing.T, rig acceptedEventRig, scope BridgeOutputScope, store *bridgeMeasuredStore, sent *atomic.Uint64, p bridgeMeasuredPhase) bridgeMeasuredResult {
	t.Helper()
	s := rig.s
	l := s.agent.v2.bridge.limits
	result := bridgeMeasuredResult{SustainedBurst: p.sustainedBurst, ControlledMove: p.controlledMove, Burst: p.burst, Name: p.name, Produced: int(p.duration.Seconds() * float64(p.rate))}
	s.mu.Lock()
	baseSeq := s.agent.v2.bridge.state.Through
	turn := s.agent.v2.bridge.state.LastTurn + 1
	if baseSeq == 0 && len(s.agent.v2.bridge.state.Turns) == 1 {
		turn = s.agent.v2.bridge.state.Turns[0].Turn
	}
	s.mu.Unlock()
	_, err := s.worker.BeginBridgeOutput(scope, turn)
	require.NoError(t, err)
	baselineSent, baselineWrites, baselineBytes := sent.Load(), store.writes.Load(), store.bytes.Load()
	saveCalls, saveWall := s.worker.metrics.saveCalls.Load(), s.worker.metrics.saveWallNS.Load()
	measured := s.agent.v2.instance.(*bridgeMeasuredAgent)
	measured.mu.Lock()
	saveIndex := len(measured.saves)
	refusalStart := measured.refusals.Load()
	measured.mu.Unlock()
	cpu := bridgeProcessCPU()
	var timingMu sync.Mutex
	latencies := make([]time.Duration, 0, result.Produced)
	identities := make(map[uint64]string, result.Produced)
	record := func(seq uint64, ready time.Time, outcome string) {
		timingMu.Lock()
		defer timingMu.Unlock()
		if _, exists := identities[seq]; exists {
			t.Errorf("duplicate measurement identity %d", seq)
		}
		identities[seq] = outcome
		latencies = append(latencies, time.Since(ready))
	}
	source := make(chan bridgeMeasuredPacket, l.PendingPackets)
	var producerBlocked atomic.Int64
	var dropped atomic.Int64
	sourceDone := make(chan struct{})
	var producedDone atomic.Bool
	start := time.Now()
	go func() {
		defer close(sourceDone)
		defer producedDone.Store(true)
		defer close(source)
		for i := 0; i < result.Produced; i++ {
			at := start.Add(time.Duration(i) * time.Second / time.Duration(p.rate))
			if p.jitter && i%11 == 0 {
				at = at.Add(7 * time.Millisecond)
			}
			if wait := time.Until(at); wait > 0 {
				time.Sleep(wait)
			}
			payload := bridgeMeasuredPayload(p.size, p.tails && ((p.rapid && (i+1)%13 == 0) || (!p.rapid && (i+1)%137 == 0) || i == result.Produced-1))
			binary.BigEndian.PutUint64(payload[1:9], baseSeq+uint64(i)+1)
			// Encoded-ready is recorded before IPC enqueue and before credit wait.
			packet := bridgeMeasuredPacket{seq: baseSeq + uint64(i) + 1, turn: turn, payload: payload, ready: time.Now()}
			if p.rapid {
				packet.turn += agent.Turn(i / 13)
			} else if p.tails {
				packet.turn += agent.Turn(i / 137)
			}
			measured.wireMu.Lock()
			measured.ready[packet.seq] = packet.ready
			measured.turns[packet.seq] = packet.turn
			measured.wireMu.Unlock()
			if p.burst {
				blocked := time.Now()
				select {
				case source <- packet:
				case <-s.ctx.Done():
					return
				}
				producerBlocked.Add(int64(time.Since(blocked)))
				continue
			}
			select {
			case source <- packet:
			default:
				dropped.Add(1)
				record(packet.seq, packet.ready, "source_backpressure")
			}
		}
	}()
	var observeMu sync.Mutex
	var release time.Time
	var recovery time.Duration
	var recoveries []time.Duration
	var sampleN, sampleX, sampleY, sampleXX, sampleXY float64
	stable := time.Time{}
	observerStop, observerDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(observerDone)
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-observerStop:
				return
			case <-tick.C:
				now := time.Now()
				depth := len(source)
				observeMu.Lock()
				result.SourceQueue = max(result.SourceQueue, depth)
				if !producedDone.Load() {
					x, y := now.Sub(start).Seconds(), float64(depth)
					sampleN++
					sampleX += x
					sampleY += y
					sampleXX += x * x
					sampleXY += x * y
				}
				if !release.IsZero() && recovery == 0 {
					if depth <= 1 {
						if stable.IsZero() {
							stable = now
						}
						if now.Sub(stable) >= 100*time.Millisecond {
							recovery = now.Sub(release)
						}
					} else {
						stable = time.Time{}
					}
				}
				s.mu.Lock()
				q := s.agent.v2.playback
				cost := s.bridgeExportCostLocked()
				result.ExportCostMax = max(result.ExportCostMax, cost)
				result.ExportCostLimit = s.bridgeLookaheadBudget()
				result.MemoryPeak = max(result.MemoryPeak, s.agent.v2.bridge.memory.Load())
				if cost > result.ExportCostLimit {
					result.CapViolations++
				}
				result.PacerQueue = max(result.PacerQueue, len(q.bridgeQueue))
				debt := q.duration
				if q.nextAt.After(now) {
					debt += q.nextAt.Sub(now)
				}
				result.PacerDebtMS = max(result.PacerDebtMS, float64(debt)/float64(time.Millisecond))
				if !s.checkpoint.lastCheckpoint.IsZero() {
					result.CheckpointMaxAgeMS = max(result.CheckpointMaxAgeMS, float64(now.Sub(s.checkpoint.lastCheckpoint))/float64(time.Millisecond))
				}
				s.mu.Unlock()
				observeMu.Unlock()
			}
		}
	}()
	direct := make([]time.Duration, 0)
	controlQueue := make(chan agent.ControlMessage, 32)
	controlDone := make(chan struct{})
	var controls, gaps, retries atomic.Int64
	go func() {
		defer close(controlDone)
		s.mu.Lock()
		sequence := s.state.Agent.V2.ControlHigh
		s.mu.Unlock()
		for message := range controlQueue {
			sequence++
			message.Sequence = sequence
			deadline := time.Now().Add(3 * time.Second)
			for {
				_, e := s.worker.AgentControl(context.Background(), s.id, message)
				if e == nil {
					controls.Add(1)
					if message.Name == "input_gap" {
						gaps.Add(1)
					}
					break
				}
				if !errors.Is(e, agent.ErrBackpressure) || time.Now().After(deadline) {
					t.Errorf("control did not settle: %v", e)
					return
				}
				retries.Add(1)
				time.Sleep(10 * time.Millisecond)
			}
		}
	}()
	stopControl := sync.OnceFunc(func() { close(controlQueue); <-controlDone })
	t.Cleanup(stopControl)
	sendControl := func(message agent.ControlMessage) {
		select {
		case controlQueue <- message:
		default:
			t.Fatal("bounded control queue exhausted")
		}
	}
	var credits bridgeCreditSamples
	current := turn
	for packet := range source {
		if d, e := opusDuration(packet.payload); e == nil && d < bridgePacketDuration {
			result.PartialTails++
		}
		if packet.seq != baseSeq+uint64(result.Admitted+result.Refused)+1 {
			result.Refused++
			record(packet.seq, packet.ready, "unadmitted_after_source_overflow")
			continue
		}
		if p.upstream && (packet.seq-baseSeq == 51 || packet.seq-baseSeq == 101) {
			time.Sleep(150 * time.Millisecond)
			observeMu.Lock()
			if !release.IsZero() {
				recoveries = append(recoveries, recovery)
			}
			release = time.Now()
			stable = time.Time{}
			recovery = 0
			observeMu.Unlock()
		}
		if packet.turn != current {
			require.NoError(t, s.worker.EndBridgeOutput(scope, current, packet.seq-1))
			if p.rapid {
				interruption, e := s.worker.InterruptBridgeOutput(context.Background(), scope, fmt.Sprint("interrupt-", packet.turn), packet.turn, true)
				require.NoError(t, e)
				result.Canceled += len(interruption.Canceled)
			}
			_, err = s.worker.BeginBridgeOutput(scope, packet.turn)
			require.NoError(t, err)
			data := make([]byte, 8)
			binary.BigEndian.PutUint64(data, uint64(packet.turn))
			sendControl(agent.ControlMessage{ID: fmt.Sprint("turn-", packet.turn), Name: "turn", Data: data})
			current = packet.turn
		}

		if (packet.seq-baseSeq)%50 == 0 {
			data := make([]byte, 16)
			binary.BigEndian.PutUint64(data, packet.seq-1)
			binary.BigEndian.PutUint64(data[8:], packet.seq)
			sendControl(agent.ControlMessage{ID: fmt.Sprint("gap-", packet.seq), Name: "input_gap", Data: data})
		}
		var creditWait time.Time
		for {
			var credit BridgeOutputCredit
			var e error
			if !p.burst && !p.overload {
				credit, e = credits.read(s, scope)
			} else {
				credit, e = s.worker.BridgeOutputCredit(scope)
			}
			require.NoError(t, e)
			if credit.PacketSlots > 0 && credit.PayloadBytes >= len(packet.payload) {
				break
			}
			if time.Since(packet.ready) > 10*time.Second {
				t.Fatal("bounded credit wait exceeded control drain budget")
			}
			if creditWait.IsZero() {
				creditWait = time.Now()
			}
			time.Sleep(time.Millisecond)
		}
		if !creditWait.IsZero() {
			result.CreditWaitMS += float64(time.Since(creditWait)) / float64(time.Millisecond)
		}
		if p.upstream && time.Since(packet.ready) > 20*time.Millisecond {
			direct = append(direct, time.Since(packet.ready))
		}
		r, e := s.worker.SubmitBridgeOutput(context.Background(), scope, packet.turn, packet.seq, packet.payload)
		require.NoError(t, e)
		record(packet.seq, packet.ready, r.Outcome+":"+r.Reason)
		if r.Outcome == "admitted" {
			result.Admitted++
		} else {
			result.Refused++
			if r.Reason == "output_late" {
				result.Late++
			}
		}
	}
	<-sourceDone
	stopControl()
	result.Controls = int(controls.Load())
	result.InputGaps = int(gaps.Load())
	result.ControlRetries = int(retries.Load())
	result.TaskRefusals = int(measured.refusals.Load() - refusalStart)
	result.Refused += int(dropped.Load())
	timingMu.Lock()
	result.TrackedIDs = len(identities)
	timingMu.Unlock()
	require.Equal(t, result.Produced, result.TrackedIDs, "every produced identity has an explicit terminal observation")
	result.RefusedDurationMS = float64(result.Refused) * float64(bridgePacketDuration) / float64(time.Millisecond)
	s.mu.Lock()
	through := s.agent.v2.bridge.state.Through
	s.mu.Unlock()
	require.NoError(t, s.worker.EndBridgeOutput(scope, current, through))
	require.Eventually(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return len(s.agent.v2.playback.bridgeQueue) == 0 }, 15*time.Second, 5*time.Millisecond, "all admitted output must drain")
	if p.upstream {
		time.Sleep(110 * time.Millisecond)
	}
	close(observerStop)
	<-observerDone
	result.Sent = int(sent.Load() - baselineSent)
	result.Closures = 0
	if s.ctx.Err() != nil || s.agent.failed.Load() {
		result.Closures = 1
	}
	result.RawP95MS = float64(bridgeRawP95(latencies)) / float64(time.Millisecond)
	result.StallDirectP95MS = float64(bridgeRawP95(direct)) / float64(time.Millisecond)
	observeMu.Lock()
	if !release.IsZero() {
		recoveries = append(recoveries, recovery)
	}
	for _, d := range recoveries {
		if d == 0 {
			result.RecoveryMS = float64(bridgeRecoveryTarget+time.Second) / float64(time.Millisecond)
		} else {
			result.RecoveryMS = max(result.RecoveryMS, float64(d)/float64(time.Millisecond))
		}
		result.RecoverySamples = append(result.RecoverySamples, float64(d)/float64(time.Millisecond))
	}
	if denominator := sampleN*sampleXX - sampleX*sampleX; denominator > 0 {
		result.QueueSlope = (sampleN*sampleXY - sampleX*sampleY) / denominator
	}
	observeMu.Unlock()
	result.CheckpointWrites = store.writes.Load() - baselineWrites
	result.CheckpointBytes = store.bytes.Load() - baselineBytes
	result.SaveCalls = s.worker.metrics.saveCalls.Load() - saveCalls
	result.SaveWallMS = float64(s.worker.metrics.saveWallNS.Load()-saveWall) / float64(time.Millisecond)
	result.CPUSeconds = bridgeProcessCPU() - cpu
	measured.mu.Lock()
	saveSamples := slices.Clone(measured.saves[saveIndex:])
	measured.mu.Unlock()
	result.SaveP95MS = float64(bridgeRawP95(saveSamples)) / float64(time.Millisecond)
	if len(saveSamples) > 0 {
		result.SaveMaxMS = float64(slices.Max(saveSamples)) / float64(time.Millisecond)
	}

	result.ProducerBlockedMS = float64(producerBlocked.Load()) / float64(time.Millisecond)
	measured.wireMu.Lock()
	first := map[agent.Turn]uint64{}
	for id, turn := range measured.turns {
		if id > baseSeq && id <= baseSeq+uint64(result.Produced) && (first[turn] == 0 || id < first[turn]) {
			first[turn] = id
		}
	}
	seen := map[uint64]bool{}
	var firstLatency []time.Duration
	var previous *bridgeWireSample
	for _, sample := range measured.wire {
		if sample.id <= baseSeq || sample.id > baseSeq+uint64(result.Produced) {
			continue
		}
		if seen[sample.id] {
			result.DuplicateWireIDs++
		}
		seen[sample.id] = true
		result.WireSchedulerDebtMS = max(result.WireSchedulerDebtMS, float64(sample.lateness)/float64(time.Millisecond))
		if first[sample.turn] == sample.id {
			firstLatency = append(firstLatency, sample.at.Sub(measured.ready[sample.id]))
		}
		if previous != nil && previous.backlog {
			gap := sample.at.Sub(previous.at)
			media := time.Duration(uint32(sample.timestamp-previous.timestamp)) * time.Second / 48000
			result.WireGapMS = max(result.WireGapMS, float64(gap)/float64(time.Millisecond))
			result.GapBeyondDebtMS = max(result.GapBeyondDebtMS, float64(gap-media-sample.lateness+previous.lateness)/float64(time.Millisecond))
		}
		owned := sample
		previous = &owned
	}
	result.FirstWireSamples = len(firstLatency)
	result.FirstWireP95MS = float64(bridgeRawP95(firstLatency)) / float64(time.Millisecond)
	if p.burst {
		result.MissingWireIDs = result.Produced - len(seen)
	}
	measured.wireMu.Unlock()
	result.CreditPolls, result.SpuriousZeroCredit = credits.Polls, credits.SpuriousZero
	result.Qualified = bridgeLatencyQualifies(result)
	encoded, e := json.Marshal(result)
	require.NoError(t, e)
	t.Log(string(encoded))
	return result
}

func TestBridgeOutputSustainedMeasurement(t *testing.T) {
	if testing.Short() {
		t.Skip("sustained output qualification exceeds 60 s")
	}
	rig, scope, store, sent := newMeasuredBridge(t)
	phases := []bridgeMeasuredPhase{
		{name: "continuous-mixed", duration: 61 * time.Second, rate: 50, size: 160, jitter: true, tails: true, rapid: true},
		{name: "python-ipc-stalls", duration: 3 * time.Second, rate: 50, size: 160, upstream: true},
		{name: "160-byte-burst", duration: bridgeBurstDuration, rate: bridgeBurstRate, size: 160, burst: true},
		{name: "160-byte-sustained-burst", duration: bridgeBurstDuration, rate: bridgeBurstRate, size: 160, burst: true, sustainedBurst: true},
		{name: "maximum-size-overload", duration: 2 * time.Second, rate: 200, size: bridgeBinaryBytes, overload: true},
	}
	for _, phase := range phases {
		t.Run(phase.name, func(t *testing.T) {
			if phase.burst && !phase.sustainedBurst {
				phase.controlledMove = t.Run("planned-move", TestBridgeOutputBurstMovesAtExactCut)
			}
			var r bridgeMeasuredResult
			if phase.burst && !phase.sustainedBurst {
				r = measureBridgeBurst(t, phase.controlledMove)
			} else {
				r = measureBridgePhase(t, rig, scope, store, sent, phase)
			}
			if phase.name == "continuous-mixed" {
				require.GreaterOrEqual(t, phase.duration, 60*time.Second)
				require.GreaterOrEqual(t, r.Produced, 3000)
				require.Greater(t, r.PartialTails, 100)
			}
			if phase.overload {
				require.False(t, r.Qualified, "overload must fail latency qualification")
				require.Zero(t, r.Closures)
			} else {
				require.True(t, r.Qualified, "raw per-phase latency qualification")
				if phase.duration >= 20*time.Second {
					require.GreaterOrEqual(t, r.SaveCalls, uint64(phase.duration.Seconds()*5), "sustain checkpoint callbacks across observer rotations")
				}
				if phase.tails {
					require.Greater(t, r.PartialTails, 0)
				}
				require.Greater(t, r.InputGaps, 0, "include accepted gap/control state in checkpoints")
				require.Less(t, r.CheckpointWrites, uint64(r.Produced/2), "no per-packet store writes")
			}
		})
	}
	store.renewMu.Lock()
	t.Logf("loaded renewal request gap p95=%s max=%s", bridgeRawP95(store.renewalGaps), func() time.Duration {
		if len(store.renewalGaps) == 0 {
			return 0
		}
		return slices.Max(store.renewalGaps)
	}())
	store.renewMu.Unlock()
}
func TestBridgeOutputHostStallFailsLatencyQualification(t *testing.T) {
	if testing.Short() {
		t.Skip("host admission negative control")
	}
	rig, scope := newBridgeRig(t, false)
	s := rig.s
	var actorEvents atomic.Uint64
	rig.app.handle = func(context.Context, agent.Event) (agent.Effects, error) {
		actorEvents.Add(1)
		return agent.Effects{SetTimers: []agent.TimerSpec{{ID: "heartbeat", After: 10 * time.Millisecond}}}, nil
	}
	actorDone := make(chan struct{})
	go func() { defer close(actorDone); s.v2AgentEvents() }()
	t.Cleanup(func() { s.cancel(); <-actorDone })
	require.Eventually(t, func() bool { return actorEvents.Load() > 0 }, time.Second, time.Millisecond)
	type stalledReply struct {
		receipt BridgeOutputReceipt
		latency time.Duration
	}
	replies := make(chan stalledReply, 10)
	for i := 1; i <= 10; i++ {
		seq := uint64(i)
		go func() {
			payload := bridgePacket(160)
			ready := time.Now()
			r, e := s.worker.SubmitBridgeOutput(context.Background(), scope, 1, seq, payload)
			if e != nil {
				panic(e)
			}
			replies <- stalledReply{r, time.Since(ready)}
		}()
		require.Eventually(t, func() bool { return len(s.agent.v2.bridge.ingress) == i }, time.Second, time.Millisecond)
	}
	// The ingress consumer is held before its commit lock; the actor is free.
	time.Sleep(160 * time.Millisecond)
	startBridgeLoop(t, s)
	r := bridgeMeasuredResult{Name: "host-admission-stall", Produced: 10}
	latencies := make([]time.Duration, 0, 10)
	seen := map[uint64]bool{}
	for range 10 {
		reply := <-replies
		receipt := reply.receipt
		latencies = append(latencies, reply.latency)
		require.False(t, seen[receipt.Sequence])
		seen[receipt.Sequence] = true
		if receipt.Reason == "output_late" {
			r.Late++
		}
		r.Refused++
	}
	r.RawP95MS = float64(bridgeRawP95(latencies)) / float64(time.Millisecond)
	r.TrackedIDs = len(seen)
	r.ActorEvents = actorEvents.Load()
	if s.agent.failed.Load() || s.ctx.Err() != nil {
		r.Closures++
	}
	r.RefusedDurationMS = float64(10*bridgePacketDuration) / float64(time.Millisecond)
	r.SaveCalls = s.worker.metrics.saveCalls.Load()
	r.Qualified = bridgeLatencyQualifies(r)
	require.False(t, r.Qualified)
	require.Equal(t, 10, r.Late)
	require.NoError(t, s.ctx.Err())
	require.False(t, s.agent.failed.Load())
	require.Greater(t, actorEvents.Load(), uint64(5), "actor must remain live throughout the ingress stall")
	require.Equal(t, "admitted", submitBridge(t, s, scope, 1, 11).Outcome)
	encoded, err := json.Marshal(r)
	require.NoError(t, err)
	t.Log(string(encoded))
}
func TestBridgeLatencyQualificationRejectsControls(t *testing.T) {
	good := bridgeMeasuredResult{Produced: 100, Admitted: 100, Sent: 100, RawP95MS: 100}
	require.True(t, bridgeLatencyQualifies(good))
	for _, bad := range []bridgeMeasuredResult{{Produced: 100, Admitted: 100, Sent: 100, RawP95MS: 100, SpuriousZeroCredit: 1}, {Produced: 100, Admitted: 99, Refused: 1, Sent: 99}, {Produced: 100, Admitted: 100, Sent: 100, RawP95MS: 151}, {Produced: 100, Admitted: 100, Sent: 99}, {Produced: 100, Admitted: 100, Sent: 100, RecoveryMS: 501}, {Produced: 100, Admitted: 100, Sent: 100, Late: 1}, {Produced: 100, Admitted: 100, Sent: 100, TaskRefusals: 1}, {Produced: 100, Admitted: 100, Sent: 100, Closures: 1}} {
		require.False(t, bridgeLatencyQualifies(bad))
	}
}

// A 10 ms Opus packet is a partial final 20 ms fixture frame, not a truncated
// or invalid packet. It exercises actual duration and end-of-turn accounting.
func bridgeMeasuredPayload(size int, tail bool) []byte {
	payload := bridgePacket(size)
	if tail {
		payload[0] = 0xf0
	}
	return payload
}
func TestBridgeOutputMeasurementPartialTails(t *testing.T) {
	full, e := opusDuration(bridgeMeasuredPayload(160, false))
	require.NoError(t, e)
	require.Equal(t, 20*time.Millisecond, full)
	tail, e := opusDuration(bridgeMeasuredPayload(160, true))
	require.NoError(t, e)
	require.Equal(t, 10*time.Millisecond, tail)
}

func advanceBridgeMeasurementHorizon(s *session, scope BridgeOutputScope, through uint64) error {
	err := s.worker.AdvanceBridgeReplayHorizon(scope, through)
	if s.ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}
func TestBridgeOutputCheckpointWriterShutdown(t *testing.T) {
	rig, scope := newBridgeRig(t, false)
	rig.s.cancel()
	require.NoError(t, advanceBridgeMeasurementHorizon(rig.s, scope, 0), "a pending writer tick must allow normal teardown")
}

func TestBridgeBurstQualificationUsesFirstWireAndDebt(t *testing.T) {
	good := bridgeMeasuredResult{ControlledMove: true, RealMove: true, MoveOverage: 1200, MoveProduced: 4, FirstWireSamples: 10, Burst: true, Produced: 400, Admitted: 400, Sent: 400, RawP95MS: 2000, FirstWireP95MS: 10, CreditWaitMS: 5000}
	require.True(t, bridgeLatencyQualifies(good), "later burst credit wait is reported rather than graded")
	for name, change := range map[string]func(*bridgeMeasuredResult){
		"first-delay":         func(r *bridgeMeasuredResult) { r.FirstWireP95MS = 151 },
		"sample-count":        func(r *bridgeMeasuredResult) { r.FirstWireSamples = 1 },
		"production-resume":   func(r *bridgeMeasuredResult) { r.RealMove = false },
		"drain-target":        func(r *bridgeMeasuredResult) { r.MoveOverage = 0 },
		"concurrent-producer": func(r *bridgeMeasuredResult) { r.MoveProduced = 0 },
		"controlled-cut":      func(r *bridgeMeasuredResult) { r.ControlledMove = false },
		"cap":                 func(r *bridgeMeasuredResult) { r.CapViolations = 1 },
		"duplicate":           func(r *bridgeMeasuredResult) { r.DuplicateWireIDs = 1 },
		"missing":             func(r *bridgeMeasuredResult) { r.MissingWireIDs = 1 },
		"pacer-gap":           func(r *bridgeMeasuredResult) { r.GapBeyondDebtMS = 21 },
		"refused":             func(r *bridgeMeasuredResult) { r.Refused = 1; r.Admitted--; r.Sent-- },
		"late":                func(r *bridgeMeasuredResult) { r.Late = 1 },
	} {
		t.Run(name, func(t *testing.T) { bad := good; change(&bad); require.False(t, bridgeLatencyQualifies(bad)) })
	}

	good.Burst = false
	require.False(t, bridgeLatencyQualifies(good), "real-time ready-before-credit gate stays unchanged")
}

func TestBridgeBurstQualificationRequiresSustainedBacklog(t *testing.T) {
	good := bridgeMeasuredResult{Burst: true, SustainedBurst: true, Produced: 400, Admitted: 400, Sent: 400, CreditWaitMS: 4000, ProducerBlockedMS: 2000, ExportCostMax: 670000, ExportCostLimit: 675021}
	require.True(t, bridgeLatencyQualifies(good))
	for name, change := range map[string]func(*bridgeMeasuredResult){
		"credit-blocked": func(r *bridgeMeasuredResult) { r.CreditWaitMS = 0 },
		"near-cap":       func(r *bridgeMeasuredResult) { r.ExportCostMax = 600000 },
		"bounded-source": func(r *bridgeMeasuredResult) { r.ProducerBlockedMS = 0 },
		"starvation":     func(r *bridgeMeasuredResult) { r.GapBeyondDebtMS = 21 },
		"full-drain":     func(r *bridgeMeasuredResult) { r.Sent-- },
		"refused":        func(r *bridgeMeasuredResult) { r.Refused = 1 },
		"late":           func(r *bridgeMeasuredResult) { r.Late = 1 },
		"cap":            func(r *bridgeMeasuredResult) { r.CapViolations = 1 },
	} {
		t.Run(name, func(t *testing.T) { bad := good; change(&bad); require.False(t, bridgeLatencyQualifies(bad)) })
	}
}
