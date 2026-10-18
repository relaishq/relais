package workerapp_test

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/clusterprocess"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/callharness"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/relay"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/relaishq/relais/pkg/storage"
	"github.com/relaishq/relais/pkg/workerapp"
	"github.com/stretchr/testify/require"
)

type drainProcessResult struct {
	ID      agent.TaskID
	Outcome agent.Outcome
	Final   bool
	Data    string
}
type drainProcessAgent struct {
	Mode    string
	Results []drainProcessResult
}

func (a *drainProcessAgent) Handle(_ context.Context, e agent.Event) (agent.Effects, error) {
	if e.Kind == agent.Started && strings.HasPrefix(a.Mode, "playback-") {
		class := agent.ReadOnly
		if a.Mode == "playback-side-effecting" {
			class = agent.SideEffecting
		}
		tasks := []agent.TaskSpec{{Step: "play", Kind: "drain/playback", Class: class, Resume: agent.Retry}}
		if a.Mode == "playback-mixed" {
			tasks = append(tasks, agent.TaskSpec{Step: "other", Kind: "drain/task", Class: agent.ReadOnly, Resume: agent.Retry, Request: []byte("other finished")})
		}
		tasks = append(tasks, agent.TaskSpec{Step: "queued", Kind: "drain/task", Class: agent.ReadOnly, Resume: agent.Abandon, Request: []byte("queued exact request")})
		return agent.Effects{StartTasks: tasks}, nil
	}
	if e.Kind == agent.Started {
		return agent.Effects{StartTasks: []agent.TaskSpec{
			{Step: "running", Kind: "drain/task", Class: agent.ReadOnly, Resume: agent.Abandon, Request: []byte("running request")},
			{Step: "queued", Kind: "drain/task", Class: agent.ReadOnly, Resume: agent.Abandon, Request: []byte("queued exact request")},
		}}, nil
	}
	if e.Kind == agent.TaskMessage {
		a.Results = append(a.Results, drainProcessResult{e.Result.TaskID, e.Result.Outcome, e.Result.Final, string(e.Result.Chunk.Data)})
	}
	return agent.Effects{}, nil
}
func (a *drainProcessAgent) Save(context.Context) (agent.State, error) {
	b, err := json.Marshal(a)
	return agent.State{Version: 1, Bytes: b}, err
}
func (a *drainProcessAgent) Restore(_ context.Context, s agent.State) error {
	return json.Unmarshal(s.Bytes, a)
}

// The test executable is a real worker process using the same public lifecycle
// as a custom binary. The only external task service is the test's local gate.
func TestDrainProcessWorker(t *testing.T) {
	if os.Getenv("RELAIS_DRAIN_TEST_CHILD") != "1" {
		return
	}
	var args []string
	require.NoError(t, json.Unmarshal([]byte(os.Getenv("RELAIS_DRAIN_TEST_ARGS")), &args))
	os.Args = append([]string{os.Args[0]}, args...)
	mode := os.Getenv("RELAIS_DRAIN_TEST_MODE")
	reg := workerapp.Registration{Name: "drain-test/v1", Factory: func(agent.FactoryArgs) agent.Agent { return &drainProcessAgent{Mode: mode} }, Limits: agent.Limits{MaxActiveTasks: 1}, Tasks: map[string]agent.TaskHandler{
		"drain/task": func(ctx context.Context, run *agent.TaskRun) error {
			if os.Getenv("RELAIS_DRAIN_TEST_SOURCE") == "1" {
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, os.Getenv("RELAIS_DRAIN_TEST_GATE"), nil)
				if err != nil {
					return err
				}
				response, err := http.DefaultClient.Do(req)
				if err != nil {
					return err
				}
				_, _ = io.Copy(io.Discard, response.Body)
				_ = response.Body.Close()
			}
			if run.Spec().Step == "other" {
				return nil // One terminal admission when the 300 ms gate opens.
			}
			return run.Emit(ctx, run.After()+1, agent.Chunk{Data: run.Spec().Request})
		}}}
	if strings.HasPrefix(mode, "playback-") {
		reg.Limits.MaxStateBytes = 4096 // Retain the 16-chunk transcript in app state.
		if mode != "playback-wait" {
			reg.SkipDrainWait = []string{"drain/playback"}
		}
		if mode == "playback-mixed" {
			reg.Limits.MaxActiveTasks = 2
		}
		reg.Tasks["drain/playback"] = func(ctx context.Context, run *agent.TaskRun) error {
			notify := func(path string) error {
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, os.Getenv("RELAIS_DRAIN_TEST_GATE")+path, nil)
				if err != nil {
					return err
				}
				response, err := http.DefaultClient.Do(req)
				if err != nil {
					return err
				}
				return response.Body.Close()
			}
			if os.Getenv("RELAIS_DRAIN_TEST_SOURCE") != "1" {
				if err := notify("/after/" + strconv.FormatUint(run.After(), 10)); err != nil {
					return err
				}
			}
			for seq := run.After() + 1; seq <= 16; seq++ {
				if err := run.Emit(ctx, seq, agent.Chunk{Data: []byte(fmt.Sprintf("chunk/%02d", seq))}); err != nil {
					return err
				}
				if seq == 1 && os.Getenv("RELAIS_DRAIN_TEST_SOURCE") == "1" {
					if err := notify("/playback"); err != nil {
						return err
					}
				}
			}
			return nil
		}
	}
	workerapp.Main(workerapp.Options{DefaultAgent: reg.Name, Agents: []workerapp.Registration{reg}})
}

func TestWorkerSIGTERMQuiescence(t *testing.T) {
	if testing.Short() {
		t.Skip("real process drain matrix")
	}
	addr := os.Getenv("RELAIS_TEST_REDIS_ADDR")
	if addr == "" {
		if os.Getenv("RELAIS_TEST_REDIS_REQUIRE") == "1" {
			t.Fatal("dedicated Redis required")
		}
		t.Skip("dedicated Redis not configured")
	}
	for _, mode := range []string{"concurrent", "unset", "budget", "mark-fails", "unreachable", "store-stall", "kill"} {
		t.Run(mode, func(t *testing.T) { testWorkerDrainProcess(t, addr, mode) })
	}
}
func TestWorkerSIGTERMPlaybackDrainWait(t *testing.T) {
	if testing.Short() {
		t.Skip("real process playback drain matrix")
	}
	addr := os.Getenv("RELAIS_TEST_REDIS_ADDR")
	if addr == "" {
		if os.Getenv("RELAIS_TEST_REDIS_REQUIRE") == "1" {
			t.Fatal("dedicated Redis required")
		}
		t.Skip("dedicated Redis not configured")
	}
	for _, mode := range []string{"playback-skip", "playback-wait", "playback-side-effecting", "playback-mixed"} {
		t.Run(mode, func(t *testing.T) { testWorkerDrainProcess(t, addr, mode) })
	}
}

func testWorkerDrainProcess(t *testing.T, addr, mode string) {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	prefix := fmt.Sprintf("drain-process:%d:", time.Now().UnixNano())
	store, err := sessionstore.NewRedis(ctx, storage.RedisConfig{Addr: addr, Prefix: prefix}, make([]byte, 32))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	r, err := relay.New(relay.Config{Owners: store})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, r.Close()) })
	plane := controlplane.New(r, store)
	proxy := newDrainRedisProxy(t, addr)

	gate := make(chan struct{})
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(gate) }) })
	entered := make(chan struct{}, 128)
	resumed := make(chan uint64, 1)
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/after/") {
			after, err := strconv.ParseUint(strings.TrimPrefix(r.URL.Path, "/after/"), 10, 64)
			if err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			resumed <- after
			w.WriteHeader(204)
			return
		}
		entered <- struct{}{}
		if r.URL.Path == "/playback" {
			w.WriteHeader(204)
			return
		}
		select {
		case <-gate:
		case <-r.Context().Done():
		}
		w.WriteHeader(204)
	}))
	t.Cleanup(service.Close)
	marked := make(chan struct{})
	var markOnce sync.Once
	drained := make(chan time.Time, 2)
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/mark-draining") {
			if mode == "mark-fails" {
				w.WriteHeader(503)
				return
			}
			if mode == "store-stall" {
				proxy.stalled.Store(true)
			}
			plane.ProcessHandler().ServeHTTP(w, r)
			markOnce.Do(func() { close(marked) })
			return
		}
		if r.URL.Path == "/workers/0/drain" {
			drained <- time.Now()
		}
		plane.ProcessHandler().ServeHTTP(w, r)
	}))
	t.Cleanup(control.Close)
	children := make([]*clusterprocess.Child, 2)
	childLogs := make([]string, 2)
	remotes := make([]*controlplane.RemoteWorker, 2)
	for i := range children {
		workerRedis := addr
		if i == 0 && mode == "store-stall" {
			workerRedis = proxy.listener.Addr().String()
		}
		args := []string{"-name", strconv.Itoa(i), "-control", control.URL, "-relay-leg", r.WorkerAddr().String(), "-relay-media", r.PublicAddr().String(), "-redis", workerRedis, "-redis-prefix", prefix, "-frame-cache-off"}
		if mode == "concurrent" || mode == "kill" {
			args = append(args, "-drain-timeout=6s", "-agent-drain-task-budget=2s")
		} else {
			args = append(args, "-drain-timeout=3s", "-agent-drain-task-budget=1s")
		}
		if mode != "unset" {
			args = append(args, "-agent-drain-move-reserve=1s")
		}
		if strings.HasPrefix(mode, "playback-") {
			args = append(args, "-agent-max-snapshot-bytes=16384", "-agent-snapshot-burst-bytes=65536", "-agent-snapshot-bytes-per-second=262144")
		}
		encoded, err := json.Marshal(args)
		require.NoError(t, err)
		env := append(os.Environ(), "RELAIS_DRAIN_TEST_CHILD=1", "RELAIS_DRAIN_TEST_MODE="+mode, "RELAIS_DRAIN_TEST_ARGS="+string(encoded), "RELAIS_DRAIN_TEST_GATE="+service.URL, "RELAIS_DRAIN_TEST_SOURCE="+strconv.Itoa(1-i), "RELAIS_SESSIONSTORE_KEY="+hex.EncodeToString(make([]byte, 32)), "GORACE=atexit_sleep_ms=0")
		childDir := t.TempDir()
		childLogs[i] = filepath.Join(childDir, "worker.log")
		child, err := clusterprocess.Start(childDir, "worker", env, os.Args[0], "-test.run=^TestDrainProcessWorker$")
		require.NoError(t, err)
		children[i] = child
		t.Cleanup(child.Stop)
		ready, err := child.Ready(ctx)
		require.NoError(t, err)
		address := netip.MustParseAddrPort(ready.Media)
		r.AddWorker(address)
		remotes[i] = &controlplane.RemoteWorker{URL: ready.HTTP}
		require.NoError(t, plane.Register(strconv.Itoa(i), address, remotes[i]))
	}
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); _ = plane.Run(runCtx) }()
	t.Cleanup(func() { stop(); <-done })
	h, err := callharness.Start(callharness.Options{External: &callharness.ExternalTopology{SignalingURL: control.URL + "/calls", RelayAddr: r.PublicAddr()}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, h.Close()) })
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		for i, path := range childLogs {
			data, err := os.ReadFile(path)
			t.Logf("DRAIN_PROCESS worker=%d log_error=%v log=%s", i, err, data)
		}
		statusCtx, stopStatus := context.WithTimeout(context.Background(), time.Second)
		defer stopStatus()
		status, err := plane.Status(statusCtx)
		t.Logf("DRAIN_PROCESS final control status=%+v error=%v", status, err)
	})
	count := 1
	if mode == "concurrent" {
		count = 20
	}
	calls := make([]*callharness.Call, count)
	for i := range calls {
		calls[i], err = h.Dial(ctx, callharness.CallOptions{})
		require.NoError(t, err)
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("handler never entered")
		}
	}
	if mode == "playback-mixed" {
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("other handler never entered")
		}
	}
	// Kill recovery needs a durable launched task intent, not an empty checkpoint.
	require.Eventually(t, func() bool {
		b, err := store.GetState(ctx, calls[0].SessionID())
		return err == nil && strings.Contains(string(b), "never_launched")
	}, 3*time.Second, 5*time.Millisecond)
	if mode == "unreachable" {
		control.Close()
	}
	start := time.Now()
	require.NoError(t, children[0].SignalGroup(syscall.SIGTERM))
	if mode == "unreachable" {
		select {
		case <-children[0].Done():
			require.NoError(t, children[0].Err())
			require.Less(t, time.Since(start), 700*time.Millisecond, "unreachable control must skip handler wait")
		case <-time.After(3 * time.Second):
			t.Fatal("unreachable control stalled shutdown")
		}
		return
	}
	if mode != "mark-fails" {
		select {
		case <-marked:
		case <-ctx.Done():
			t.Fatal("no placement mark")
		}
	}
	if mode == "playback-mixed" {
		// Release the unlisted handler 300 ms after SIGTERM, while playback
		// remains active for about 1.6 s. No agent callback blocks on this gate.
		if remaining := time.Until(start.Add(300 * time.Millisecond)); remaining > 0 {
			time.Sleep(remaining)
		}
		select {
		case <-drained:
			t.Fatal("drain did not wait for the unlisted handler")
		default:
		}
		release.Do(func() { close(gate) })
	}
	if mode == "concurrent" || mode == "kill" {
		require.Eventually(t, func() bool {
			_, _, err := remotes[0].CreateSession(ctx, "raced offer")
			return errors.Is(err, mediaworker.ErrDraining)
		}, time.Second, 5*time.Millisecond)
		fresh, err := h.Dial(ctx, callharness.CallOptions{Worker: -1})
		require.NoError(t, err)
		status, err := plane.Status(ctx)
		require.NoError(t, err)
		for _, c := range status.Calls {
			if c.ID == fresh.SessionID() {
				require.Equal(t, "1", c.Owner)
			}
		}
		heartbeat := status.Workers[0].LastHeartbeat
		require.NoError(t, calls[0].SendMedia(ctx, 80*time.Millisecond))
		// Read the actual store cut, so a cached status cannot masquerade as a
		// checkpoint made during quiescence.
		require.Eventually(t, func() bool {
			b, err := store.GetState(ctx, calls[0].SessionID())
			if err != nil {
				return false
			}
			var snapshot struct {
				State struct {
					Agent struct{ Progress struct{ Consumed uint64 } }
				}
			}
			return json.Unmarshal(b, &snapshot) == nil && snapshot.State.Agent.Progress.Consumed > 0
		}, time.Second, 5*time.Millisecond, "a loaded race build can need several checkpoint ticks; keep this below the 2s handler budget")
		time.Sleep(140 * time.Millisecond)
		status, err = plane.Status(ctx)
		require.NoError(t, err)
		require.True(t, status.Workers[0].Draining)
		require.False(t, status.Workers[0].Dead)
		require.True(t, status.Workers[0].LastHeartbeat.After(heartbeat))
		// Deliberately spend more than the old 1s budget. This proves the
		// process case has scheduling headroom while checkpoints remain live.
		if remaining := time.Until(start.Add(1100 * time.Millisecond)); remaining > 0 {
			time.Sleep(remaining)
		}
		select {
		case <-drained:
			t.Fatal("control drain ran before handler wait finished")
		default:
		}
		if mode == "kill" {
			require.NoError(t, children[0].SignalGroup(syscall.SIGKILL))
		} else {
			release.Do(func() { close(gate) })
		}
		_, err = fresh.Hangup(ctx)
		require.NoError(t, err)
	}
	if mode != "kill" {
		select {
		case at := <-drained:
			elapsed := at.Sub(start)
			if mode == "unset" || mode == "mark-fails" {
				require.Less(t, elapsed, 500*time.Millisecond)
			}
			if mode == "playback-skip" {
				require.Less(t, elapsed, 700*time.Millisecond, "listed ReadOnly playback must not spend the 1s task budget")
			}
			if mode == "playback-mixed" {
				require.GreaterOrEqual(t, elapsed, 300*time.Millisecond)
				require.Less(t, elapsed, 800*time.Millisecond, "only the unlisted handler and its terminal admission delay drain")
			}
			if mode == "budget" || mode == "store-stall" || mode == "playback-wait" || mode == "playback-side-effecting" {
				require.Greater(t, elapsed, 850*time.Millisecond)
				require.Less(t, elapsed, 1800*time.Millisecond)
			}
			if mode == "concurrent" {
				require.Greater(t, elapsed, time.Second)
				require.Less(t, elapsed, 2500*time.Millisecond)
			}
			t.Logf("SIGTERM mode=%s sessions=%d control_drain_after=%s", mode, count, elapsed)
		case <-ctx.Done():
			t.Fatal("no control drain")
		}
		select {
		case <-children[0].Done():
			if mode == "store-stall" {
				require.Less(t, time.Since(start), 3*time.Second, "store stall must not extend shutdown deadline")
			}
			require.NoError(t, children[0].Err())
		case <-time.After(4 * time.Second):
			t.Fatal("worker did not close within deadline")
		}
	}
	if strings.HasPrefix(mode, "playback-") {
		select {
		case after := <-resumed:
			require.Greater(t, after, uint64(0), "target must resume above the accepted source prefix")
			require.Less(t, after, uint64(16))
			t.Logf("playback resumed after=%d", after)
		case <-ctx.Done():
			t.Fatal("playback did not resume on target")
		}
	}
	// The queued task's final result follows its first chunk by the host's
	// 100 ms emission spacing, so concurrent mode must wait for all four.
	wantResults := 3
	if mode == "concurrent" {
		wantResults = 4
	}
	if strings.HasPrefix(mode, "playback-") {
		wantResults = 19 // 16 chunks + playback final + queued chunk and final
		if mode == "playback-mixed" {
			wantResults++
		}
	}
	for _, call := range calls {
		var state drainProcessAgent
		var lastStatus mediaworker.WorkerStatus
		var lastReadError error
		readResults := func() bool {
			response, err := http.Get(remotes[1].URL + "/status?agent-session=" + call.SessionID())
			if err != nil {
				return false
			}
			defer func() { _ = response.Body.Close() }()
			lastReadError = json.NewDecoder(response.Body).Decode(&lastStatus)
			if lastReadError != nil || lastStatus.AgentSession == nil {
				return false
			}
			if json.Unmarshal(lastStatus.AgentSession.App, &state) != nil {
				return false
			}
			return len(state.Results) >= wantResults || mode == "kill" && len(state.Results) > 0
		}
		// Poll on this goroutine so a timeout cannot format state concurrently
		// with an in-flight HTTP read or JSON decode.
		deadline := time.Now().Add(8 * time.Second)
		for !readResults() {
			if time.Now().After(deadline) {
				t.Fatalf("session %s results=%+v status=%+v read_error=%v", call.SessionID(), state, lastStatus, lastReadError)
			}
			time.Sleep(10 * time.Millisecond)
		}
		if strings.HasPrefix(mode, "playback-") {
			require.Len(t, state.Results, wantResults)
			var chunks []string
			for _, result := range state.Results {
				require.Equal(t, agent.OK, result.Outcome)
				if strings.HasPrefix(result.Data, "chunk/") {
					chunks = append(chunks, result.Data)
				}
			}
			var want []string
			for seq := 1; seq <= 16; seq++ {
				want = append(want, fmt.Sprintf("chunk/%02d", seq))
			}
			require.Equal(t, want, chunks, "accepted playback prefix and resumed suffix must have no gaps or duplicates")
		} else if mode == "concurrent" {
			require.Len(t, state.Results, 4)
			for _, r := range state.Results {
				require.Equal(t, agent.OK, r.Outcome)
			}
		} else {
			require.Equal(t, agent.OutcomeUnknown, state.Results[0].Outcome)
		}
		if mode != "kill" && !strings.HasPrefix(mode, "playback-") {
			require.Equal(t, "queued exact request", state.Results[len(state.Results)-2].Data)
		} else if mode != "kill" {
			var queued int
			for _, result := range state.Results {
				if result.Data == "queued exact request" {
					queued++
				}
			}
			require.Equal(t, 1, queued)
		}
		_, err = call.Hangup(ctx)
		require.NoError(t, err)
	}
}

// Only the source worker's Redis leg stalls. Control and the destination can
// still adopt the exact export, even while a source snapshot is in flight.
type drainRedisProxy struct {
	listener    net.Listener
	stalled     atomic.Bool
	done        chan struct{}
	wg          sync.WaitGroup
	mu          sync.Mutex
	connections []net.Conn
}

func newDrainRedisProxy(t *testing.T, upstream string) *drainRedisProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	p := &drainRedisProxy{listener: listener, done: make(chan struct{})}
	p.wg.Go(func() {
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			server, err := net.Dial("tcp", upstream)
			if err != nil {
				_ = client.Close()
				return
			}
			p.mu.Lock()
			p.connections = append(p.connections, client, server)
			p.mu.Unlock()
			p.wg.Go(func() { _, _ = io.Copy(server, client) })
			p.wg.Go(func() {
				b := make([]byte, 32768)
				for {
					n, err := server.Read(b)
					if err != nil {
						return
					}
					if p.stalled.Load() {
						<-p.done
						return
					}
					if _, err = client.Write(b[:n]); err != nil {
						return
					}
				}
			})
		}
	})
	t.Cleanup(func() {
		close(p.done)
		_ = listener.Close()
		p.mu.Lock()
		for _, conn := range p.connections {
			_ = conn.Close()
		}
		p.mu.Unlock()
		p.wg.Wait()
	})
	return p
}
