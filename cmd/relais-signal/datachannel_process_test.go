package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/clusterprocess"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/relay"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/relaishq/relais/pkg/storage"
	"github.com/relaishq/relais/pkg/workerapp"
	"github.com/stretchr/testify/require"
)

// Re-exec the already built test executable. No Go build or dependency download
// is hidden in this process test. Only the child worker is SIGKILLed.
func TestDataChannelWorkerProcessHelper(t *testing.T) {
	raw := os.Getenv("RELAIS_DC_WORKER_TEST_ARGS")
	if raw == "" {
		return
	}
	var args []string
	require.NoError(t, json.Unmarshal([]byte(raw), &args))
	require.NoError(t, workerapp.Run(context.Background(), args, workerapp.Options{}))
}

func TestDataChannelWorkerSIGKILLReconnectsThroughSignal(t *testing.T) {
	if testing.Short() {
		t.Skip("real worker SIGKILL/Redis test; fast takeover classification runs in short mode")
	}
	redisBin, err := exec.LookPath("redis-server")
	require.NoError(t, err)
	address, err := clusterprocess.FreeTCP()
	require.NoError(t, err)
	_, port, err := net.SplitHostPort(address)
	require.NoError(t, err)
	require.NotEqual(t, "6379", port)
	dir := t.TempDir()
	redis, err := clusterprocess.Start(dir, "redis", os.Environ(), redisBin, "--bind", "127.0.0.1", "--port", port, "--save", "", "--appendonly", "no", "--dir", dir)
	require.NoError(t, err)
	t.Cleanup(redis.Stop)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	require.NoError(t, clusterprocess.WaitTCP(ctx, address, redis))
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	prefix := "dc-process:" + strconv.FormatInt(time.Now().UnixNano(), 10) + ":"
	store, err := sessionstore.NewRedis(ctx, storage.RedisConfig{Addr: address, Prefix: prefix}, key)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	u := newDCUpstream(t)
	r, err := relay.New(relay.Config{Owners: store, Buffer: &relay.BufferConfig{}})
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })
	p := controlplane.New(r, store)
	control := httptest.NewServer(p.ProcessHandler())
	t.Cleanup(control.Close)
	var killedDCWorker *clusterprocess.Child
	for i := 0; i < 2; i++ {
		args := []string{"-name=" + fmt.Sprint(i), "-redis=" + address, "-redis-prefix=" + prefix, "-control=" + control.URL, "-relay-leg=" + r.WorkerAddr().String(), "-relay-media=" + r.PublicAddr().String(), "-frame-cache-off", "-datachannel", "-datachannel-upstream=" + u.url}
		raw, err := json.Marshal(args)
		require.NoError(t, err)
		env := append(os.Environ(), "RELAIS_DC_WORKER_TEST_ARGS="+string(raw), "RELAIS_SESSIONSTORE_KEY="+hex.EncodeToString(key))
		executable, err := os.Executable()
		require.NoError(t, err)
		child, err := clusterprocess.Start(dir, fmt.Sprintf("worker-%d", i), env, executable, "-test.run=^TestDataChannelWorkerProcessHelper$", "-test.timeout=30s")
		require.NoError(t, err)
		t.Cleanup(child.Stop)
		ready, err := child.Ready(ctx)
		require.NoError(t, err)
		addr := netip.MustParseAddrPort(ready.Media)
		r.AddWorker(addr)
		require.NoError(t, p.Register(fmt.Sprint(i), addr, &controlplane.RemoteWorker{URL: ready.HTTP}))
		if i == 0 {
			// Save the exact owned process, not a process-name match.
			t.Cleanup(func() { t.Logf("owned worker PID=%d", child.PID()) })
			killedDCWorker = child
		}
	}
	srv, _ := frontend(t, p.Handler())
	sys := &dcSystem{plane: p, signal: srv.URL, client: srv, relay: r, store: store, upstream: u}
	runCtx, stopRun := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = p.Run(runCtx) }()
	t.Cleanup(func() { stopRun(); <-done })
	c := dialDC(t, sys, true)
	c.roundtrip(t, 20)
	require.Eventually(t, func() bool { return c.audio.Load() > 5 }, time.Second, 10*time.Millisecond)
	beforeID := c.id
	raw, err := store.GetState(ctx, c.id)
	require.NoError(t, err)
	data, err := mediaworker.SnapshotIsDataSession(raw)
	require.NoError(t, err)
	require.True(t, data)
	require.NoError(t, killedDCWorker.SignalGroup(syscall.SIGKILL))
	<-killedDCWorker.Done()
	t.Logf("SIGKILL owned worker PID=%d, session=%s", killedDCWorker.PID(), c.id)
	require.Eventually(t, func() bool {
		status, err := p.Status(context.Background())
		if err != nil {
			return false
		}
		for _, event := range status.Takeovers {
			if event.ID == c.id {
				require.True(t, event.Lost)
				require.Equal(t, "data_session_reconnect_required", event.LossCause)
				require.Contains(t, event.Error, mediaworker.ErrDataSessionReconnectRequired.Error())
				require.Zero(t, r.Stats().ReplayPackets)
				return true
			}
		}
		return false
	}, 4*time.Second, 20*time.Millisecond)
	// New signal placement must choose the remaining worker. The old ICE/SCTP
	// association is never resumed. A new session produces a fresh upstream id.
	c2 := dialDCOnWorker(t, sys, true, "1", 1024)
	require.NotEqual(t, beforeID, c2.id)
	c2.roundtrip(t, 20)
	require.Eventually(t, func() bool { return c2.audio.Load() > 5 }, time.Second, 10*time.Millisecond)
	require.Equal(t, beforeID, <-u.sessions)
	require.Equal(t, c2.id, <-u.sessions)
}

func TestDataChannelDiagnosticConsumer(t *testing.T) {
	if testing.Short() {
		t.Skip("build/run the diagnostic command; message-path tests remain in short mode")
	}
	sys := newDCSystem(t, true, 0)
	dir := t.TempDir()
	binary := filepath.Join(dir, "dcprobe")
	build := exec.Command("go", "build", "-mod=readonly", "-o", binary, "../../experiments/nvwa-room/cmd/dcprobe")
	build.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off")
	out, err := build.CombinedOutput()
	require.NoError(t, err, string(out))
	ca := filepath.Join(dir, "signal-ca.pem")
	require.NoError(t, os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: sys.client.Certificate().Raw}), 0600))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary, "-signal="+sys.signal, "-ca="+ca, "-n=20", "-interval=1ms", "-settle=100ms")
	command.Env = append(os.Environ(), "RELAIS_SIGNAL_TOKEN="+testToken)
	out, err = command.CombinedOutput()
	require.NoError(t, err, string(out))
	var report struct {
		Session  string
		Channels []struct{ Sent, Received, Loss, Duplicates, Reordered int }
	}
	require.NoError(t, json.Unmarshal(out, &report))
	require.NotEmpty(t, report.Session)
	require.Len(t, report.Channels, 2)
	for _, channel := range report.Channels {
		require.Equal(t, 20, channel.Sent)
		require.Equal(t, 20, channel.Received)
		require.Zero(t, channel.Loss)
		require.Zero(t, channel.Duplicates)
		require.Zero(t, channel.Reordered)
	}
	t.Logf("dcprobe receipt: %s", out)
}
