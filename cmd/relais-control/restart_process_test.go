package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/clusterprocess"
	"github.com/relaishq/relais/internal/privateapi"
	"github.com/relaishq/relais/internal/redistestcluster"
	"github.com/relaishq/relais/internal/redistestenv"
	agentv2 "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/callharness"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/stretchr/testify/require"
)

type restartProcesses struct {
	t                          *testing.T
	ctx                        context.Context
	manager                    clusterprocess.Manager
	bin, dir                   string
	env                        []string
	control, relay, leg, media string
	workers                    [2]string
	workerURLs                 [2]string
	workerChildren             [2]*clusterprocess.Child
	controlChild               *clusterprocess.Child
	generation                 int
}

func restartBinaries(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for _, name := range []string{"relais-relay", "relais-worker", "relais-control"} {
		if name == "relais-control" && os.Getenv("RELAIS_TEST_CONTROL_BINARY") != "" {
			binary, err := os.ReadFile(os.Getenv("RELAIS_TEST_CONTROL_BINARY"))
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(bin, name), binary, 0700))
			continue
		}
		command := exec.CommandContext(ctx, "go", "build", "-o", filepath.Join(bin, name), "./cmd/"+name)
		command.Dir = "../.."
		command.Env = append(os.Environ(), "CGO_ENABLED=0")
		out, err := command.CombinedOutput()
		require.NoError(t, err, "%s", out)
	}
	return bin
}
func newRestartProcesses(t *testing.T, ctx context.Context, bin, redisAddr string, cluster bool) *restartProcesses {
	t.Helper()
	dir := t.TempDir()
	sys := &restartProcesses{t: t, ctx: ctx, bin: bin, dir: dir}
	t.Cleanup(func() {
		if t.Failed() {
			entries, _ := os.ReadDir(sys.dir)
			for _, entry := range entries {
				if strings.HasSuffix(entry.Name(), ".log") {
					data, _ := os.ReadFile(filepath.Join(sys.dir, entry.Name()))
					if len(data) > 8000 {
						data = data[len(data)-8000:]
					}
					t.Logf("PROCESS_LOG %s\n%s", entry.Name(), data)
				}
			}
		}
		sys.manager.Stop(false)
	})
	tcp := func() string { addr, err := clusterprocess.FreeTCP(); require.NoError(t, err); return addr }
	udp := func() string { addr, err := clusterprocess.FreeUDP(); require.NoError(t, err); return addr }
	sys.control, sys.relay, sys.workers[0], sys.workers[1] = tcp(), tcp(), tcp(), tcp()
	sys.leg, sys.media = udp(), udp()
	sys.env = append(os.Environ(), "RELAIS_REDIS_ADDR="+redisAddr, "RELAIS_REDIS_CLUSTER="+map[bool]string{true: "1", false: "0"}[cluster], fmt.Sprintf("RELAIS_REDIS_PREFIX=restart:%d:", time.Now().UnixNano()), "RELAIS_SESSIONSTORE_KEY="+strings.Repeat("00", 32))
	sys.start("relay", filepath.Join(bin, "relais-relay"), "-http", sys.relay, "-leg", sys.leg, "-media", sys.media)
	for i := range 2 {
		args := []string{filepath.Join(bin, "relais-worker"), "-name", strconv.Itoa(i), "-http", sys.workers[i], "-media", "127.0.0.1:0", "-relay-leg", sys.leg, "-relay-media", sys.media, "-control", "http://" + sys.control, "-frame-cache-off"}
		if os.Getenv("RELAIS_TEST_RESTART_SPLIT") == "1" {
			args = append(args, "-agent-split-checkpoints")
		}
		sys.workerChildren[i] = sys.start(fmt.Sprintf("worker-%d", i), args...)
		sys.workerURLs[i] = "http://" + sys.workers[i]
	}
	return sys
}
func (s *restartProcesses) start(name string, args ...string) *clusterprocess.Child {
	s.t.Helper()
	child, err := s.manager.Start(s.dir, name, s.env, args...)
	require.NoError(s.t, err)
	bounded, cancel := context.WithTimeout(s.ctx, 10*time.Second)
	defer cancel()
	_, err = child.Ready(bounded)
	require.NoError(s.t, err)
	return child
}
func (s *restartProcesses) startControl() time.Duration {
	s.t.Helper()
	s.generation++
	started := time.Now()
	s.controlChild = s.start(fmt.Sprintf("control-%d", s.generation), filepath.Join(s.bin, "relais-control"), "-http", s.control, "-relay", "http://"+s.relay, "-worker", "0="+s.workerURLs[0], "-worker", "1="+s.workerURLs[1])
	s.waitReady()
	return time.Since(started)
}
func (s *restartProcesses) status() (controlplane.Status, error) {
	var status controlplane.Status
	ctx, cancel := context.WithTimeout(s.ctx, time.Second)
	defer cancel()
	err := privateapi.Do(ctx, http.DefaultClient, "http://"+s.control, http.MethodGet, "/status", nil, &status, controlplane.RemoteErrors)
	return status, err
}
func (s *restartProcesses) waitReady() {
	s.t.Helper()
	require.Eventually(s.t, func() bool {
		status, err := s.status()
		return err == nil && status.Ready && (os.Getenv("RELAIS_TEST_CONTROL_BINARY") != "" || status.Rebuild != nil && status.Rebuild.Phase == "ready")
	}, 12*time.Second, 20*time.Millisecond)
}
func (s *restartProcesses) kill(child *clusterprocess.Child) {
	s.t.Helper()
	require.NoError(s.t, child.SignalGroup(syscall.SIGKILL))
	<-child.Done()
}
func (s *restartProcesses) harness(t *testing.T) *callharness.Harness {
	h, err := callharness.Start(callharness.Options{External: &callharness.ExternalTopology{SignalingURL: "http://" + s.control + "/calls", RelayAddr: netip.MustParseAddrPort(s.media)}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, h.Close()) })
	return h
}
func (s *restartProcesses) assertOwners(t *testing.T) {
	status, err := s.status()
	require.NoError(t, err)
	observed := map[string][]mediaworker.InstalledSession{}
	for _, endpoint := range s.workerURLs {
		worker := &controlplane.RemoteWorker{URL: endpoint}
		ctx, cancel := context.WithTimeout(s.ctx, time.Second)
		local, err := worker.Status(ctx)
		cancel()
		if err != nil {
			continue
		}
		for _, entry := range local.Inventory {
			observed[entry.ID] = append(observed[entry.ID], entry)
		}
	}
	for _, call := range status.Calls {
		var matching int
		for _, entry := range observed[call.ID] {
			if entry.Owner == call.Address && entry.Epoch == call.Epoch {
				matching++
			}
		}
		require.Equal(t, 1, matching, "call %s observations=%v", call.ID, observed[call.ID])
	}
}
func standaloneRestartRedis(t *testing.T) string {
	t.Helper()
	addr := redistestenv.Address(t)
	if addr == "" {
		if os.Getenv("RELAIS_TEST_REDIS_REQUIRE") == "1" {
			t.Fatal("dedicated Redis required")
		}
		t.Skip("dedicated Redis not configured")
	}
	return addr
}

// These are distinct OS processes. Every original call is moved, recovered
// after a worker SIGKILL, and ended by the successor. Packet arrival timestamps
// prove authenticated media continued during the control-only outage.
func TestControlRestartProcesses(t *testing.T) {
	if testing.Short() {
		t.Skip("process restart/media tests")
	}
	addr := standaloneRestartRedis(t)
	testControlRestartProcesses(t, addr, false, []int{1, 8, 32})
}
func TestControlRestartSplitProcesses(t *testing.T) {
	if testing.Short() {
		t.Skip("split checkpoint process restart")
	}
	addr := standaloneRestartRedis(t)
	t.Setenv("RELAIS_TEST_RESTART_SPLIT", "1")
	testControlRestartProcesses(t, addr, false, []int{1})
}

func TestClusterControlRestartProcesses(t *testing.T) {
	if testing.Short() {
		t.Skip("process restart/media tests")
	}
	addrs := redistestcluster.Addresses(t)
	testControlRestartProcesses(t, strings.Join(addrs, ","), true, []int{1, 8, 32})
}
func testControlRestartProcesses(t *testing.T, addr string, cluster bool, counts []int) {
	bin := restartBinaries(t)
	for _, count := range counts {
		t.Run(fmt.Sprintf("calls-%d", count), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			sys := newRestartProcesses(t, ctx, bin, addr, cluster)
			sys.startControl()
			h := sys.harness(t)
			calls := make([]*callharness.Call, count)
			for i := range count {
				var err error
				calls[i], err = h.Dial(ctx, callharness.CallOptions{Worker: 0})
				require.NoError(t, err)
			}
			flowCtx, stopFlow := context.WithCancel(ctx)
			var flow sync.WaitGroup
			for _, call := range calls {
				flow.Go(func() { _ = call.SendMedia(flowCtx, 80*time.Second) })
			}
			defer func() { stopFlow(); flow.Wait() }()
			// A separate control cannot acquire authority or mutate while first lives.
			secondHTTP, err := clusterprocess.FreeTCP()
			require.NoError(t, err)
			second := sys.start("control-contender", filepath.Join(bin, "relais-control"), "-http", secondHTTP, "-relay", "http://"+sys.relay, "-worker", "0="+sys.workerURLs[0], "-worker", "1="+sys.workerURLs[1])
			var competing controlplane.Status
			require.NoError(t, privateapi.Do(ctx, http.DefaultClient, "http://"+secondHTTP, http.MethodGet, "/status", nil, &competing, controlplane.RemoteErrors))
			require.Never(t, func() bool {
				var status controlplane.Status
				err := privateapi.Do(ctx, http.DefaultClient, "http://"+secondHTTP, http.MethodGet, "/status", nil, &status, controlplane.RemoteErrors)
				return err != nil || status.Ready
			}, controlplane.ControlLeaseTTL+500*time.Millisecond, 50*time.Millisecond)
			require.Equal(t, "waiting_lease", competing.Rebuild.Phase)
			for _, path := range []string{"/calls/" + calls[0].SessionID() + "/move", "/workers/0/drain"} {
				err := privateapi.Do(ctx, http.DefaultClient, "http://"+secondHTTP, http.MethodPost, path, nil, nil, controlplane.RemoteErrors)
				require.ErrorIs(t, err, agentv2.ErrControlNotReady)
			}
			err = privateapi.Do(ctx, http.DefaultClient, "http://"+secondHTTP, http.MethodDelete, "/calls/"+calls[0].SessionID(), nil, nil, controlplane.RemoteErrors)
			require.ErrorIs(t, err, agentv2.ErrControlNotReady)
			sys.kill(second)
			downStart := time.Now()
			sys.kill(sys.controlChild)
			// Stay down long enough to require a sequence of authenticated packets.
			require.True(t, restartProcessWait(ctx, time.Second))
			downEnd := time.Now()
			elapsed := sys.startControl()
			status, err := sys.status()
			require.NoError(t, err)
			require.Len(t, status.Calls, count)
			t.Logf("REBUILD calls=%d cluster=%t process_to_ready=%s reported=%s lease_wait=%s", count, cluster, elapsed, status.Rebuild.Duration, status.Rebuild.LeaseWait)
			for _, call := range calls {
				var result controlplane.MoveResult
				require.NoError(t, privateapi.Do(ctx, http.DefaultClient, "http://"+sys.control, http.MethodPost, "/calls/"+call.SessionID()+"/move?to=1", nil, &result, controlplane.RemoteErrors))
			}
			sys.assertOwners(t)
			// Snapshot freshness is provided by live media, not an artificial sleep.
			sys.kill(sys.workerChildren[1])
			var last controlplane.Status
			defer func() {
				if t.Failed() {
					t.Logf("LAST_STATUS %+v", last)
				}
			}()
			require.Eventually(t, func() bool {
				status, err := sys.status()
				last = status
				if err != nil || len(status.Calls) != count {
					return false
				}
				for _, call := range status.Calls {
					if call.Owner != "0" || call.TakeoverCount == 0 {
						return false
					}
				}
				return true
			}, 8*time.Second, 20*time.Millisecond)
			sys.assertOwners(t)
			// Rebuild the still-live inventory while the killed worker remains
			// down, then end every original call through that successor too.
			sys.kill(sys.controlChild)
			sys.startControl()
			restored, err := sys.status()
			require.NoError(t, err)
			require.Len(t, restored.Calls, count)
			stopFlow()
			flow.Wait()
			for _, call := range calls {
				report, err := call.Hangup(ctx)
				require.NoError(t, err)
				require.True(t, report.ConnectedThroughout())
				require.Zero(t, report.DecryptionFailures.Total())
				require.Zero(t, report.ICERestarts)
				require.Zero(t, report.Renegotiations)
				audio := report.Track("audio")
				require.NotNil(t, audio)
				arrivals := 0
				var previous time.Time
				for _, offset := range audio.Arrivals {
					at := report.StartedAt.Add(offset)
					if at.Before(downStart) || !at.Before(downEnd) {
						continue
					}
					arrivals++
					if !previous.IsZero() {
						require.Less(t, at.Sub(previous), 300*time.Millisecond, "control-only outage gap")
					}
					previous = at
				}
				require.GreaterOrEqual(t, arrivals, 10, "media must flow while control is absent")
			}
			status, err = sys.status()
			require.NoError(t, err)
			require.Empty(t, status.Calls)
		})
	}
}
func restartProcessWait(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// A response gate stops only the control-facing reply. The real worker handler
// has committed its effect, so this exercises an actual unknown outcome.
type operationReplyGate struct {
	path    string
	armed   atomic.Bool
	entered chan struct{}
	release chan struct{}
	resumes atomic.Int64
}

func (g *operationReplyGate) proxy(t *testing.T, target string) string {
	parsed, err := url.Parse(target)
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(parsed)
	proxy.ModifyResponse = func(response *http.Response) error {
		if response.Request.URL.Path == "/resume" {
			g.resumes.Add(1)
		}
		matches := strings.HasSuffix(response.Request.URL.Path, g.path)
		if g.path == "/sessions/" {
			matches = response.Request.Method == http.MethodDelete && strings.HasPrefix(response.Request.URL.Path, g.path)
		}
		if matches && g.armed.CompareAndSwap(true, false) {
			close(g.entered)
			select {
			case <-g.release:
			case <-response.Request.Context().Done():
			}
		}
		return nil
	}
	server := httptest.NewServer(proxy)
	t.Cleanup(server.Close)
	return server.URL
}

func TestControlRestartInterruptedOperationsProcesses(t *testing.T) {
	if testing.Short() {
		t.Skip("interrupted process operations")
	}
	addr := standaloneRestartRedis(t)
	bin := restartBinaries(t)
	for _, operation := range []string{"move-export", "move-resume", "drain", "create", "end"} {
		t.Run(operation, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			sys := newRestartProcesses(t, ctx, bin, addr, false)
			gate := &operationReplyGate{entered: make(chan struct{}), release: make(chan struct{})}
			target := 0
			switch operation {
			case "create":
				gate.path = "/sessions"
			case "end":
				gate.path = "/sessions/"
			case "move-resume":
				gate.path = "/resume"
				target = 1
			default:
				gate.path = "/export"
			}
			sys.workerURLs[target] = gate.proxy(t, sys.workerURLs[target])
			sys.startControl()
			h := sys.harness(t)
			call, err := h.Dial(ctx, callharness.CallOptions{Worker: 0})
			require.NoError(t, err)
			mediaCtx, stopMedia := context.WithCancel(ctx)
			mediaDone := make(chan struct{})
			go func() { defer close(mediaDone); _ = call.SendMedia(mediaCtx, 40*time.Second) }()
			defer func() { stopMedia(); <-mediaDone }()
			gate.armed.Store(true)
			operationDone := make(chan struct{})
			go func() {
				defer close(operationDone)
				switch operation {
				case "end":
					_ = privateapi.Do(ctx, http.DefaultClient, "http://"+sys.control, http.MethodDelete, "/calls/"+call.SessionID(), nil, nil, controlplane.RemoteErrors)
				case "create":
					_, _ = h.Dial(ctx, callharness.CallOptions{Worker: 0})
				case "drain":
					_ = privateapi.Do(ctx, http.DefaultClient, "http://"+sys.control, http.MethodPost, "/workers/0/drain", nil, nil, controlplane.RemoteErrors)
				default:
					_ = privateapi.Do(ctx, http.DefaultClient, "http://"+sys.control, http.MethodPost, "/calls/"+call.SessionID()+"/move?to=1", nil, nil, controlplane.RemoteErrors)
				}
			}()
			select {
			case <-gate.entered:
			case <-time.After(8 * time.Second):
				t.Fatal("operation never reached gated committed response")
			}
			resumes := gate.resumes.Load()
			sys.kill(sys.controlChild)
			close(gate.release)
			<-operationDone
			sys.startControl()
			status, err := sys.status()
			require.NoError(t, err)
			require.NotEmpty(t, status.Rebuild.Outcomes)
			t.Logf("INTERRUPTED operation=%s outcomes=%+v live_calls=%d", operation, status.Rebuild.Outcomes, len(status.Calls))
			for _, outcome := range status.Rebuild.Outcomes {
				require.Contains(t, []string{"live", "lost", "ended", "partial", "reconciled"}, outcome.Outcome)
			}
			if operation == "move-resume" {
				require.Equal(t, resumes, gate.resumes.Load(), "installed tenure must not be resumed again")
			}
			if operation == "drain" {
				require.True(t, status.Workers[0].Draining)
				found := false
				for _, outcome := range status.Rebuild.Outcomes {
					found = found || outcome.Operation == "drain" && outcome.Outcome == "partial"
				}
				require.True(t, found)
			}
			sys.assertOwners(t)
			stopMedia()
			<-mediaDone
			for _, current := range status.Calls {
				require.NoError(t, privateapi.Do(ctx, http.DefaultClient, "http://"+sys.control, http.MethodDelete, "/calls/"+current.ID, nil, nil, controlplane.RemoteErrors))
			}
			final, err := sys.status()
			require.NoError(t, err)
			require.Empty(t, final.Calls)
		})
	}
}

// The optional control-binary override runs this exact regression against the
// accepted baseline, with today's relay/workers. Legacy startup has no rebuild
// field; the assertion below still requires its successor to retain the call.
func TestControlRestartKeepsCallInventoryProcesses(t *testing.T) {
	if testing.Short() {
		t.Skip("call inventory process restart")
	}
	addr := standaloneRestartRedis(t)
	bin := restartBinaries(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sys := newRestartProcesses(t, ctx, bin, addr, false)
	sys.startControl()
	h := sys.harness(t)
	call, err := h.Dial(ctx, callharness.CallOptions{Worker: 0})
	require.NoError(t, err)
	sys.kill(sys.controlChild)
	sys.startControl()
	status, err := sys.status()
	require.NoError(t, err)
	require.Len(t, status.Calls, 1, "successor must manage the pre-existing call")
	require.Equal(t, call.SessionID(), status.Calls[0].ID)
	var move controlplane.MoveResult
	require.NoError(t, privateapi.Do(ctx, http.DefaultClient, "http://"+sys.control, http.MethodPost, "/calls/"+call.SessionID()+"/move?to=1", nil, &move, controlplane.RemoteErrors))
	_, err = call.Hangup(ctx)
	require.NoError(t, err)
}
