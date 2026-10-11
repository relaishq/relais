package callharness

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"net/netip"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/relais/internal/redisendpoint"
	"github.com/relais/internal/workerprobe"
	"github.com/relais/pkg/agent"
	"github.com/relais/pkg/controlplane"
	"github.com/relais/pkg/mediaworker"
	"github.com/relais/pkg/relay"
	"github.com/relais/pkg/sessionstore"
	"github.com/relais/pkg/storage"
	"github.com/stretchr/testify/require"
)

type agentResumeObservation struct {
	notice agent.ResumeNotice
	count  uint64
}
type harnessCounterAgent struct {
	count          uint64
	consumedAt     int64
	resumes        chan agentResumeObservation
	restoreFailure bool
	processDelay   time.Duration
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
func (a *harnessCounterAgent) Save(context.Context) (agent.State, error) {
	data := make([]byte, 16)
	binary.BigEndian.PutUint64(data, a.count)
	binary.BigEndian.PutUint64(data[8:], uint64(a.consumedAt))
	return agent.State{Version: 1, Bytes: data}, nil
}
func (a *harnessCounterAgent) Restore(_ context.Context, state agent.State) error {
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
func (a *harnessCounterAgent) Resume(_ context.Context, notice agent.ResumeNotice) error {
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
	snapshotInterval     time.Duration
	targetRestoreFailure bool
	echo                 bool
	sourceProcessDelay   time.Duration
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
		w, err := mediaworker.New(mediaworker.Config{SnapshotInterval: config.snapshotInterval, Agent: mediaworker.AgentConfig{Factory: func(string) agent.Agent {
			if config.echo {
				return &agent.Echo{}
			}
			a := &harnessCounterAgent{resumes: resumes, restoreFailure: i == 1 && config.targetRestoreFailure}
			if i == 0 {
				a.processDelay = config.sourceProcessDelay
			}
			return a
		}}, DisableFrameCache: true, Relay: &mediaworker.RelayConfig{Owners: store, Addr: r.WorkerAddr(), PublicAddr: r.PublicAddr()}})
		require.NoError(t, err)
		sys.workers = append(sys.workers, w)
		t.Cleanup(func() { require.NoError(t, w.Close()) })
		r.AddWorker(w.LocalAddr())
		require.NoError(t, plane.Register(strconv.Itoa(i), w.LocalAddr(), w))
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
		require.NoError(t, redisendpoint.Validate(addr))
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
	if err := s.Store.PutState(ctx, lease, data); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
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
		require.NoError(t, call.SendMedia(ctx, 700*time.Millisecond))
		require.Eventually(t, func() bool { spy.mu.Lock(); defer spy.mu.Unlock(); return spy.lastCount == 35 }, time.Second, time.Millisecond)
		spy.mu.Lock()
		writes := append([]time.Time(nil), spy.changedWrites...)
		spy.mu.Unlock()
		require.GreaterOrEqual(t, len(writes), 5)
		for i := 1; i < len(writes); i++ {
			require.GreaterOrEqual(t, writes[i].Sub(writes[i-1]), 100*time.Millisecond, "5 ms media checkpoints cannot bypass 100 ms application save rate")
		}
		state, p, err := sys.workers[0].SessionAgent(call.SessionID())
		require.NoError(t, err)
		require.EqualValues(t, 35, p.Consumed)
		require.EqualValues(t, p.Consumed, binary.BigEndian.Uint64(state.Bytes))
		_, err = call.Hangup(ctx)
		require.NoError(t, err)
		t.Logf("AGENT_SAVE_RATE consumed=35 changed_writes=%d minimum_interval=100ms transport_interval=5ms", len(writes))
	})
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
