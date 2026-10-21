package workerapp_test

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/relaishq/relais/examples/delayed-loopback/loopback"
	"github.com/relaishq/relais/internal/redistestenv"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/callharness"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/relay"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/relaishq/relais/pkg/storage"
	"github.com/stretchr/testify/require"
)

// In-process crash tests use the host's kill seam on both stores. This separate
// process gate proves real SIGKILL/takeover of the shipped example on Redis.
func TestDelayedLoopbackProcessCuts(t *testing.T) {
	if testing.Short() {
		t.Skip("real-process SIGKILL matrix")
	}
	addr := redistestenv.Address(t)
	if addr == "" {
		if os.Getenv("RELAIS_TEST_REDIS_REQUIRE") == "1" {
			t.Fatal("dedicated Redis required")
		}
		t.Skip("dedicated Redis not configured")
	}
	binary := filepath.Join(t.TempDir(), "delayed-loopback")
	build := exec.Command("go", "build", "-o", binary, "../../examples/delayed-loopback")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	out, err := build.CombinedOutput()
	require.NoError(t, err, "%s", out)
	for _, cut := range []string{"recording", "processing", "playout"} {
		t.Run(cut, func(t *testing.T) { testLoopbackProcessCut(t, binary, addr, cut) })
	}
}
func testLoopbackProcessCut(t *testing.T, binary, addr, cut string) {
	token := make([]byte, 16)
	_, err := rand.Read(token)
	require.NoError(t, err)
	prefix := "loopback-process:" + hex.EncodeToString(token) + ":"
	store, err := sessionstore.NewRedis(context.Background(), storage.RedisConfig{Addr: addr, Prefix: prefix}, make([]byte, 32))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	r, err := relay.New(relay.Config{Owners: store})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, r.Close()) })
	plane := controlplane.New(r, store)
	control := httptest.NewServer(plane.ProcessHandler())
	t.Cleanup(control.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	start := func(name string) *externalWorker {
		p := &externalWorker{done: make(chan error, 1)}
		p.cmd = exec.CommandContext(ctx, binary, "-name", name, "-control", control.URL, "-relay-leg", r.WorkerAddr().String(), "-relay-media", r.PublicAddr().String(), "-redis", addr, "-redis-prefix", prefix, "-frame-cache-off")
		p.cmd.Env = append(os.Environ(), "RELAIS_SESSIONSTORE_KEY="+hex.EncodeToString(make([]byte, 32)))
		output, err := p.cmd.StdoutPipe()
		require.NoError(t, err)
		p.cmd.Stderr = os.Stderr
		require.NoError(t, p.cmd.Start())
		ready := make(chan map[string]string, 1)
		go func() {
			scanner := bufio.NewScanner(output)
			for scanner.Scan() {
				var value struct {
					HTTP  string `json:"http"`
					Media string `json:"media"`
				}
				if json.Unmarshal(scanner.Bytes(), &value) == nil && value.HTTP != "" {
					ready <- map[string]string{"http": value.HTTP, "media": value.Media}
				}
			}
		}()
		go func() { p.done <- p.cmd.Wait() }()
		t.Cleanup(func() {
			_ = p.cmd.Process.Kill()
			select {
			case <-p.done:
			case <-time.After(3 * time.Second):
				t.Error("example worker did not exit")
			}
		})
		select {
		case value := <-ready:
			p.addr = netip.MustParseAddrPort(value["media"])
			p.remote = &controlplane.RemoteWorker{URL: value["http"]}
		case <-ctx.Done():
			t.Fatal("example worker readiness timeout")
		}
		return p
	}
	source, target := start("0"), start("1")
	for i, p := range []*externalWorker{source, target} {
		r.AddWorker(p.addr)
		require.NoError(t, plane.Register(strconv.Itoa(i), p.addr, p.remote))
	}
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); _ = plane.Run(runCtx) }()
	defer func() { stop(); <-done }()
	h, err := callharness.Start(callharness.Options{External: &callharness.ExternalTopology{SignalingURL: control.URL + "/calls", RelayAddr: r.PublicAddr()}})
	require.NoError(t, err)
	defer h.Close()
	call, err := h.Dial(ctx, callharness.CallOptions{})
	require.NoError(t, err)
	require.NoError(t, call.SendMedia(ctx, 600*time.Millisecond))
	// Observe the durable cut, including a move-only marker. A crash on an older
	// empty checkpoint would not prove loss of this turn's recording/reply.
	require.Eventually(t, func() bool {
		raw, err := store.GetState(ctx, call.SessionID())
		if err != nil {
			return false
		}
		var snapshot struct {
			State struct {
				Agent struct{ State struct{ Bytes []byte } }
			}
		}
		if json.Unmarshal(raw, &snapshot) != nil {
			return false
		}
		var state loopback.State
		return json.Unmarshal(snapshot.State.Agent.State.Bytes, &state) == nil && state.Phase == cut && state.Turn == 1 && state.Packets > 0
	}, 6*time.Second, 2*time.Millisecond)
	require.NoError(t, source.cmd.Process.Kill())
	readStatus := func() (*mediaworker.AgentSessionStatus, bool) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.remote.URL+"/status?agent-session="+call.SessionID(), nil)
		if err != nil {
			return nil, false
		}
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, false
		}
		defer func() { _ = response.Body.Close() }()
		var status mediaworker.WorkerStatus
		if response.StatusCode != 200 || json.NewDecoder(response.Body).Decode(&status) != nil {
			return nil, false
		}
		return status.AgentSession, status.AgentSession != nil
	}
	var recovered loopback.State
	require.Eventually(t, func() bool {
		status, ok := readStatus()
		return ok && json.Unmarshal(status.App, &recovered) == nil && recovered.Turn == 2 && recovered.Resume != nil
	}, 8*time.Second, 5*time.Millisecond)
	require.Equal(t, agent.OutcomeUnknown, recovered.Resume.MoveOnly)
	require.Equal(t, agent.OutcomeUnknown, recovered.Outcome)
	require.Equal(t, "recording", recovered.Phase)
	require.Zero(t, recovered.Cursor)
	status, ok := readStatus()
	require.True(t, ok)
	require.Zero(t, status.RecordingBytes)
	require.Zero(t, status.PlaybackBytes)
	require.Zero(t, status.PlaybackPackets)
	require.NoError(t, call.SendMedia(ctx, 100*time.Millisecond))
	require.Eventually(t, func() bool {
		status, ok := readStatus()
		return ok && json.Unmarshal(status.App, &recovered) == nil && recovered.Turn == 3
	}, 6*time.Second, 5*time.Millisecond)
	_, err = call.Hangup(ctx)
	require.NoError(t, err)
}
