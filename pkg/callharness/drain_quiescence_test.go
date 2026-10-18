package callharness

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/privateapi"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

func TestV2DrainConcurrentHandlersAndQueuedStarts(t *testing.T) {
	forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
		count := 20
		if testing.Short() {
			count = 2
		}
		gate := make(chan struct{})
		var sourceStarts atomic.Int32
		launches := make(chan struct{}, count)
		var handlers [2]agent.TaskHandler
		handlers[0] = func(ctx context.Context, run *agent.TaskRun) error {
			sourceStarts.Add(1)
			launches <- struct{}{}
			select {
			case <-gate:
			case <-ctx.Done():
				return ctx.Err()
			}
			return run.Emit(ctx, run.After()+1, agent.Chunk{Data: run.Spec().Request})
		}
		handlers[1] = func(ctx context.Context, run *agent.TaskRun) error {
			return run.Emit(ctx, run.After()+1, agent.Chunk{Data: run.Spec().Request})
		}
		specs := []agent.TaskSpec{
			{Step: "running", Kind: "test/v1", Class: agent.ReadOnly, Resume: agent.Abandon, Request: []byte("running request")},
			{Step: "queued", Kind: "test/v1", Class: agent.ReadOnly, Resume: agent.Abandon, Request: []byte("never launched request")},
		}
		regs, _ := taskRegistrations(specs, handlers, false)
		for i := range regs {
			regs[i].Bounds.MaxActiveTasks = 1
			regs[i].Factory = func(agent.FactoryArgs) agent.Agent {
				return &taskHarnessAgent{specs: specs, results: make(chan agent.TaskResult, 16)}
			}
		}
		sys := startAgentHarness(t, store, agentHarnessSettings{v2: regs, remoteTarget: true, callbackBudget: time.Second})
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		calls := make([]*Call, count)
		for i := range calls {
			var err error
			calls[i], err = sys.h.Dial(ctx, CallOptions{})
			require.NoError(t, err)
			select {
			case <-launches:
			case <-ctx.Done():
				t.Fatal("missing handler")
			}
		}
		require.EqualValues(t, count, sourceStarts.Load())
		require.NoError(t, sys.plane.MarkDraining("0"))
		sys.workers[0].BeginDrain()
		timer := time.AfterFunc(300*time.Millisecond, func() { close(gate) })
		defer timer.Stop()
		start := time.Now()
		waitCtx, stop := context.WithTimeout(ctx, 2*time.Second)
		require.NoError(t, sys.workers[0].QuiesceTasks(waitCtx))
		stop()
		elapsed := time.Since(start)
		require.GreaterOrEqual(t, elapsed, 280*time.Millisecond)
		require.Less(t, elapsed, 1500*time.Millisecond, "handlers wait concurrently")
		require.EqualValues(t, count, sourceStarts.Load(), "queued starts must remain never_launched")
		moves, err := sys.plane.Drain(ctx, "0")
		require.NoError(t, err)
		require.Len(t, moves, count)
		for _, call := range calls {
			require.Eventually(t, func() bool {
				state, _, err := sys.workers[1].SessionAgent(call.SessionID())
				if err != nil {
					return false
				}
				var saved taskHarnessState
				return json.Unmarshal(state.Bytes, &saved) == nil && len(saved.Results) == 4
			}, 3*time.Second, 5*time.Millisecond)
			state, _, err := sys.workers[1].SessionAgent(call.SessionID())
			require.NoError(t, err)
			var saved taskHarnessState
			require.NoError(t, json.Unmarshal(state.Bytes, &saved))
			for _, r := range saved.Results {
				require.Equal(t, agent.OK, r.Outcome)
			}
			_, err = call.Hangup(ctx)
			require.NoError(t, err)
		}
		t.Logf("QUIESCENCE sessions=%d wait=%s exact=true queued_source_launches=0", count, elapsed)
	})
}

func TestV2DrainBudgetUsesUrgentTypedOutcome(t *testing.T) {
	forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
		launches := make(chan taskLaunch, 4)
		specs := []agent.TaskSpec{{Step: "unfinished", Kind: "test/v1", Class: agent.SideEffecting, Resume: agent.Abandon, Request: []byte("effect")}, {Step: "queued", Kind: "test/v1", Class: agent.ReadOnly, Resume: agent.Abandon, Request: []byte("exact queued payload")}}
		handlers := [2]agent.TaskHandler{
			func(ctx context.Context, run *agent.TaskRun) error {
				launches <- taskLaunch{id: run.ID(), request: run.Spec().Request}
				<-ctx.Done()
				return ctx.Err()
			},
			func(ctx context.Context, run *agent.TaskRun) error {
				require.Equal(t, specs[1].Request, run.Spec().Request)
				return run.Emit(ctx, 1, agent.Chunk{Data: []byte("moved")})
			},
		}
		regs, results := taskRegistrations(specs, handlers, false)
		for i := range regs {
			regs[i].Bounds.MaxActiveTasks = 1
		}
		sys := startAgentHarness(t, store, agentHarnessSettings{v2: regs, remoteTarget: true, callbackBudget: time.Second})
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		call, err := sys.h.Dial(ctx, CallOptions{})
		require.NoError(t, err)
		takeTaskLaunch(t, launches)
		wait, stop := context.WithTimeout(ctx, 80*time.Millisecond)
		require.ErrorIs(t, sys.workers[0].QuiesceTasks(wait), context.DeadlineExceeded)
		stop()
		_, err = sys.plane.Drain(ctx, "0")
		require.NoError(t, err)
		first := takeTaskFinal(t, results[1])
		require.Equal(t, agent.OutcomeUnknown, first[len(first)-1].Outcome)
		second := takeTaskFinal(t, results[1])
		require.Equal(t, agent.OK, second[len(second)-1].Outcome)
		require.Equal(t, "moved", string(second[0].Chunk.Data))
		require.Empty(t, launches)
		_, err = call.Hangup(ctx)
		require.NoError(t, err)
	})
}

func TestV2DrainDoesNotWaitForBufferedPlayback(t *testing.T) {
	forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
		regs, _, gate, _ := pacerRegistrations(t, 1, false, false)
		for i := range regs {
			regs[i].Tasks["tts/v1"] = func(ctx context.Context, run *agent.TaskRun) error {
				select {
				case <-gate:
				case <-ctx.Done():
					return ctx.Err()
				}
				audio := make([][]byte, 500)
				for i := range audio {
					audio[i] = []byte{0xf8, 0xff, 0xfe}
				}
				return run.Emit(ctx, 1, agent.Chunk{Audio: &agent.EncodedAudio{Format: "audio/opus", Packets: audio}})
			}
		}
		sys := startAgentHarness(t, store, agentHarnessSettings{v2: regs, remoteTarget: true, callbackBudget: time.Second})
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		call, err := sys.h.Dial(ctx, CallOptions{})
		require.NoError(t, err)
		packets := receivePacer(call)
		close(gate)
		nextPaced(t, packets)
		wait, stop := context.WithTimeout(ctx, time.Second)
		defer stop()
		start := time.Now()
		require.NoError(t, sys.workers[0].QuiesceTasks(wait))
		waitDuration := time.Since(start)
		require.Less(t, waitDuration, 500*time.Millisecond)
		before, err := drainAgentStatus(sys.workers[0], call.SessionID())
		require.NoError(t, err)
		require.Greater(t, before.PlaybackPackets, 450, "ten seconds of reply remains buffered")
		_, err = sys.plane.Drain(ctx, "0")
		require.NoError(t, err)
		after, err := drainAgentStatus(sys.workers[1], call.SessionID())
		require.NoError(t, err)
		require.Greater(t, after.PlaybackPackets, 400, "backlog moves rather than draining")
		nextPaced(t, packets)
		_, err = call.Hangup(ctx)
		require.NoError(t, err)
		t.Logf("PLAYBACK moved_pending=%d before=%d wait=%s", after.PlaybackPackets, before.PlaybackPackets, waitDuration)
	})
}

type drainStallStore struct {
	sessionstore.Store
	stalled atomic.Bool
	entered chan struct{}
}

func (s *drainStallStore) PutState(ctx context.Context, l sessionstore.Lease, b []byte) error {
	if s.stalled.Load() {
		select {
		case s.entered <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return ctx.Err()
	}
	return s.Store.PutState(ctx, l, b)
}
func TestV2DrainQuiescenceStoreStall(t *testing.T) {
	forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
		gate := &drainStallStore{Store: store, entered: make(chan struct{}, 16)}
		started := make(chan struct{}, 1)
		specs := []agent.TaskSpec{{Step: "running", Kind: "test/v1", Class: agent.ReadOnly, Resume: agent.Abandon}}
		handlers := [2]agent.TaskHandler{func(ctx context.Context, _ *agent.TaskRun) error {
			started <- struct{}{}
			<-ctx.Done()
			return ctx.Err()
		}, func(context.Context, *agent.TaskRun) error { return nil }}
		regs, _ := taskRegistrations(specs, handlers, false)
		sys := startAgentHarness(t, gate, agentHarnessSettings{v2: regs, callbackBudget: time.Second})
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		defer cancel()
		call, err := sys.h.Dial(ctx, CallOptions{})
		require.NoError(t, err)
		<-started
		gate.stalled.Store(true)
		require.NoError(t, call.SendMedia(ctx, 40*time.Millisecond))
		select {
		case <-gate.entered:
		case <-ctx.Done():
			t.Fatal("store did not stall")
		}
		start := time.Now()
		wait, stop := context.WithTimeout(ctx, 100*time.Millisecond)
		require.ErrorIs(t, sys.workers[0].QuiesceTasks(wait), context.DeadlineExceeded)
		stop()
		drainCtx, stopDrain := context.WithTimeout(ctx, 350*time.Millisecond)
		server := httptest.NewServer(sys.plane.Handler())
		defer server.Close()
		err = privateapi.Do(drainCtx, server.Client(), server.URL, http.MethodPost, "/workers/0/drain", nil, nil, nil)
		stopDrain()
		require.Error(t, err)
		require.Less(t, time.Since(start), time.Second, "store I/O must not hold the quiescence or export fence")
		gate.stalled.Store(false)
		t.Logf("STORE_STALL drain bounded=%s err=%v", time.Since(start), err)
	})
}

func drainAgentStatus(w *mediaworker.Worker, id string) (*mediaworker.AgentSessionStatus, error) {
	rw := httptest.NewRecorder()
	w.PrivateHandler().ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/status?agent-session="+id, nil))
	var status mediaworker.WorkerStatus
	err := json.Unmarshal(rw.Body.Bytes(), &status)
	return status.AgentSession, err
}

type drainClaimStore struct {
	sessionstore.Store
	entered, release chan struct{}
	claimed          atomic.Bool
	releaseFailure   atomic.Bool
}

func (s *drainClaimStore) Claim(ctx context.Context, id string, worker netip.AddrPort, ttl time.Duration) (sessionstore.Lease, error) {
	lease, err := s.Store.Claim(ctx, id, worker, ttl)
	if err != nil {
		return lease, err
	}
	if !s.claimed.CompareAndSwap(false, true) {
		return lease, nil
	}
	close(s.entered)
	select {
	case <-s.release:
		return lease, nil
	case <-ctx.Done():
		return lease, ctx.Err()
	}
}
func (s *drainClaimStore) Release(ctx context.Context, lease sessionstore.Lease) error {
	if s.releaseFailure.Load() {
		return sessionstore.ErrTransient
	}
	return s.Store.Release(ctx, lease)
}
func TestV2DrainClaimCleanupFailureDoesNotRetry(t *testing.T) {
	forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
		gate := &drainClaimStore{Store: store, entered: make(chan struct{}), release: make(chan struct{})}
		sys := startAgentHarness(t, gate)
		t.Cleanup(func() {
			gate.releaseFailure.Store(false)
			leases, err := store.ListByWorker(context.Background(), sys.workers[0].LocalAddr())
			if err == nil {
				for _, lease := range leases {
					_ = store.Release(context.Background(), lease)
				}
			}
		})
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		done := make(chan error, 1)
		go func() { _, err := sys.h.Dial(ctx, CallOptions{}); done <- err }()
		select {
		case <-gate.entered:
		case <-ctx.Done():
			t.Fatal("create never claimed")
		}
		require.NoError(t, sys.plane.MarkDraining("0"))
		sys.workers[0].BeginDrain()
		gate.releaseFailure.Store(true)
		close(gate.release)
		require.ErrorContains(t, <-done, "claim cleanup")
		require.Zero(t, sys.workers[0].SessionCount())
		require.Zero(t, sys.workers[1].SessionCount())
		status, err := sys.plane.Status(ctx)
		require.NoError(t, err)
		require.Empty(t, status.Calls)
		leases, err := store.ListByWorker(ctx, sys.workers[0].LocalAddr())
		require.NoError(t, err)
		require.Len(t, leases, 1, "cleanup failure must remain uncertain rather than permission to retry")
	})
}

func TestV2DrainRacedCreateReplacedOnce(t *testing.T) {
	forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
		gate := &drainClaimStore{Store: store, entered: make(chan struct{}), release: make(chan struct{})}
		sys := startAgentHarness(t, gate)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		type dialResult struct {
			call *Call
			err  error
		}
		done := make(chan dialResult, 1)
		go func() { call, err := sys.h.Dial(ctx, CallOptions{}); done <- dialResult{call, err} }()
		select {
		case <-gate.entered:
		case <-ctx.Done():
			t.Fatal("create never claimed")
		}
		require.NoError(t, sys.plane.MarkDraining("0"))
		sys.workers[0].BeginDrain()
		close(gate.release)
		result := <-done
		require.NoError(t, result.err)
		require.Equal(t, 1, sys.workers[1].SessionCount())
		require.Zero(t, sys.workers[0].SessionCount())
		status, err := sys.plane.Status(ctx)
		require.NoError(t, err)
		require.Len(t, status.Calls, 1)
		require.Equal(t, "1", status.Calls[0].Owner)
		leases, err := store.ListByWorker(ctx, sys.workers[0].LocalAddr())
		require.NoError(t, err)
		require.Empty(t, leases)
		_, err = result.call.Hangup(ctx)
		require.NoError(t, err)
	})
}

func TestV2DrainWaitsForCanceledHandlerReturn(t *testing.T) {
	forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
		entered, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		defer once.Do(func() { close(release) })
		handlers := [2]agent.TaskHandler{func(context.Context, *agent.TaskRun) error {
			close(entered)
			<-release // Deliberately outlive attempt cancellation.
			return nil
		}, func(context.Context, *agent.TaskRun) error { return nil }}
		specs := []agent.TaskSpec{{Step: "outliving", Kind: "test/v1", Class: agent.ReadOnly, Resume: agent.Abandon, Timeout: 200 * time.Millisecond}}
		regs, results := taskRegistrations(specs, handlers, false)
		sys := startAgentHarness(t, store, agentHarnessSettings{v2: regs, callbackBudget: time.Second})
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		call, err := sys.h.Dial(ctx, CallOptions{})
		require.NoError(t, err)
		<-entered
		result := takeTaskFinal(t, results[0])
		require.Equal(t, agent.Timeout, result[len(result)-1].Outcome)
		wait, stop := context.WithTimeout(ctx, 60*time.Millisecond)
		require.ErrorIs(t, sys.workers[0].QuiesceTasks(wait), context.DeadlineExceeded, "an attempt outcome does not prove callback return")
		stop()
		once.Do(func() { close(release) })
		require.NoError(t, sys.workers[0].QuiesceTasks(ctx))
		_, err = call.Hangup(ctx)
		require.NoError(t, err)
	})
}

// Chunk-paced handlers remain in flight even while their earlier audio plays.
// Quiescence spends its budget, then urgent export resumes above the accepted
// prefix rather than generating or playing that prefix twice.
func TestV2DrainChunkedPlaybackUsesTaskBudget(t *testing.T) {
	forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
		regs, _, gate, _ := pacerRegistrations(t, 80, false, false)
		packets := pacerFixtures(t, 80)
		after := make(chan uint64, 1)
		for i := range regs {
			regs[i].Tasks["tts/v1"] = func(ctx context.Context, run *agent.TaskRun) error {
				select {
				case <-gate:
				case <-ctx.Done():
					return ctx.Err()
				}
				if i == 1 {
					after <- run.After()
				}
				for start := 0; start < len(packets); start += 5 {
					sequence := uint64(start/5 + 1)
					if sequence <= run.After() {
						continue
					}
					if err := run.Emit(ctx, sequence, agent.Chunk{Audio: &agent.EncodedAudio{Format: "audio/opus", Packets: packets[start : start+5]}}); err != nil {
						return err
					}
				}
				return nil
			}
		}
		sys := startAgentHarness(t, store, agentHarnessSettings{v2: regs, remoteTarget: true, callbackBudget: time.Second})
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		call, err := sys.h.Dial(ctx, CallOptions{})
		require.NoError(t, err)
		received := receivePacer(call)
		close(gate)
		first := nextPaced(t, received)
		wait, stop := context.WithTimeout(ctx, 250*time.Millisecond)
		start := time.Now()
		require.ErrorIs(t, sys.workers[0].QuiesceTasks(wait), context.DeadlineExceeded)
		stop()
		waitEnded := time.Now()
		require.GreaterOrEqual(t, waitEnded.Sub(start), 225*time.Millisecond)
		before, err := drainAgentStatus(sys.workers[0], call.SessionID())
		require.NoError(t, err)
		require.Len(t, before.Tasks, 1)
		require.GreaterOrEqual(t, before.Tasks[0].Published, uint64(2))
		_, err = sys.plane.Drain(ctx, "0")
		require.NoError(t, err)
		select {
		case cursor := <-after:
			require.GreaterOrEqual(t, cursor, before.Tasks[0].Published)
			require.Less(t, cursor, uint64(16))
		case <-ctx.Done():
			t.Fatal("target did not resume chunked task")
		}
		playedDuringWait := false
		for i, want := range packets {
			sample := first
			if i > 0 {
				sample = nextPaced(t, received)
			}
			require.Equal(t, want, sample.packet.Payload)
			playedDuringWait = playedDuringWait || sample.at.After(start) && sample.at.Before(waitEnded)
		}
		require.True(t, playedDuringWait, "audio keeps playing while the chunked handler uses its wait budget")
		_, err = call.Hangup(ctx)
		require.NoError(t, err)
	})
}
