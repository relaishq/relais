package mediaworker

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/netip"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/srtp/v3"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/relaishq/relais/internal/agenthost"
	"github.com/relaishq/relais/internal/redistestenv"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/metrics"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/relaishq/relais/pkg/storage"
	"github.com/stretchr/testify/require"
)

// TestBridgeTaskPairAdmission checks the proposed ADR 0014 task shape against
// production metadata and split-checkpoint admission, before timing throughput.
func TestBridgeTaskPairAdmission(t *testing.T) {
	runs := make(chan *agent.TaskRun, 2)
	handler := func(ctx context.Context, run *agent.TaskRun) error {
		runs <- run
		<-ctx.Done()
		return ctx.Err()
	}
	rig := newAcceptedEventRig(t, agenthost.Bounds{}, nil)
	s := rig.s
	s.componentMode = true
	s.storeClockAt = time.Time{}
	s.state.Agent = agentState{}
	rig.app.state = agent.State{Version: 1, Bytes: make([]byte, 3072), History: make([]byte, 8192)}
	s.worker.cfg.Agent.Factory = agenthost.Factory(agenthost.Registration{
		Name: "bridge", Factory: func(agent.FactoryArgs) agent.Agent { return rig.app },
		Tasks: map[string]agent.TaskHandler{"bridge.state/v1": handler, "bridge.audio/v1": handler},
	})
	require.NoError(t, s.initAgent(false, ResumeOptions{}))
	rig.app.handle = func(_ context.Context, e agent.Event) (agent.Effects, error) {
		if e.Kind == agent.Started {
			return agent.Effects{StartTasks: []agent.TaskSpec{
				{Step: "s", Kind: "bridge.state/v1", Turn: 1, Class: agent.ReadOnly},
				{Step: "a", Kind: "bridge.audio/v1", Turn: 1, Class: agent.ReadOnly},
			}}, nil
		}
		if e.Kind == agent.TaskMessage && e.Result.Chunk.Audio != nil {
			var effects agent.Effects
			for i := range e.Result.Chunk.Audio.Packets {
				effects.Audio = append(effects.Audio, agent.Audio{Turn: 1, Source: agent.AudioRef{TaskID: e.Result.TaskID, Sequence: e.Result.Sequence, PacketOffset: uint32(i)}})
			}
			return effects, nil
		}
		return agent.Effects{}, nil
	}
	_, err := (acceptedEvents{s}).submit(agent.Event{Kind: agent.Started, ID: "start"}, eventLive)
	require.NoError(t, err)
	metadata, err := json.Marshal(s.agent.latest.V2)
	require.NoError(t, err)
	t.Logf("pair tasks=%d refusals=%d metadata=%d cap=%d", len(s.agent.v2.tasks), len(s.agent.latest.V2.Capacity), len(metadata), v2HostMetadataBytes)
	require.Len(t, s.agent.v2.tasks, 2)
	require.Empty(t, s.agent.latest.V2.Capacity)
	require.NoError(t, s.checkpoint.persist(context.Background(), checkpointRequired))
	s.launchTasks()
	t.Cleanup(s.stopTaskAttempts)
	var audio *agent.TaskRun
	for range 2 {
		run := <-runs
		if run.Spec().Kind == "bridge.audio/v1" {
			audio = run
		}
	}
	require.NotNil(t, audio)
	done := make(chan error, 1)
	go func() {
		done <- audio.Emit(s.ctx, 1, agent.Chunk{Audio: &agent.EncodedAudio{Format: "audio/opus", Packets: [][]byte{{0xf8, 0xff, 0xfe}, {0xf8, 0xff, 0xfe}, {0xf8, 0xff, 0xfe}, {0xf8, 0xff, 0xfe}, {0xf8, 0xff, 0xfe}}}})
	}()
	s.acceptEmission(<-s.agent.v2.emissions)
	require.NoError(t, <-done)
	delivered, err := s.deliverTaskResult()
	require.NoError(t, err)
	require.True(t, delivered)
	require.Len(t, s.agent.v2.playback.queue, 5)
	metadata, err = json.Marshal(s.agent.latest.V2)
	require.NoError(t, err)
	t.Logf("audio admitted metadata=%d cap=%d", len(metadata), v2HostMetadataBytes)
}

// bridgePacketTiming owns observations outside accepted agent state. Polling the
// real pacer queue (and the encrypted send for a packet sent before a poll) gives
// an upper bound on admission latency, not a timestamp inside the transaction.
type bridgePacketTiming struct {
	mu             sync.Mutex
	ready          map[uint32]time.Time
	admitted       map[uint32]time.Time
	turns          map[uint32]agent.Turn
	cancelled      map[agent.Turn]time.Time
	maxPacer       int
	before         map[uint32]time.Time
	lower          []time.Duration
	delays         []time.Duration
	gaps           []time.Duration
	lastWire       time.Time
	produced       int
	wirePackets    int
	lastRTPTime    uint32
	rtpSeen        bool
	pacerRebases   int
	pacerSkipTicks uint64
	maxPending     int
	pendingFirst   int
	pendingLast    int
}

func (m *bridgePacketTiming) observe(p []byte, now time.Time, wire bool) {
	if len(p) < 5 {
		return
	}
	id := binary.BigEndian.Uint32(p[1:5])
	m.mu.Lock()
	defer m.mu.Unlock()
	if ready, ok := m.ready[id]; ok {
		if _, seen := m.admitted[id]; !seen {
			m.admitted[id] = now
			m.delays = append(m.delays, now.Sub(ready))
		}
	}
	if wire {
		m.wirePackets++
		if !m.lastWire.IsZero() {
			m.gaps = append(m.gaps, now.Sub(m.lastWire))
		}
		m.lastWire = now
	}
}

func (a *bridgeThroughputAgent) failureDetails(wire *bridgeWireProbe) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var last time.Duration
	if len(a.saveCost.wall) > 0 {
		last = a.saveCost.wall[len(a.saveCost.wall)-1]
	}
	b, _ := json.Marshal(map[string]any{"last_event": a.lastEvent, "fake_wire": wire.summary(), "last_save_body_ms": float64(last) / float64(time.Millisecond), "save_body_p99_ms": bridgeQuantile(a.saveCost.wall, .99), "handle_body_p99_ms": bridgeQuantile(a.handleCost.wall, .99)})
	return string(b)
}

func (m *bridgePacketTiming) beforeAdmission(p []byte, now time.Time) {
	if len(p) < 5 {
		return
	}
	id := binary.BigEndian.Uint32(p[1:5])
	m.mu.Lock()
	defer m.mu.Unlock()
	if ready, ok := m.ready[id]; ok {
		if m.before == nil {
			m.before = map[uint32]time.Time{}
		}
		if _, seen := m.before[id]; !seen {
			m.before[id] = now
			m.lower = append(m.lower, now.Sub(ready))
		}
	}
}

type bridgeCallbackCost struct {
	wall  []time.Duration
	cpu   uint64
	known int
}

func (c *bridgeCallbackCost) begin() func() {
	finish := beginSaveCPU()
	at := time.Now()
	return func() {
		c.wall = append(c.wall, time.Since(at))
		n, ok := finish()
		if ok {
			c.cpu += n
			c.known++
		}
	}
}

type bridgeThroughputAgent struct {
	mu                   sync.Mutex // callbacks may settle after actor cancellation
	sessionID            string
	expected             map[agent.TaskID]string
	refusalKinds         map[string]int
	observedCheckpoint   atomic.Uint64
	state                agent.State
	timing               *bridgePacketTiming
	turn                 agent.Turn
	handleCost, saveCost bridgeCallbackCost
	stateMessages        int
	refusals             int
	deferrals            int
	restartState         int
	streamCallbacks      int
	taskCallbacks        int
	lastEvent            string
	pending              *agent.State // unadmitted, bounded latest-wins pair; never acknowledged by Save
	stateIDs             sync.Map
	pressure             bool
}

func (a *bridgeThroughputAgent) output(r agent.TaskResult) agent.Effects {
	for _, packet := range r.Chunk.Audio.Packets {
		a.timing.beforeAdmission(packet, time.Now())
	}
	effects := agent.Effects{}
	for i := range r.Chunk.Audio.Packets {
		effects.Audio = append(effects.Audio, agent.Audio{Turn: r.Turn, Source: agent.AudioRef{TaskID: r.TaskID, Sequence: r.Sequence, PacketOffset: uint32(i)}})
	}
	return effects
}
func (a *bridgeThroughputAgent) prototypeOutput(_ context.Context, r agent.TaskResult) (agent.Effects, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	defer a.handleCost.begin()()
	a.lastEvent = string(prototypeEncodedOutput)
	a.streamCallbacks++
	return a.output(r), nil
}
func (a *bridgeThroughputAgent) Handle(_ context.Context, e agent.Event) (agent.Effects, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	defer a.handleCost.begin()()
	a.lastEvent = string(e.Kind)
	if a.pending != nil && bytes.Equal(a.state.Bytes, a.pending.Bytes) && bytes.Equal(a.state.History, a.pending.History) {
		a.pending = nil
	}
	switch e.Kind {
	case agent.Started:
		a.turn = 1
		effects := a.start(e)
		if a.pressure {
			for i := range 2 {
				effects.StartTasks = append(effects.StartTasks, agent.TaskSpec{Step: fmt.Sprintf("p%d", i), Kind: "bridge.pressure/v1", Turn: ^agent.Turn(0), Class: agent.ReadOnly})
			}
		}
		return effects, nil
	case agent.Control:
		if e.Control.Name == "interrupt" {
			a.turn++
			a.state.Bytes = slices.Clone(a.state.Bytes)
			binary.BigEndian.PutUint64(a.state.Bytes[8:16], uint64(a.turn))
			if a.pending != nil {
				next := cloneV2State(*a.pending)
				binary.BigEndian.PutUint64(next.Bytes[8:16], uint64(a.turn))
				a.pending = &next
			}
			floor := a.turn
			return agent.Effects{CancelBefore: &floor, ClearAudioBefore: &floor, CancelTimers: []string{"state_restart", "retry/bridge.state/v1", "retry/bridge.audio/v1"}, SetTimers: []agent.TimerSpec{{ID: "next", After: 20 * time.Millisecond}}}, nil
		}
	case agent.TimerFired:
		if strings.HasPrefix(e.Timer.ID, "retry/") {
			kind := strings.TrimPrefix(e.Timer.ID, "retry/")
			spec := agent.TaskSpec{Step: fmt.Sprintf("r%d", a.restartState), Kind: kind, Turn: a.turn, Class: agent.ReadOnly}
			a.track(e.ID, spec)
			return agent.Effects{StartTasks: []agent.TaskSpec{spec}}, nil
		}
		switch e.Timer.ID {
		case "next":
			return a.start(e), nil

		case "checkpoint_retry":
			if a.pending != nil {
				a.state = cloneV2State(*a.pending)
				a.deferrals++
				return agent.Effects{SetTimers: []agent.TimerSpec{{ID: "checkpoint_retry", After: 30 * time.Millisecond}}}, nil
			}
		}
	case agent.TaskMessage:
		a.taskCallbacks++
		r := e.Result
		if r.Outcome == agent.Capacity {
			a.refusals++
			kind := a.expected[r.TaskID]
			if a.refusalKinds == nil {
				a.refusalKinds = map[string]int{}
			}
			a.refusalKinds[kind+":"+r.Detail]++
			effects := agent.Effects{SetTimers: []agent.TimerSpec{{ID: "checkpoint_retry", After: 30 * time.Millisecond}}}
			if r.Turn >= a.turn && kind != "" {
				a.restartState++
				effects.SetTimers = append(effects.SetTimers, agent.TimerSpec{ID: "retry/" + kind, After: 20 * time.Millisecond})
			}
			return effects, nil
		}
		if r.Chunk.Audio != nil {
			return a.output(*r), nil
		}
		if len(r.Chunk.Data) == 3072+8192 {
			next := agent.State{Version: 1, Bytes: slices.Clone(r.Chunk.Data[:3072]), History: slices.Clone(r.Chunk.Data[3072:])}
			binary.BigEndian.PutUint64(next.Bytes[8:16], uint64(a.turn))
			a.pending = &next
			a.state = cloneV2State(next)
			a.stateMessages++
			a.observedCheckpoint.Store(binary.BigEndian.Uint64(next.Bytes[:8]))
			return agent.Effects{SetTimers: []agent.TimerSpec{{ID: "checkpoint_retry", After: 30 * time.Millisecond}}}, nil
		}
	}
	return agent.Effects{}, nil
}
func (a *bridgeThroughputAgent) track(cause agent.EventID, spec agent.TaskSpec) {
	if a.expected == nil {
		a.expected = map[agent.TaskID]string{}
	}
	a.expected[deriveTaskID(a.sessionID, "bridge", cause, spec.Step)] = spec.Kind
}
func (a *bridgeThroughputAgent) start(e agent.Event) agent.Effects {
	effects := agent.Effects{StartTasks: []agent.TaskSpec{{Step: "s", Kind: "bridge.state/v1", Turn: a.turn, Class: agent.ReadOnly}, {Step: "a", Kind: "bridge.audio/v1", Turn: a.turn, Class: agent.ReadOnly}}}
	for _, spec := range effects.StartTasks {
		a.track(e.ID, spec)
	}
	return effects
}
func (a *bridgeThroughputAgent) Save(context.Context) (agent.State, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	defer a.saveCost.begin()()
	return a.state, nil
}
func (a *bridgeThroughputAgent) Restore(_ context.Context, state agent.State) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state = state
	a.turn = agent.Turn(binary.BigEndian.Uint64(state.Bytes[8:16]))
	return nil
}

type bridgeRenewalStore struct {
	sessionstore.Store
	sessionstore.ComponentStore
	mu        sync.Mutex
	requests  []time.Time
	successes []time.Time
	delay     time.Duration
}

func (s *bridgeRenewalStore) Renew(ctx context.Context, lease sessionstore.Lease, ttl time.Duration) (sessionstore.Lease, error) {
	s.mu.Lock()
	s.requests = append(s.requests, time.Now())
	s.mu.Unlock()
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			return sessionstore.Lease{}, ctx.Err()
		}
	}
	result, err := s.Store.Renew(ctx, lease, ttl)
	if err == nil {
		s.mu.Lock()
		s.successes = append(s.successes, time.Now())
		s.mu.Unlock()
	}
	return result, err
}

func bridgeQuantile(values []time.Duration, p float64) float64 {
	if len(values) == 0 {
		return 0
	}
	ordered := slices.Clone(values)
	slices.Sort(ordered)
	return float64(ordered[max(0, int(math.Ceil(float64(len(ordered))*p))-1)]) / float64(time.Millisecond)
}
func bridgeGaps(at []time.Time) []time.Duration {
	var gaps []time.Duration
	for i := 1; i < len(at); i++ {
		gaps = append(gaps, at[i].Sub(at[i-1]))
	}
	return gaps
}
func bridgeStoreTraffic() (float64, float64) {
	registry := prometheus.NewRegistry()
	registry.MustRegister(metrics.TrafficCollectors()...)
	families, err := registry.Gather()
	if err != nil {
		panic(err)
	}
	var bytes, commands float64
	for _, f := range families {
		if f.GetName() != "relais_transport_bytes_total" && f.GetName() != "relais_store_wire_commands_total" {
			continue
		}
		for _, m := range f.Metric {
			labels := map[string]string{}
			for _, l := range m.Label {
				labels[l.GetName()] = l.GetValue()
			}
			if labels["client"] != "session" {
				continue
			}
			if f.GetName() == "relais_transport_bytes_total" {
				bytes += m.GetCounter().GetValue()
			} else {
				commands += m.GetCounter().GetValue()
			}
		}
	}
	return commands, bytes
}

// TestBridgeTaskOutputThroughput is opt-in because each measured speech phase
// lasts at least 60 seconds. Normal units and -short never pay that cost.
// Run with RELAIS_BRIDGE_THROUGHPUT=1, optionally RELAIS_BRIDGE_LOAD=1.
// The fake bridge has no provider, socket, Python, codec or remote dependencies.
func TestBridgeTaskOutputThroughput(t *testing.T) {
	if testing.Short() || os.Getenv("RELAIS_BRIDGE_THROUGHPUT") != "1" {
		t.Skip("opt-in sustained bridge measurement")
	}
	target := 3003
	if value := os.Getenv("RELAIS_BRIDGE_PACKETS"); value != "" {
		var err error
		target, err = strconv.Atoi(value)
		require.NoError(t, err)
		require.Greater(t, target, 0)
	}
	factorName := os.Getenv("RELAIS_BRIDGE_FACTOR")
	if factorName == "" {
		factorName = "baseline"
		if os.Getenv("RELAIS_BRIDGE_LOAD") == "1" {
			factorName = "combined"
		}
	}
	factor, validFactor := bridgeFactor(factorName)
	require.True(t, validFactor, "unknown load factor")
	loaded := factorName != "baseline"
	batchBound := 10
	if value := os.Getenv("RELAIS_BRIDGE_BATCH"); value != "" {
		var err error
		batchBound, err = strconv.Atoi(value)
		require.NoError(t, err)
	}
	require.Contains(t, []int{10, 20}, batchBound)
	candidate := os.Getenv("RELAIS_BRIDGE_OUTPUT")
	if candidate == "" {
		candidate = "fixed"
	}
	require.Contains(t, []string{"fixed", "catchup", "stream"}, candidate)
	stress := os.Getenv("RELAIS_BRIDGE_STRESS") == "1"
	var currentTurn atomic.Uint64
	currentTurn.Store(1)
	procs := factor.Procs
	previous := runtime.GOMAXPROCS(procs)
	defer runtime.GOMAXPROCS(previous)
	var base sessionstore.Store = sessionstore.NewMemory()
	if addr := redistestenv.Address(t); addr != "" {
		store, err := sessionstore.NewRedis(context.Background(), storage.RedisConfig{Addr: addr, Prefix: fmt.Sprintf("bridge-throughput-%d:", time.Now().UnixNano())}, make([]byte, 32))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, store.Close()) })
		base = store
	}
	store := &bridgeRenewalStore{Store: base, ComponentStore: base.(sessionstore.ComponentStore)}
	store.delay = factor.Renewal
	snap := dtlsFixture(t)
	lease, err := store.Claim(context.Background(), snap.State.ID, netip.MustParseAddrPort("127.0.0.1:10000"), time.Hour)
	require.NoError(t, err)
	s, wire := dtlsCheckpointSession(t, store, snap, lease, ResumeOptions{})
	wireProbe := &bridgeWireProbe{packetConn: wire, original: wire}
	s.worker.conn = wireProbe
	s.componentMode = true
	s.worker.cfg.Relay.LeaseTTL = 34 * time.Second
	s.worker.log = s.worker.cfg.LoggerFactory.NewLogger("bridge-throughput")
	s.state.Agent = agentState{}
	s.worker.cfg.SnapshotInterval = 100 * time.Millisecond
	observations := &bridgePacketTiming{ready: map[uint32]time.Time{}, admitted: map[uint32]time.Time{}, turns: map[uint32]agent.Turn{}}
	stallObserver := &bridgeStallObserver{}
	a := &bridgeThroughputAgent{timing: observations, state: agent.State{Version: 1, Bytes: make([]byte, 3072), History: make([]byte, 8192)}}
	a.sessionID = s.id
	a.pressure = stress
	binary.BigEndian.PutUint64(a.state.Bytes[8:16], 1)
	var queuesMu sync.Mutex
	queues := map[agent.Turn]chan []byte{}
	turnQueue := func(turn agent.Turn) chan []byte {
		queuesMu.Lock()
		defer queuesMu.Unlock()
		if queues[turn] == nil {
			queues[turn] = make(chan []byte, 256)
		}
		return queues[turn]
	}
	var checkpointSerial atomic.Uint64
	var offersMu sync.Mutex
	offersOpen := true
	var maxBatch atomic.Int64
	var jitterIndex, injectedStalls atomic.Int64
	var sourceStart time.Time
	var produced atomic.Int64
	var emitted atomic.Int64
	audioHandler := func(ctx context.Context, run *agent.TaskRun) error {
		seq := run.After()
		packetQueue := turnQueue(run.Spec().Turn)
		last := time.Time{}
		for {
			if candidate == "catchup" && !last.IsZero() {
				timer := time.NewTimer(time.Until(last.Add(100 * time.Millisecond)))
				select {
				case <-ctx.Done():
					timer.Stop()
					return ctx.Err()
				case <-timer.C:
				}
			}
			var packets [][]byte
			closed := false
			minimum := 1
			if candidate == "fixed" || (candidate == "catchup" && last.IsZero()) {
				minimum = 5
			}
			for len(packets) < minimum {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case p, ok := <-packetQueue:
					if !ok {
						closed = true
						break
					}
					packets = append(packets, p)
				}
				if closed {
					break
				}
			}
			if len(packets) == 0 {
				return nil
			}
			// A global schedule survives task rotation: both candidates see one
			// 150 ms bridge stall per 5 s, rather than one per task lifetime.
			stalled := false
			if factor.Stall > 0 {
				index := int64(time.Since(sourceStart) / (5 * time.Second))
				old := jitterIndex.Load()
				if index > old && jitterIndex.CompareAndSwap(old, index) {
					stallObserver.begin(time.Now())
					timer := time.NewTimer(factor.Stall)
					select {
					case <-ctx.Done():
						timer.Stop()
						return ctx.Err()
					case <-timer.C:
						stallObserver.end(time.Now())
						stalled = true
						injectedStalls.Add(1)
					}
				}
			}
			// Refresh immediately before emission: a bridge pause must not leave
			// us emitting a stale pre-stall batch while new ready packets wait.
			if candidate == "catchup" {
				for len(packets) < batchBound && !closed {
					select {
					case p, ok := <-packetQueue:
						if !ok {
							closed = true
						} else {
							packets = append(packets, p)
						}
					default:
						goto emissionReady
					}
				}
			}
		emissionReady:
			seq++
			audio := &agent.EncodedAudio{Format: "audio/opus", Packets: packets}
			var err error
			if candidate == "stream" {
				for {
					err = s.submitPrototypeOutput(ctx, agent.TaskResult{TaskID: run.ID(), Sequence: seq, Turn: run.Spec().Turn, Outcome: agent.OK, Chunk: agent.Chunk{Audio: audio}})
					if !errors.Is(err, agent.ErrBackpressure) {
						break
					}
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-time.After(10 * time.Millisecond):
					}
				}
			} else {
				err = run.Emit(ctx, seq, agent.Chunk{Audio: audio})
			}
			if err != nil {
				return err
			}
			if stalled {
				stallObserver.release(time.Now())
			}
			last = time.Now()
			emitted.Add(int64(len(packets)))
			for {
				old := maxBatch.Load()
				if int64(len(packets)) <= old || maxBatch.CompareAndSwap(old, int64(len(packets))) {
					break
				}
			}
			if closed {
				return nil
			}
		}
	}

	stateHandler := func(ctx context.Context, run *agent.TaskRun) error {
		a.stateIDs.Store(run.ID(), struct{}{})
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		seq := run.After()
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-tick.C:
				seq++
				data := []byte(`{"input_gap":[1,2],"control":"ping"}`)
				offersMu.Lock()
				serial := uint64(time.Since(sourceStart) / (5 * time.Second))
				serial = min(serial, uint64(time.Duration(target)*20*time.Millisecond/(5*time.Second)))
				if !offersOpen {
					serial = checkpointSerial.Load()
				}
				if serial > 0 && serial > a.observedCheckpoint.Load() {
					data = make([]byte, 3072+8192)
					checkpointSerial.Store(serial)
					binary.BigEndian.PutUint64(data, serial)
					binary.BigEndian.PutUint64(data[3072:], serial)
				}
				offersMu.Unlock()
				if err := run.Emit(ctx, seq, agent.Chunk{Data: data}); err != nil {
					return err
				}
			}
		}
	}
	registration := agenthost.Registration{
		Name:    "bridge",
		Factory: func(agent.FactoryArgs) agent.Agent { return a },
		Tasks: map[string]agent.TaskHandler{
			"bridge.state/v1":    stateHandler,
			"bridge.audio/v1":    audioHandler,
			"bridge.pressure/v1": func(ctx context.Context, _ *agent.TaskRun) error { <-ctx.Done(); return ctx.Err() },
		},
	}
	s.worker.cfg.Agent = (AgentConfig{CallbackTimeout: 100 * time.Millisecond, Factory: agenthost.Factory(registration)}).defaults()
	require.NoError(t, s.initAgent(false, ResumeOptions{}))
	if candidate == "stream" {
		s.agent.v2.prototypeOutput = make(chan prototypeOutputRequest)
	}
	s.worker.sessions = map[string]*session{s.id: s}
	require.NoError(t, s.checkpoint.persist(context.Background(), checkpointRequired))
	close(s.agent.v2.ready)
	// Decode the captured outbound SRTP exactly as a caller would, without an
	// Opus decoder. Arrival gaps here exclude an actual network and caller playout.
	connection, ok := s.dtlsConn.ConnectionState()
	require.True(t, ok)
	config := srtp.Config{Profile: s.state.SRTP.Profile}
	require.NoError(t, config.ExtractSessionKeysFromDTLS(&connection, false))
	receiver := testContext(t, config.Keys.LocalMasterKey, config.Keys.LocalMasterSalt, config.Profile)
	ctx, cancel := context.WithCancel(s.ctx)
	var workers sync.WaitGroup
	failures := make(chan error, 16)
	workers.Go(func() {
		for {
			select {
			case <-ctx.Done():
				return
			case raw := <-wire.packets:
				plain, e := receiver.DecryptRTP(nil, raw, nil)
				if e != nil {
					failures <- e
					return
				}
				var packet rtp.Packet
				if e = packet.Unmarshal(plain); e != nil {
					failures <- e
					return
				}
				observations.observe(packet.Payload, time.Now(), true)
				observations.mu.Lock()
				if observations.rtpSeen {
					advance := packet.Timestamp - observations.lastRTPTime
					if advance > 960 {
						observations.pacerRebases++
						observations.pacerSkipTicks += uint64(advance - 960)
					}
				}
				observations.lastRTPTime, observations.rtpSeen = packet.Timestamp, true
				observations.mu.Unlock()
			}
		}
	})
	workers.Go(func() {
		tick := time.NewTicker(time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				s.mu.Lock()
				now := time.Now()
				for _, ref := range s.agent.v2.playback.queue {
					if packet, _, ok := s.agent.v2.playback.packet(ref); ok {
						observations.observe(packet, now, false)
					}
				}
				observations.mu.Lock()
				observations.maxPacer = max(observations.maxPacer, len(s.agent.v2.playback.queue))
				pending := len(observations.ready) - len(observations.admitted)
				if !stress {
					observations.maxPending = max(observations.maxPending, pending)
				}
				observations.mu.Unlock()
				if factor.Stall > 0 {
					stallObserver.sample(now, pending, len(s.agent.v2.playback.queue))
				}
				s.mu.Unlock()
			}
		}
	})
	var checkpointAges []time.Duration
	workers.Go(func() {
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				status, e := s.worker.agentSessionStatus(s.id)
				if e != nil {
					failures <- e
					return
				}
				checkpointAges = append(checkpointAges, time.Duration(status.CheckpointAgeMS*float64(time.Millisecond)))
			}
		}
	})
	workers.Go(func() { s.checkpoint.run(ctx) })
	// Exercise the production lease-renewal method at its normal one-second
	// cadence. Fleet batch scheduling and stalled storage are separate limits.
	workers.Go(func() {
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				s.worker.renewLease(s)
			}
		}
	})
	var loadSink atomic.Uint64
	if factor.Busy {
		workers.Go(func() {
			var n uint64 = 1
			for ctx.Err() == nil {
				until := time.Now().Add(35 * time.Millisecond)
				for time.Now().Before(until) {
					n = n*6364136223846793005 + 1
				}
				loadSink.Store(n)
				runtime.Gosched()
			}
		})
	}
	sourceStart = time.Now()
	actorDone := make(chan struct{})
	go func() { defer close(actorDone); s.v2AgentLoop() }()
	defer func() { cancel(); s.cancel(); <-actorDone; workers.Wait(); s.worker.running.Wait() }()
	start := sourceStart
	beforeCommands, beforeBytes := bridgeStoreTraffic()
	// 3,003 independent ready times: 60.06 seconds and a three-packet final batch.
	// Timer delivery jitter belongs to this fake bridge; it never invents an
	// earlier scheduled ready time for a packet that did not yet exist.
	producerDone := make(chan struct{})
	var sourceFinished time.Time
	workers.Go(func() {
		defer func() {
			offersMu.Lock()
			offersOpen = false
			offersMu.Unlock()
			sourceFinished = time.Now()
			close(producerDone)
		}()

		for id := uint32(1); id <= uint32(target); id++ {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Until(start.Add(time.Duration(id) * 20 * time.Millisecond))):
			}
			packet := make([]byte, 160)
			packet[0] = 0xf8
			binary.BigEndian.PutUint32(packet[1:5], id)
			turn := agent.Turn(currentTurn.Load())
			binary.BigEndian.PutUint64(packet[5:13], uint64(turn))
			observations.mu.Lock()
			observations.ready[id] = time.Now()
			observations.turns[id] = turn
			observations.produced++
			observations.mu.Unlock()
			select {
			case turnQueue(turn) <- packet:
				produced.Add(1)
			case <-ctx.Done():
				return
			}
		}
		queuesMu.Lock()
		for _, q := range queues {
			close(q)
		}
		queuesMu.Unlock()
	})
	var controlSeq uint64
	var controlRetries, interruptions int
	var maxActive int
	controlTick := time.NewTicker(50 * time.Millisecond)
	var nextPing = time.Second / 2
	nextInterrupt := 2 * time.Second
	rapidIndex := 0
	rapid := []time.Duration{10 * time.Second, 10050 * time.Millisecond, 10100 * time.Millisecond, 10150 * time.Millisecond, 10200 * time.Millisecond}
	defer controlTick.Stop()
	sampleTick := time.NewTicker(time.Second)
	defer sampleTick.Stop()
	var sourceEnd time.Time
	var sourceEndPacer int
	for {
		select {
		case e := <-failures:
			t.Fatal(e)
		case <-s.ctx.Done():
			t.Logf("BRIDGE_FAILURE %s", a.failureDetails(wireProbe))
			t.Fatalf("agent stopped during measurement: %v", s.ctx.Err())
		case <-controlTick.C:
			elapsedNow := time.Since(start)
			name := "ping"
			interrupt := stress && ((rapidIndex < len(rapid) && elapsedNow >= rapid[rapidIndex]) || elapsedNow >= nextInterrupt)
			if interrupt {
				name = "interrupt"
				if rapidIndex < len(rapid) && elapsedNow >= rapid[rapidIndex] {
					rapidIndex++
				} else {
					nextInterrupt += 2 * time.Second
				}
			} else if elapsedNow < nextPing {
				continue
			} else {
				nextPing += time.Second / 2
			}
			controlSeq++
			cctx, ccancel := context.WithTimeout(ctx, time.Second)
			var e error
			for {
				_, e = s.worker.AgentControl(cctx, s.id, agent.ControlMessage{ID: fmt.Sprint(controlSeq), Sequence: controlSeq, Name: name})
				if !errors.Is(e, agent.ErrCapacity) && !errors.Is(e, agent.ErrBackpressure) {
					break
				}
				controlRetries++
				select {
				case <-cctx.Done():
					e = cctx.Err()
				case <-time.After(20 * time.Millisecond):
					continue
				}
				break
			}
			ccancel()
			if e != nil {
				t.Logf("BRIDGE_FAILURE %s", a.failureDetails(wireProbe))
			}
			require.NoError(t, e)
			if interrupt {
				observations.mu.Lock()
				if observations.cancelled == nil {
					observations.cancelled = map[agent.Turn]time.Time{}
				}
				observations.cancelled[agent.Turn(currentTurn.Load())] = time.Now()
				observations.mu.Unlock()
				currentTurn.Add(1)
				interruptions++
			}
		case <-sampleTick.C:
			pending := 0
			observations.mu.Lock()
			for id, turn := range observations.turns {
				if !stress || uint64(turn) == currentTurn.Load() {
					if _, admitted := observations.admitted[id]; !admitted {
						pending++
					}
				}
			}
			observations.mu.Unlock()
			status, e := s.worker.agentSessionStatus(s.id)
			require.NoError(t, e)
			active := 0
			for _, task := range status.Tasks {
				if task.Phase == taskStarted && !task.Final {
					active++
				}
			}
			maxActive = max(maxActive, active)
			observations.mu.Lock()
			observations.maxPending = max(observations.maxPending, pending)
			if time.Since(start) < 3*time.Second {
				observations.pendingFirst = pending
			}
			observations.pendingLast = pending
			observations.mu.Unlock()
		case <-producerDone:
			sourceEnd = sourceFinished
			s.mu.Lock()
			sourceEndPacer = len(s.agent.v2.playback.queue)
			s.mu.Unlock()
			goto drain
		}
	}
drain:
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		observations.mu.Lock()
		count := len(observations.admitted)
		observations.mu.Unlock()
		observations.mu.Lock()
		expected := 0
		for id := range observations.ready {
			_ = id
			expected++
		}
		if stress {
			expected = 0
			for id, turn := range observations.turns {
				if uint64(turn) >= currentTurn.Load() {
					if _, ok := observations.admitted[id]; !ok {
						expected++
					}
				}
			}
			expected += count
		}
		observations.mu.Unlock()
		if count == expected {
			break
		}
		select {
		case <-time.After(10 * time.Millisecond):
		case e := <-failures:
			t.Fatal(e)
		case <-deadline.C:
			t.Fatalf("only %d/3003 packets admitted; producer queue=%d", count, int(produced.Load()-emitted.Load()))
		}
	}
	// Read the published actor pair, never the application's mutable staging.
	sourcePublished, sourceDurable, _ := bridgePublishedCheckpoints(s)
	tailAt := time.Now()
	settleDeadline := time.NewTimer(2 * time.Second)
	defer settleDeadline.Stop()
	var published agentState
	var durableSeq uint64
	settled := false
	s.mu.Lock()
	admissionEndPacer, admissionEndMedia := len(s.agent.v2.playback.queue), s.agent.v2.playback.duration
	s.mu.Unlock()
settle:
	for {
		published, durableSeq, _ = bridgePublishedCheckpoints(s)
		wanted := checkpointSerial.Load()
		if bridgeStateSequence(published.State.Bytes) >= wanted && durableSeq >= wanted {
			settled = true
			break
		}
		select {
		case err := <-failures:
			t.Fatal(err)
		case <-s.ctx.Done():
			t.Fatal(s.ctx.Err())
		case <-settleDeadline.C:
			break settle
		case <-time.After(10 * time.Millisecond):
		}
	}
	settleTail := time.Since(tailAt)
	// A checkpoint acknowledgement and a paced media drain are different facts.
	// Keep the artificial load running. Allow a bounded tail derived from the
	// queued media duration instead of demanding >2 s of media fit into 2 s.
	s.mu.Lock()
	remainingMedia := s.agent.v2.playback.duration
	s.mu.Unlock()
	drainBudget := bridgePlaybackDrainBudget(remainingMedia)
	drainTimer := time.NewTimer(drainBudget)
	defer drainTimer.Stop()
	audioDrained := false
	twoSecondPacer := -1
audioDrain:
	for {
		s.mu.Lock()
		queued := len(s.agent.v2.playback.queue)
		s.mu.Unlock()
		if time.Since(tailAt) >= 2*time.Second && twoSecondPacer < 0 {
			twoSecondPacer = queued
		}
		observations.mu.Lock()
		wirePackets := observations.wirePackets
		observations.mu.Unlock()
		if queued == 0 && (stress || wirePackets == target) {
			audioDrained = true
			break
		}
		select {
		case <-time.After(10 * time.Millisecond):
		case <-drainTimer.C:
			break audioDrain
		case <-s.ctx.Done():
			t.Logf("BRIDGE_FAILURE %s", a.failureDetails(wireProbe))
			t.Fatal(s.ctx.Err())
		case e := <-failures:
			t.Fatal(e)
		}
	}
	if twoSecondPacer < 0 && audioDrained {
		twoSecondPacer = 0
	}
	drainTail := time.Since(tailAt)

	elapsed := time.Since(start)
	afterCommands, afterBytes := bridgeStoreTraffic()
	cancel()
	s.cancel()
	<-actorDone
	workers.Wait()
	s.worker.running.Wait()
	// Make deferred cleanup idempotent after collecting actor-owned measurements.
	actorDone = make(chan struct{})
	close(actorDone)
	tuple, err := base.(sessionstore.ComponentStore).ReadComponents(context.Background(), s.id)
	require.NoError(t, err)
	var storedWork, storedHistory []byte
	require.NoError(t, unmarshalComponent(tuple.Components.Work, componentWork, &storedWork))
	require.NoError(t, unmarshalComponent(tuple.Components.History, componentHistory, &storedHistory))
	require.Len(t, storedWork, 3072)
	require.Len(t, storedHistory, 8192)
	require.Equal(t, bridgeStateSequence(storedWork), bridgeStateSequence(storedHistory), "coherent full component pair")
	lower, delays, unpaired := observations.paired()
	phaseLatency, censored, censoredOver150, censoredMax := observations.phaseAndCancellation(start, stress, loaded)
	observations.mu.Lock()
	gaps := slices.Clone(observations.gaps)
	callbackOnly := len(observations.before) - len(lower)
	observations.mu.Unlock()
	// agentCall deliberately permits a canceled callback to settle after the
	// actor exits. Sample its state under the same lock used by callbacks.
	a.mu.Lock()
	defer a.mu.Unlock()
	pendingCandidate := a.pending != nil && (!bytes.Equal(published.State.Bytes, a.pending.Bytes) || !bytes.Equal(published.History, a.pending.History))

	report := map[string]any{
		"fake_wire": wireProbe.summary(), "factor": factorName, "batch_bound": batchBound, "cpu_load": factor.Busy, "stall_ms": factor.Stall.Milliseconds(),
		"stall_observations": observations.stallSummary(stallObserver.copy(), sourceEnd),
		"source_duration_s":  sourceEnd.Sub(sourceStart).Seconds(), "source_end_pacer_packets": sourceEndPacer, "admission_end_pacer_packets": admissionEndPacer, "admission_end_queued_media_ms": float64(admissionEndMedia) / float64(time.Millisecond),
		"audio_drained": audioDrained, "audio_drain_budget_ms": float64(drainBudget) / float64(time.Millisecond), "audio_drain_tail_ms": float64(drainTail) / float64(time.Millisecond), "pacer_packets_after_two_seconds": twoSecondPacer, "pacer_rebase_events": observations.pacerRebases, "pacer_skipped_rtp_ms": float64(observations.pacerSkipTicks) / 48,
		"phase_latency": phaseLatency, "cancelled_before_observation": censored, "cancelled_wait_over150": censoredOver150, "cancelled_wait_max_ms": float64(censoredMax) / float64(time.Millisecond), "pacer_queue_max": observations.maxPacer,
		"checkpoint_loop": "production", "callback_deadline_ms": s.worker.cfg.Agent.CallbackTimeout.Milliseconds(), "save_interval_ms": s.worker.cfg.Agent.SaveInterval.Milliseconds(), "snapshot_interval_ms": s.worker.cfg.SnapshotInterval.Milliseconds(),
		"settled": settled, "checkpoint_settle_tail_ms": float64(settleTail) / float64(time.Millisecond),
		"source_end_published_checkpoint_seq": bridgeStateSequence(sourcePublished.State.Bytes), "source_end_durable_checkpoint_seq": sourceDurable,
		"callback_only_packets": callbackOnly, "unpaired_packets": unpaired,
		"refusal_kinds": a.refusalKinds,
		"jitter_stalls": injectedStalls.Load(), "renew_delay_ms": float64(store.delay) / float64(time.Millisecond),
		"interruptions": interruptions, "control_retries": controlRetries, "max_active": maxActive, "offered_checkpoints": checkpointSerial.Load(),
		"candidate": candidate, "stress": stress, "max_batch": maxBatch.Load(), "deferrals": a.deferrals, "pending_candidate": pendingCandidate, "task_callbacks": a.taskCallbacks, "stream_callbacks": a.streamCallbacks,
		"wire_packets": len(gaps) + 1, "accepted_checkpoint_seq": bridgeStateSequence(published.State.Bytes), "durable_checkpoint_seq": bridgeStateSequence(storedWork),
		"loaded": loaded, "gomaxprocs": procs, "duration_s": elapsed.Seconds(), "packets": len(delays), "lower_packets": len(lower), "packet_bytes": 160,
		"latency_lower_ms":  []float64{bridgeQuantile(lower, .5), bridgeQuantile(lower, .95), bridgeQuantile(lower, .99)},
		"latency_upper_ms":  []float64{bridgeQuantile(delays, .5), bridgeQuantile(delays, .95), bridgeQuantile(delays, .99)},
		"wire_gap_ms":       []float64{bridgeQuantile(gaps, .5), bridgeQuantile(gaps, .95), bridgeQuantile(gaps, .99), bridgeQuantile(gaps, 1)},
		"queue_max_packets": observations.maxPending, "queue_first_packets": observations.pendingFirst, "queue_last_packets": observations.pendingLast,
		"handle_p99_ms": bridgeQuantile(a.handleCost.wall, .99), "save_p99_ms": bridgeQuantile(a.saveCost.wall, .99),
		"handle_cpu_ns": a.handleCost.cpu, "handle_cpu_known": a.handleCost.known, "save_cpu_ns": a.saveCost.cpu, "save_cpu_known": a.saveCost.known,
		"state_checkpoints": a.stateMessages, "refusals": a.refusals,
		"checkpoint_age_p99_ms":   bridgeQuantile(checkpointAges, .99),
		"encoded_component_bytes": []int{len(tuple.Components.Transport) + componentCipherOverhead, len(tuple.Components.Work) + componentCipherOverhead, len(tuple.Components.History) + componentCipherOverhead, len(tuple.Components.Setup) + componentCipherOverhead},
		"store_commands_per_s":    (afterCommands - beforeCommands) / elapsed.Seconds(), "store_bytes_per_s": (afterBytes - beforeBytes) / elapsed.Seconds(),
		"renew_request_gap_max_ms": bridgeQuantile(bridgeGaps(store.requests), 1), "renew_success_gap_max_ms": bridgeQuantile(bridgeGaps(store.successes), 1),
		"renew_request_gap_p99_ms": bridgeQuantile(bridgeGaps(store.requests), .99), "renew_success_gap_p99_ms": bridgeQuantile(bridgeGaps(store.successes), .99),
	}
	encoded, err := json.Marshal(report)
	require.NoError(t, err)
	t.Logf("BRIDGE_MEASUREMENT %s", encoded)
	minimum := target
	if minimum > 3000 {
		minimum = 3000
	}
	if stress {
		minimum = target * 2 / 3
	}
	require.GreaterOrEqual(t, len(delays), minimum)
	require.Zero(t, unpaired, "latency pairs must name identical packet IDs")
	require.True(t, settled, "final published pair must become checkpointed after quiescence")
	require.True(t, audioDrained, "pacer must drain within the bounded media-duration tail")
	require.False(t, pendingCandidate, "latest candidate must settle before handoff")
	require.LessOrEqual(t, maxActive, 4, "stock active-task ceiling")
	if stress {
		require.Equal(t, 4, maxActive, "pressure must occupy every task slot")
	}
	require.LessOrEqual(t, bridgeQuantile(delays, .95), 150.0, "pacer admission upper-bound p95")
	require.Equal(t, "meets", bridgeThroughputVerdictWithMinimum(lower, delays, minimum), "added p95 admission threshold; lower and upper bounds distinguish misses from sampling uncertainty")
	require.Equal(t, "meets", bridgeOutputPhasesVerdict(phaseLatency), "a rapid-interruption phase must not be hidden by the whole-run p95")
}

// The measurement must distinguish a demonstrated miss from observer uncertainty.
func bridgeThroughputVerdict(lower, upper []time.Duration) string {
	return bridgeThroughputVerdictWithMinimum(lower, upper, 3000)
}
func bridgeThroughputVerdictWithMinimum(lower, upper []time.Duration, minimum int) string {
	if len(lower) < minimum || len(upper) < minimum {
		return "inconclusive"
	}
	if bridgeQuantile(lower, .95) > 150 {
		return "misses"
	}
	if bridgeQuantile(upper, .95) <= 150 {
		return "meets"
	}
	return "inconclusive"
}
func TestBridgeThroughputVerdict(t *testing.T) {
	for _, tc := range []struct {
		name         string
		lower, upper time.Duration
		count        int
		want         string
	}{
		{"below", 80 * time.Millisecond, 90 * time.Millisecond, 3000, "meets"},
		{"at-threshold", 150 * time.Millisecond, 150 * time.Millisecond, 3000, "meets"},
		{"above", 151 * time.Millisecond, 152 * time.Millisecond, 3000, "misses"},
		{"observer-uncertainty", 140 * time.Millisecond, 160 * time.Millisecond, 3000, "inconclusive"},
		{"short", time.Millisecond, time.Millisecond, 100, "inconclusive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lower, upper := make([]time.Duration, tc.count), make([]time.Duration, tc.count)
			for i := range lower {
				lower[i], upper[i] = tc.lower, tc.upper
			}
			require.Equal(t, tc.want, bridgeThroughputVerdict(lower, upper))
		})
	}
}

// A command counter that is not registered with the default gatherer must not
// silently report zero store requests in the throughput evidence.
func TestBridgeStoreTrafficCountsCommands(t *testing.T) {
	addr := redistestenv.Address(t)
	if addr == "" {
		t.Skip("requires a disposable test Redis")
	}
	store, err := sessionstore.NewRedis(context.Background(), storage.RedisConfig{Addr: addr, Prefix: fmt.Sprintf("bridge-traffic-%d:", time.Now().UnixNano())}, make([]byte, 32))
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()
	beforeCommands, beforeBytes := bridgeStoreTraffic()
	_, err = store.Clock(context.Background(), "bridge-traffic")
	require.NoError(t, err)
	afterCommands, afterBytes := bridgeStoreTraffic()
	require.Greater(t, afterCommands, beforeCommands)
	require.Greater(t, afterBytes, beforeBytes)
}

func TestBridgeLatencyCountsEachPacketOnce(t *testing.T) {
	at := time.Now()
	timing := &bridgePacketTiming{ready: map[uint32]time.Time{1: at}, admitted: map[uint32]time.Time{}}
	packet := []byte{0xf8, 0, 0, 0, 1}
	timing.beforeAdmission(packet, at.Add(80*time.Millisecond))
	timing.beforeAdmission(packet, at.Add(500*time.Millisecond))
	timing.observe(packet, at.Add(90*time.Millisecond), false)
	timing.observe(packet, at.Add(100*time.Millisecond), true)
	require.Equal(t, []time.Duration{80 * time.Millisecond}, timing.lower)
	require.Equal(t, []time.Duration{90 * time.Millisecond}, timing.delays)
}

// Paired latency bounds use the same IDs. Intentional cancellation can remove a
// packet after its callback but before a queue poll; those callbacks do not
// belong to the admitted packet distribution.
func (m *bridgePacketTiming) paired() (lower, upper []time.Duration, unpaired int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, admitted := range m.admitted {
		ready, readyOK := m.ready[id]
		before, beforeOK := m.before[id]
		if !readyOK || !beforeOK || before.Before(ready) || admitted.Before(before) {
			unpaired++
			continue
		}
		lower = append(lower, before.Sub(ready))
		upper = append(upper, admitted.Sub(ready))
	}
	return
}
func bridgeStateSequence(data []byte) uint64 {
	if len(data) < 8 {
		return 0
	}
	return binary.BigEndian.Uint64(data[:8])
}
func bridgePublishedCheckpoints(s *session) (agentState, uint64, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneAgentState(s.state.Agent), bridgeStateSequence(s.checkpoint.durableAgent.State.Bytes), len(s.agent.v2.playback.queue)
}
func TestBridgeLatencyPairingExcludesCancelledCallbacks(t *testing.T) {
	at := time.Now()
	m := &bridgePacketTiming{ready: map[uint32]time.Time{1: at, 2: at}, before: map[uint32]time.Time{1: at.Add(time.Millisecond), 2: at.Add(time.Second)}, admitted: map[uint32]time.Time{1: at.Add(2 * time.Millisecond)}}
	lower, upper, unpaired := m.paired()
	require.Zero(t, unpaired)
	require.Equal(t, []time.Duration{time.Millisecond}, lower)
	require.Equal(t, []time.Duration{2 * time.Millisecond}, upper)
}

func (m *bridgePacketTiming) phaseAndCancellation(start time.Time, stress, loaded bool) (map[string]any, int, int, time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	low, up := map[string][]time.Duration{}, map[string][]time.Duration{}
	censored, over := 0, 0
	var maximum time.Duration
	for id, ready := range m.ready {
		admitted, seen := m.admitted[id]
		if !seen {
			if cancel, ok := m.cancelled[m.turns[id]]; ok {
				wait := max(time.Duration(0), cancel.Sub(ready))
				censored++
				maximum = max(maximum, wait)
				if wait > 150*time.Millisecond {
					over++
				}
			}
			continue
		}
		before, ok := m.before[id]
		if !ok || before.Before(ready) || admitted.Before(before) {
			continue
		}
		phase := "steady"
		if stress {
			phase = "rotation"
			if elapsed := ready.Sub(start); elapsed >= 10*time.Second && elapsed < 11*time.Second {
				phase = "rapid"
			}
		}
		if loaded {
			phase += "_loaded"
		}
		low[phase] = append(low[phase], before.Sub(ready))
		up[phase] = append(up[phase], admitted.Sub(ready))
	}
	result := map[string]any{}
	for phase, lower := range low {
		upper := up[phase]
		result[phase] = map[string]any{"packets": len(lower), "lower_ms": []float64{bridgeQuantile(lower, .5), bridgeQuantile(lower, .95), bridgeQuantile(lower, .99)}, "upper_ms": []float64{bridgeQuantile(upper, .5), bridgeQuantile(upper, .95), bridgeQuantile(upper, .99)}}
	}
	return result, censored, over, maximum
}
