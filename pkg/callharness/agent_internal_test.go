package callharness

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/relaishq/relais/internal/agenthost"
	"github.com/relaishq/relais/internal/redisendpoint"
	"github.com/relaishq/relais/internal/workerprobe"
	"github.com/relaishq/relais/pkg/agent"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/relay"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/relaishq/relais/pkg/storage"
	"github.com/stretchr/testify/require"
)

type agentResumeObservation struct {
	notice agent.ResumeNotice
	count  uint64
}
type harnessCounterAgent struct {
	count           uint64
	consumedAt      int64
	resumes         chan agentResumeObservation
	restoreFailure  bool
	callbackTimeout string
	resumeSave      bool
	processDelay    time.Duration
}

func (a *harnessCounterAgent) Process(ctx context.Context, _ agent.Input) (agent.Output, error) {
	if a.processDelay > 0 {
		select {
		case <-time.After(a.processDelay):
		case <-ctx.Done():
			return agent.Output{}, ctx.Err()
		}
	}
	a.count++
	a.consumedAt = time.Now().UnixNano()
	// One 20 ms CELT silence frame, with the deterministic count in Opus padding.
	payload := append([]byte{0xfb, 0x41, 8, 0xff, 0xfe}, make([]byte, 8)...)
	binary.BigEndian.PutUint64(payload[len(payload)-8:], a.count)
	return agent.Output{Audio: payload, Changed: true}, nil
}
func (a *harnessCounterAgent) Save(ctx context.Context) (agent.State, error) {
	if a.callbackTimeout == "save" && a.resumeSave {
		<-ctx.Done()
		return agent.State{}, ctx.Err()
	}
	data := make([]byte, 16)
	binary.BigEndian.PutUint64(data, a.count)
	binary.BigEndian.PutUint64(data[8:], uint64(a.consumedAt))
	return agent.State{Version: 1, Bytes: data}, nil
}
func (a *harnessCounterAgent) Restore(ctx context.Context, state agent.State) error {
	if a.callbackTimeout == "restore" {
		<-ctx.Done()
		return ctx.Err()
	}
	if a.restoreFailure {
		return errors.New("injected restore failure")
	}
	if state.Version != 1 {
		return agent.ErrVersion
	}
	if len(state.Bytes) != 16 {
		return errors.New("invalid counter snapshot")
	}
	a.count = binary.BigEndian.Uint64(state.Bytes)
	a.consumedAt = int64(binary.BigEndian.Uint64(state.Bytes[8:]))
	return nil
}
func (a *harnessCounterAgent) Resume(ctx context.Context, notice agent.ResumeNotice) error {
	if a.callbackTimeout == "resume" {
		<-ctx.Done()
		return ctx.Err()
	}
	if a.callbackTimeout == "save" {
		a.resumeSave = true
		return nil
	}
	a.resumes <- agentResumeObservation{notice, a.count}
	return nil
}

type agentHarnessSystem struct {
	h       *Harness
	workers []*mediaworker.Worker
	plane   *controlplane.Plane
	store   sessionstore.Store
	resumes chan agentResumeObservation
}

type agentHarnessSettings struct {
	v2                    []agenthost.Registration
	targetEcho            bool
	remoteTarget          bool
	targetHandler         func(http.Handler) http.Handler
	snapshotInterval      time.Duration
	saveInterval          time.Duration
	targetRestoreFailure  bool
	targetCallbackTimeout string
	echo                  bool
	sourceProcessDelay    time.Duration
}

func startAgentHarness(t *testing.T, store sessionstore.Store, settings ...agentHarnessSettings) *agentHarnessSystem {
	t.Helper()
	if store == nil {
		store = sessionstore.NewMemory()
	}
	disable := workerprobe.Enable()
	t.Cleanup(disable)
	r, err := relay.New(relay.Config{Owners: store})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, r.Close()) })
	plane := controlplane.New(r, store)
	resumes := make(chan agentResumeObservation, 8)
	sys := &agentHarnessSystem{plane: plane, store: store, resumes: resumes}
	config := agentHarnessSettings{}
	if len(settings) > 0 {
		config = settings[0]
	}
	for i := range 2 {
		w, err := mediaworker.New(mediaworker.Config{SnapshotInterval: config.snapshotInterval, Agent: mediaworker.AgentConfig{SaveInterval: config.saveInterval, Factory: func(id string) agent.Agent {
			if i == 1 && config.targetEcho {
				return &agent.Echo{}
			}
			if len(config.v2) > 0 {
				return agenthost.Factory(config.v2[i])(id)
			}
			if config.echo {
				return &agent.Echo{}
			}
			a := &harnessCounterAgent{resumes: resumes, restoreFailure: i == 1 && config.targetRestoreFailure}
			if i == 1 {
				a.callbackTimeout = config.targetCallbackTimeout
			}
			if i == 0 {
				a.processDelay = config.sourceProcessDelay
			}
			return a
		}}, DisableFrameCache: true, Relay: &mediaworker.RelayConfig{Owners: store, Addr: r.WorkerAddr(), PublicAddr: r.PublicAddr()}})
		require.NoError(t, err)
		sys.workers = append(sys.workers, w)
		t.Cleanup(func() { require.NoError(t, w.Close()) })
		r.AddWorker(w.LocalAddr())
		var registered controlplane.Worker = w
		if i == 1 && config.remoteTarget {
			handler := w.PrivateHandler()
			if config.targetHandler != nil {
				handler = config.targetHandler(handler)
			}
			private := httptest.NewServer(handler)
			t.Cleanup(private.Close)
			registered = &controlplane.RemoteWorker{URL: private.URL, Client: private.Client()}
		}
		require.NoError(t, plane.Register(strconv.Itoa(i), w.LocalAddr(), registered))
		if i == 1 && config.remoteTarget {
			w.StartHeartbeats(plane)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = plane.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	server := httptest.NewServer(plane.Handler())
	t.Cleanup(server.Close)
	h, err := Start(Options{External: &ExternalTopology{SignalingURL: server.URL + "/calls", RelayAddr: r.PublicAddr()}})
	require.NoError(t, err)
	sys.h = h
	t.Cleanup(func() { require.NoError(t, h.Close()) })
	return sys
}

func forAgentStores(t *testing.T, scenario func(*testing.T, sessionstore.Store)) {
	t.Helper()
	t.Run("memory", func(t *testing.T) { scenario(t, sessionstore.NewMemory()) })
	t.Run("redis", func(t *testing.T) {
		addr := os.Getenv("RELAIS_TEST_REDIS_ADDR")
		if addr == "" {
			if os.Getenv("RELAIS_TEST_REDIS_REQUIRE") == "1" {
				t.Fatal("Redis required")
			}
			t.Skip("dedicated Redis not configured")
		}
		require.NoError(t, redisendpoint.ValidateDev(addr))
		token := make([]byte, 16)
		_, err := rand.Read(token)
		require.NoError(t, err)
		store, err := sessionstore.NewRedis(context.Background(), storage.RedisConfig{Addr: addr, Prefix: "agent-harness:" + hex.EncodeToString(token) + ":"}, make([]byte, 32))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, store.Close()) })
		scenario(t, store)
	})
}

type receivedAgentCounts struct {
	mu     sync.Mutex
	values []uint64
}

func observeAgentCounts(call *Call) *receivedAgentCounts {
	observed := &receivedAgentCounts{}
	call.pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		if track.Kind() != webrtc.RTPCodecTypeAudio {
			call.onTrack(track, nil)
			return
		}
		record := call.rec.addTrack(kindAudio, uint32(track.SSRC()), uint8(track.PayloadType()))
		call.startReader(func() {
			for {
				packet, _, err := track.ReadRTP()
				if err != nil {
					return
				}
				call.rec.packet(record, packet, time.Now())
				if len(packet.Payload) == 13 {
					observed.mu.Lock()
					observed.values = append(observed.values, binary.BigEndian.Uint64(packet.Payload[5:]))
					observed.mu.Unlock()
				}
			}
		})
	})
	return observed
}
func (o *receivedAgentCounts) snapshot() []uint64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]uint64(nil), o.values...)
}
func awaitAgentResume(t *testing.T, sys *agentHarnessSystem) agentResumeObservation {
	t.Helper()
	select {
	case notice := <-sys.resumes:
		return notice
	case <-time.After(3 * time.Second):
		t.Fatal("agent did not resume")
		return agentResumeObservation{}
	}
}

func TestAgentPlannedMoveExactContinuation(t *testing.T) {
	forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
		sys := startAgentHarness(t, store)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		call, err := sys.h.Dial(ctx, CallOptions{Video: true})
		require.NoError(t, err)
		counts := observeAgentCounts(call)
		require.NoError(t, call.SendMedia(ctx, 300*time.Millisecond))
		require.Eventually(t, func() bool { return len(counts.snapshot()) >= 15 }, time.Second, time.Millisecond)
		_, err = sys.plane.Move(ctx, call.SessionID(), "1")
		require.NoError(t, err)
		resumed := awaitAgentResume(t, sys)
		require.Equal(t, agent.PlannedMove, resumed.notice.Kind)
		require.Zero(t, resumed.notice.CheckpointAge)
		require.False(t, resumed.notice.InputMayBeDuplicated)
		require.Empty(t, resumed.notice.DuplicateWindows)
		require.Equal(t, resumed.count, resumed.notice.Progress.Consumed)
		require.EqualValues(t, 15, resumed.count)
		require.NoError(t, call.SendMedia(ctx, 300*time.Millisecond))
		require.Eventually(t, func() bool { return len(counts.snapshot()) >= 30 }, time.Second, time.Millisecond)
		values := counts.snapshot()
		for i, n := range values {
			require.EqualValues(t, i+1, n, "caller observes exact counter continuation")
		}
		state, progress, err := sys.workers[1].SessionAgent(call.SessionID())
		require.NoError(t, err)
		require.Equal(t, binary.BigEndian.Uint64(state.Bytes), progress.Consumed)
		report, err := call.Hangup(ctx)
		require.NoError(t, err)
		require.Zero(t, report.DecryptionFailures.Total())
		require.Zero(t, report.Renegotiations)
		require.Zero(t, report.ICERestarts)
		require.Len(t, report.Tracks, 2)
		require.Zero(t, sys.workers[0].AgentStats().InputDrops)
		require.Zero(t, sys.workers[1].AgentStats().InputDrops)
		t.Logf("AGENT_PLANNED restored=%d next=%d final=%d exact=true", resumed.count, values[15], values[len(values)-1])
	})
}

func TestAgentSlowQueueMoveExactContinuation(t *testing.T) {
	forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
		sys := startAgentHarness(t, store, agentHarnessSettings{sourceProcessDelay: 60 * time.Millisecond})
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		call, err := sys.h.Dial(ctx, CallOptions{})
		require.NoError(t, err)
		counts := observeAgentCounts(call)
		// At 50 inputs/sec and 60 ms per callback, this fills the default
		// 32-input queue. Stop sending before Move to isolate flush drops.
		require.NoError(t, call.SendMedia(ctx, 1400*time.Millisecond))
		before := sys.workers[0].AgentStats()
		require.Positive(t, before.InputDrops, "default queue overflowed before the move")
		_, err = sys.plane.Move(ctx, call.SessionID(), "1")
		require.NoError(t, err, "callbacks meeting their deadlines must not lose the call")
		resumed := awaitAgentResume(t, sys)
		require.Equal(t, agent.PlannedMove, resumed.notice.Kind)
		require.Equal(t, resumed.count, resumed.notice.Progress.Consumed)
		after := sys.workers[0].AgentStats()
		require.Greater(t, after.InputDrops, before.InputDrops, "flush budget drops the unconsumed queue")
		require.Zero(t, after.CallbackDeadlines)
		state, progress, err := sys.workers[1].SessionAgent(call.SessionID())
		require.NoError(t, err)
		require.Equal(t, resumed.count, binary.BigEndian.Uint64(state.Bytes))
		require.Equal(t, resumed.notice.Progress, progress, "move preserves the exact consumed pair")
		require.NoError(t, call.SendMedia(ctx, 100*time.Millisecond))
		require.Eventually(t, func() bool {
			values := counts.snapshot()
			return len(values) > 0 && values[len(values)-1] >= resumed.count+5
		}, time.Second, time.Millisecond)
		values := counts.snapshot()
		for i, n := range values {
			require.EqualValues(t, i+1, n, "caller receives exact state continuation, including the first resumed output")
		}
		require.Contains(t, values, resumed.count+1)
		report, err := call.Hangup(ctx)
		require.NoError(t, err)
		require.Zero(t, report.DecryptionFailures.Total())
		t.Logf("AGENT_SLOW_MOVE restored=%d flush_drops=%d deadlines=%d next=%d", resumed.count, after.InputDrops-before.InputDrops, after.CallbackDeadlines, resumed.count+1)
	})
}

func TestAgentTakeoverCheckpointContinuation(t *testing.T) {
	forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
		sys := startAgentHarness(t, store)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		call, err := sys.h.Dial(ctx, CallOptions{})
		require.NoError(t, err)
		counts := observeAgentCounts(call)
		sent := make(chan error, 1)
		go func() { sent <- call.SendMedia(ctx, 1800*time.Millisecond) }()
		require.Eventually(t, func() bool { return len(counts.snapshot()) >= 10 }, time.Second, time.Millisecond)
		ready := make(chan struct{})
		var once sync.Once
		require.NoError(t, workerprobe.SetBeforeSnapshot(sys.workers[0].LocalAddr(), func(lifetime context.Context, id string) {
			if id == call.SessionID() {
				once.Do(func() { close(ready) })
				<-lifetime.Done()
			}
		}))
		<-ready
		data, err := store.GetState(ctx, call.SessionID())
		require.NoError(t, err)
		var snap struct {
			State struct {
				Agent struct {
					State    agent.State
					Progress agent.Progress
				}
			}
		}
		require.NoError(t, json.Unmarshal(data, &snap))
		saved := binary.BigEndian.Uint64(snap.State.Agent.State.Bytes)
		require.Equal(t, saved, snap.State.Agent.Progress.Consumed, "saved state and input progress are one checkpoint")
		require.Eventually(t, func() bool {
			_, p, err := sys.workers[0].SessionAgent(call.SessionID())
			return err == nil && p.Consumed > saved+3
		}, time.Second, time.Millisecond)
		beforeCrash := counts.snapshot()
		require.NoError(t, workerprobe.Kill(sys.workers[0].LocalAddr()))
		resumed := awaitAgentResume(t, sys)
		require.Equal(t, agent.Takeover, resumed.notice.Kind)
		require.Equal(t, saved, resumed.count)
		require.Equal(t, snap.State.Agent.Progress, resumed.notice.Progress)
		require.Greater(t, resumed.notice.CheckpointAge, 100*time.Millisecond)
		require.GreaterOrEqual(t, resumed.notice.SnapshotAge, resumed.notice.CheckpointAge)
		require.False(t, resumed.notice.InputMayBeDuplicated, "#27 replay is not installed")
		require.Empty(t, resumed.notice.DuplicateWindows)
		// The input state was current at the actual checkpoint copy, not at some
		// earlier coalesced application publication. Include one audio frame of
		// idle time plus measured copy-to-commit delay and local scheduling slack.
		info, err := mediaworker.SnapshotCheckpoint(data)
		require.NoError(t, err)
		lastConsumed := time.Unix(0, int64(binary.BigEndian.Uint64(snap.State.Agent.State.Bytes[8:])))
		require.Less(t, info.CapturedAt.Sub(lastConsumed), 50*time.Millisecond)
		require.NoError(t, <-sent)
		values := counts.snapshot()
		require.Greater(t, len(values), len(beforeCrash)+10)
		firstResumed := -1
		for i := 1; i < len(values); i++ {
			if values[i] <= values[i-1] {
				firstResumed = i
				break
			}
		}
		require.Positive(t, firstResumed, "old owner must advance past its checkpoint before crash")
		require.Equal(t, saved+1, values[firstResumed], "first resumed caller payload follows saved count")
		for i := firstResumed + 1; i < len(values); i++ {
			require.Equal(t, values[i-1]+1, values[i])
		}
		require.Eventually(t, func() bool { status, err := sys.plane.Status(ctx); return err == nil && len(status.Takeovers) == 1 }, time.Second, time.Millisecond)
		status, err := sys.plane.Status(ctx)
		require.NoError(t, err)
		require.Equal(t, status.Takeovers[0].CheckpointAge, resumed.notice.CheckpointAge)
		require.Equal(t, status.Takeovers[0].SnapshotAge, resumed.notice.SnapshotAge)
		report, err := call.Hangup(ctx)
		require.NoError(t, err)
		require.Zero(t, report.DecryptionFailures.Total())
		require.Zero(t, report.Renegotiations)
		require.Zero(t, report.ICERestarts)
		t.Logf("AGENT_TAKEOVER checkpoint_count=%d old_count=%d next=%d checkpoint_age=%s snapshot_age=%s duplicated=false", saved, values[firstResumed-1], values[firstResumed], resumed.notice.CheckpointAge, resumed.notice.SnapshotAge)
	})
}

type failedAgentStore struct {
	sessionstore.Store
	fail     atomic.Bool
	failures atomic.Uint64
}

func (s *failedAgentStore) PutState(ctx context.Context, lease sessionstore.Lease, data []byte) error {
	var snap struct {
		State struct{ Agent struct{ State agent.State } }
	}
	if err := json.Unmarshal(data, &snap); err != nil {
		return err
	}
	if s.fail.Load() && len(snap.State.Agent.State.Bytes) == 16 && binary.BigEndian.Uint64(snap.State.Agent.State.Bytes) > 0 {
		s.failures.Add(1)
		return errors.New("injected durable agent save failure")
	}
	return s.Store.PutState(ctx, lease, data)
}
func TestAgentStoreFailureRetriesWithoutEndingCall(t *testing.T) {
	forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
		flaky := &failedAgentStore{Store: store}
		sys := startAgentHarness(t, flaky)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		call, err := sys.h.Dial(ctx, CallOptions{})
		require.NoError(t, err)
		require.NoError(t, call.SendMedia(ctx, 200*time.Millisecond))
		require.Eventually(t, func() bool { _, err := store.GetState(ctx, call.SessionID()); return err == nil }, time.Second, time.Millisecond)
		baseline, err := store.GetState(ctx, call.SessionID())
		require.NoError(t, err)
		flaky.fail.Store(true)
		require.NoError(t, call.SendMedia(ctx, 300*time.Millisecond))
		require.GreaterOrEqual(t, flaky.failures.Load(), uint64(2))
		require.Equal(t, 1, sys.workers[0].SessionCount())
		require.Zero(t, sys.workers[0].AgentStats().SaveFailures)
		unchanged, err := store.GetState(ctx, call.SessionID())
		require.NoError(t, err)
		require.Equal(t, baseline, unchanged)
		_, err = store.Get(ctx, call.SessionID())
		require.NoError(t, err)
		flaky.fail.Store(false)
		require.Eventually(t, func() bool {
			data, err := store.GetState(ctx, call.SessionID())
			return err == nil && !bytes.Equal(data, baseline)
		}, time.Second, time.Millisecond)
		_, err = call.Hangup(ctx)
		require.NoError(t, err)
	})
}

type agentWriteSpy struct {
	sessionstore.Store
	mu            sync.Mutex
	changedWrites []time.Time
	lastCount     uint64
	writes        []agentWriteObservation
}

type agentWriteObservation struct {
	count    uint64
	started  time.Time
	finished time.Time
	err      error
}

func (s *agentWriteSpy) PutState(ctx context.Context, lease sessionstore.Lease, data []byte) error {
	var snap struct {
		State struct {
			Agent struct {
				State    agent.State
				Progress agent.Progress
			}
		}
	}
	if err := json.Unmarshal(data, &snap); err != nil {
		return err
	}
	count := binary.BigEndian.Uint64(snap.State.Agent.State.Bytes)
	if count != snap.State.Agent.Progress.Consumed {
		return errors.New("state/progress pair disagrees")
	}
	started := time.Now()
	err := s.Store.PutState(ctx, lease, data)
	finished := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes = append(s.writes, agentWriteObservation{count, started, finished, err})
	if err != nil {
		return err
	}
	if count != s.lastCount {
		s.changedWrites = append(s.changedWrites, time.Now())
		s.lastCount = count
	}
	return nil
}
func TestAgentSaveRateAndAtomicProgress(t *testing.T) {
	forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
		spy := &agentWriteSpy{Store: store}
		sys := startAgentHarness(t, spy, agentHarnessSettings{snapshotInterval: 5 * time.Millisecond})
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		call, err := sys.h.Dial(ctx, CallOptions{})
		require.NoError(t, err)
		started := time.Now()
		var inputStopped time.Time
		defer func() {
			if !t.Failed() {
				return
			}
			_, live, liveErr := sys.workers[0].SessionAgent(call.SessionID())
			call.rec.mu.Lock()
			sent := call.rec.sentAudio
			call.rec.mu.Unlock()
			spy.mu.Lock()
			defer spy.mu.Unlock()
			t.Logf("AGENT_SAVE_DIAGNOSTIC sent=%+v live=%+v live_error=%v saved=%d stats=%+v input_stopped=%s (before hangup/flush)", sent, live, liveErr, spy.lastCount, sys.workers[0].AgentStats(), inputStopped.Sub(started))
			for _, write := range spy.writes {
				t.Logf("AGENT_SAVE_DIAGNOSTIC count=%d start=%s finish=%s store_latency=%s error=%v", write.count, write.started.Sub(started), write.finished.Sub(started), write.finished.Sub(write.started), write.err)
			}
		}()
		require.NoError(t, call.SendMedia(ctx, 700*time.Millisecond))
		inputStopped = time.Now()
		// SendMedia is paced by wall time. Missed ticks under load can send
		// fewer than 35 frames; every frame actually sent must still be consumed
		// and durably saved before hangup, without bypassing the save rate.
		call.rec.mu.Lock()
		sent := uint64(call.rec.sentAudio.Frames)
		call.rec.mu.Unlock()
		require.Eventually(t, func() bool {
			_, progress, err := sys.workers[0].SessionAgent(call.SessionID())
			return err == nil && progress.Consumed == sent
		}, time.Second, time.Millisecond, "agent must consume every sent frame")
		require.Zero(t, sys.workers[0].AgentStats().InputDrops)
		require.Eventually(t, func() bool { spy.mu.Lock(); defer spy.mu.Unlock(); return spy.lastCount == sent }, time.Second, time.Millisecond,
			"final consumed progress must reach the store while input is idle")
		spy.mu.Lock()
		writes := append([]time.Time(nil), spy.changedWrites...)
		spy.mu.Unlock()
		require.GreaterOrEqual(t, len(writes), 5)
		for i := 1; i < len(writes); i++ {
			require.GreaterOrEqual(t, writes[i].Sub(writes[i-1]), 100*time.Millisecond, "5 ms media checkpoints cannot bypass 100 ms application save rate")
		}
		state, p, err := sys.workers[0].SessionAgent(call.SessionID())
		require.NoError(t, err)
		require.EqualValues(t, sent, p.Consumed)
		require.EqualValues(t, p.Consumed, binary.BigEndian.Uint64(state.Bytes))
		_, err = call.Hangup(ctx)
		require.NoError(t, err)
		t.Logf("AGENT_SAVE_RATE consumed=%d changed_writes=%d minimum_interval=100ms transport_interval=5ms", sent, len(writes))
	})
}

func TestAgentInitialCheckpointSkipKeepsCall(t *testing.T) {
	forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
		spy := &agentWriteSpy{Store: store}
		// Keep the changed pair rate limited throughout the startup gate.
		sys := startAgentHarness(t, spy, agentHarnessSettings{snapshotInterval: 5 * time.Millisecond, saveInterval: time.Hour})
		entered, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		unblock := func() { once.Do(func() { close(release) }) }
		defer unblock()
		require.NoError(t, workerprobe.SetBeforeInitialSnapshot(sys.workers[0].LocalAddr(), func(ctx context.Context, _ string) {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
			}
		}))
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		call, err := sys.h.Dial(ctx, CallOptions{})
		require.NoError(t, err)
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("connect did not reach initial checkpoint gate")
		}
		// The background loop stores the handshake before connect's own put.
		require.Eventually(t, func() bool {
			spy.mu.Lock()
			defer spy.mu.Unlock()
			return len(spy.writes) > 0 && spy.writes[0].err == nil
		}, time.Second, time.Millisecond)
		require.NoError(t, call.SendMedia(ctx, 20*time.Millisecond))
		require.Eventually(t, func() bool {
			_, progress, err := sys.workers[0].SessionAgent(call.SessionID())
			return err == nil && progress.Consumed > 0
		}, time.Second, time.Millisecond)
		unblock()
		require.Never(t, func() bool { return sys.workers[0].SessionCount() == 0 }, 200*time.Millisecond, time.Millisecond,
			"a rate-limited initial checkpoint must not close an established call with a durable background checkpoint")
		require.NoError(t, call.SendMedia(ctx, 20*time.Millisecond))
		require.Eventually(t, func() bool {
			_, progress, err := sys.workers[0].SessionAgent(call.SessionID())
			return err == nil && progress.Consumed >= 2
		}, time.Second, time.Millisecond)
	})
}

func TestAgentCallbackTimeoutRollsBackPlannedCall(t *testing.T) {
	for _, callback := range []string{"restore", "resume", "save"} {
		t.Run(callback, func(t *testing.T) {
			forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
				sys := startAgentHarness(t, store, agentHarnessSettings{targetCallbackTimeout: callback})
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				call, err := sys.h.Dial(ctx, CallOptions{})
				require.NoError(t, err)
				counts := observeAgentCounts(call)
				require.NoError(t, call.SendMedia(ctx, 200*time.Millisecond))
				require.Eventually(t, func() bool { return len(counts.snapshot()) >= 10 }, time.Second, time.Millisecond)

				result, err := sys.plane.Move(ctx, call.SessionID(), "1")
				if callback == "save" {
					require.ErrorIs(t, err, agent.ErrSave)
					require.EqualValues(t, 1, sys.workers[1].AgentStats().SaveFailures)
				} else {
					require.ErrorIs(t, err, agent.ErrRestore)
					require.EqualValues(t, 1, sys.workers[1].AgentStats().RestoreFailures)
				}
				require.ErrorIs(t, err, context.DeadlineExceeded)
				require.True(t, result.Result.RolledBack)
				require.False(t, result.Lost)
				require.Equal(t, 1, sys.workers[0].SessionCount())
				require.Zero(t, sys.workers[1].SessionCount())
				require.EqualValues(t, 1, sys.workers[1].AgentStats().CallbackDeadlines)
				resumed := awaitAgentResume(t, sys)
				require.Equal(t, agent.PlannedMove, resumed.notice.Kind)
				require.EqualValues(t, 10, resumed.count)
				require.Equal(t, resumed.count, resumed.notice.Progress.Consumed)
				require.NoError(t, call.SendMedia(ctx, 200*time.Millisecond))
				require.Eventually(t, func() bool { return len(counts.snapshot()) >= 20 }, time.Second, time.Millisecond)
				for i, n := range counts.snapshot() {
					require.EqualValues(t, i+1, n, "rollback preserves the final exported agent state")
				}
				report, err := call.Hangup(ctx)
				require.NoError(t, err)
				require.Zero(t, report.DecryptionFailures.Total())
				require.Len(t, report.Tracks, 1)
				require.Zero(t, report.Tracks[0].SequenceDiscontinuities, "planned rollback spends no sequence margin")
			})
		})
	}
}

func TestAgentFailedRestoreRollsBackPlannedCall(t *testing.T) {
	forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
		sys := startAgentHarness(t, store, agentHarnessSettings{targetRestoreFailure: true})
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		call, err := sys.h.Dial(ctx, CallOptions{})
		require.NoError(t, err)
		require.NoError(t, call.SendMedia(ctx, 200*time.Millisecond))
		_, err = sys.plane.Move(ctx, call.SessionID(), "1")
		require.Error(t, err)
		require.Equal(t, 1, sys.workers[0].SessionCount())
		require.Zero(t, sys.workers[1].SessionCount())
		require.EqualValues(t, 1, sys.workers[1].AgentStats().RestoreFailures)
		_, err = store.Get(ctx, call.SessionID())
		require.NoError(t, err)
		_, err = store.GetState(ctx, call.SessionID())
		require.NoError(t, err)
		require.Equal(t, agent.PlannedMove, awaitAgentResume(t, sys).notice.Kind, "source rolls back; failed target never calls Resume")
		require.NoError(t, call.SendMedia(ctx, 200*time.Millisecond))
		_, err = call.Hangup(ctx)
		require.NoError(t, err)
	})
}

// Fail only the first target checkpoint. The source export remains available
// to the real control-plane rollback path, on both fenced stores.
type resumeWriteFailureStore struct {
	sessionstore.Store
	mu     sync.Mutex
	target netip.AddrPort
	cause  error
}

func (s *resumeWriteFailureStore) PutState(ctx context.Context, lease sessionstore.Lease, data []byte) error {
	s.mu.Lock()
	if lease.Worker == s.target && s.cause != nil {
		err := s.cause
		s.cause = nil
		s.mu.Unlock()
		return err
	}
	s.mu.Unlock()
	return s.Store.PutState(ctx, lease, data)
}
func TestAgentResumeCheckpointFailureRollsBack(t *testing.T) {
	for _, echo := range []bool{true, false} {
		name := "custom"
		if echo {
			name = "echo"
		}
		t.Run(name, func(t *testing.T) {
			forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
				flaky := &resumeWriteFailureStore{Store: store}
				sys := startAgentHarness(t, flaky, agentHarnessSettings{echo: echo})
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				call, err := sys.h.Dial(ctx, CallOptions{})
				require.NoError(t, err)
				require.NoError(t, call.SendMedia(ctx, 200*time.Millisecond))
				flaky.mu.Lock()
				flaky.target = sys.workers[1].LocalAddr()
				flaky.cause = errors.New("injected target checkpoint failure")
				flaky.mu.Unlock()
				result, err := sys.plane.Move(ctx, call.SessionID(), "1")
				require.ErrorContains(t, err, "rolled back")
				require.True(t, result.Result.RolledBack)
				require.Equal(t, 1, sys.workers[0].SessionCount())
				require.Zero(t, sys.workers[1].SessionCount())
				lease, err := store.Get(ctx, call.SessionID())
				require.NoError(t, err)
				require.Equal(t, sys.workers[0].LocalAddr(), lease.Worker)
				_, err = store.GetState(ctx, call.SessionID())
				require.NoError(t, err)
				require.Zero(t, sys.workers[1].AgentStats().SaveFailures)
				require.NoError(t, call.SendMedia(ctx, 200*time.Millisecond))
				report, err := call.Hangup(ctx)
				require.NoError(t, err)
				require.Zero(t, report.DecryptionFailures.Total())
			})
		})
	}
}
