package workerapp_test

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/redistestenv"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/callharness"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/relay"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/relaishq/relais/pkg/storage"
	"github.com/stretchr/testify/require"
)

type externalWorker struct {
	cmd      *exec.Cmd
	addr     netip.AddrPort
	remote   *controlplane.RemoteWorker
	messages chan map[string]json.RawMessage
	done     chan error
}

func testExternalRegisteredCall(t *testing.T, binary string) {
	addr := redistestenv.Address(t)
	if addr == "" {
		t.Skip("external worker Run requires dedicated Redis; build and flags still checked")
	}
	if testing.Short() {
		t.Skip("process move/takeover runs in extended unit verification")
	}
	token := make([]byte, 16)
	_, err := rand.Read(token)
	require.NoError(t, err)
	prefix := "external-worker:" + hex.EncodeToString(token) + ":"
	store, err := sessionstore.NewRedis(context.Background(), storage.RedisConfig{Addr: addr, Prefix: prefix}, make([]byte, 32))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	r, err := relay.New(relay.Config{Owners: store})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, r.Close()) })
	plane := controlplane.New(r, store)
	control := httptest.NewServer(plane.ProcessHandler())
	t.Cleanup(control.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	start := func(name, selected, registration string) *externalWorker {
		p := &externalWorker{messages: make(chan map[string]json.RawMessage, 64), done: make(chan error, 1)}
		p.cmd = exec.CommandContext(ctx, binary, "-name", name, "-agent", selected, "-control", control.URL, "-relay-leg", r.WorkerAddr().String(), "-relay-media", r.PublicAddr().String(), "-redis", addr, "-redis-prefix", prefix, "-frame-cache-off")
		if name == "1" {
			p.cmd.Args = append(p.cmd.Args, "-agent-move-bytes-per-session", "131072")
		}
		p.cmd.Env = append(os.Environ(), "RELAIS_SESSIONSTORE_KEY="+hex.EncodeToString(make([]byte, 32)), "EXTERNAL_AGENT_NAME="+registration)
		output, err := p.cmd.StdoutPipe()
		require.NoError(t, err)
		p.cmd.Stderr = os.Stderr
		require.NoError(t, p.cmd.Start())
		go func() {
			scanner := bufio.NewScanner(output)
			for scanner.Scan() {
				var msg map[string]json.RawMessage
				if json.Unmarshal(scanner.Bytes(), &msg) == nil {
					p.messages <- msg
				}
			}
			close(p.messages)
		}()
		go func() { p.done <- p.cmd.Wait() }()
		t.Cleanup(func() {
			_ = p.cmd.Process.Kill()
			select {
			case <-p.done:
			case <-time.After(3 * time.Second):
				t.Error("external worker did not exit")
			}
		})
		for {
			select {
			case msg := <-p.messages:
				if msg["http"] == nil {
					continue
				}
				var url, media string
				require.NoError(t, json.Unmarshal(msg["http"], &url))
				require.NoError(t, json.Unmarshal(msg["media"], &media))
				p.addr = netip.MustParseAddrPort(media)
				p.remote = &controlplane.RemoteWorker{URL: url}
				return p
			case <-ctx.Done():
				t.Fatal("worker readiness timeout")
				return nil
			}
		}
	}
	source := start("0", "external/v1", "")
	// The target selects Echo for NEW calls. Persisted external/v1 must still
	// restore through its custom registration on both planned move and takeover.
	target := start("1", "echo", "")
	for i, p := range []*externalWorker{source, target} {
		r.AddWorker(p.addr)
		require.NoError(t, plane.Register(strconv.Itoa(i), p.addr, p.remote))
	}
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	runDone := make(chan struct{})
	go func() { defer close(runDone); _ = plane.Run(runCtx) }()
	defer func() { stop(); <-runDone }()
	h, err := callharness.Start(callharness.Options{External: &callharness.ExternalTopology{SignalingURL: control.URL + "/calls", RelayAddr: r.PublicAddr()}})
	require.NoError(t, err)
	defer h.Close()
	call, err := h.Dial(ctx, callharness.CallOptions{})
	require.NoError(t, err)
	await := func(p *externalWorker, key, want string) {
		t.Helper()
		for {
			select {
			case msg, ok := <-p.messages:
				require.True(t, ok, "worker exited before observation")
				var value string
				_ = json.Unmarshal(msg[key], &value)
				if value != want {
					continue
				}
				if key == "factory" {
					var budget int
					require.NoError(t, json.Unmarshal(msg["budget"], &budget))
					require.Equal(t, 1024, budget)
				}
				return
			case <-ctx.Done():
				t.Fatal("missing external observation " + want)
			}
		}
	}
	await(source, "factory", "external/v1")
	await(source, "event", "started")
	require.NoError(t, call.SendMedia(ctx, 150*time.Millisecond))
	_, err = plane.Move(ctx, call.SessionID(), "1")
	require.NoError(t, err)
	await(target, "factory", "external/v1")
	await(target, "event", "resumed")
	require.NoError(t, call.SendMedia(ctx, 150*time.Millisecond))
	// Checkpoints contain the selected persisted name, without access to internals.
	state, err := store.GetState(ctx, call.SessionID())
	require.NoError(t, err)
	var snapshot struct {
		State struct {
			Agent struct{ V2 struct{ Name string } }
		}
	}
	require.NoError(t, json.Unmarshal(state, &snapshot))
	require.Equal(t, "external/v1", snapshot.State.Agent.V2.Name)
	require.NoError(t, target.cmd.Process.Kill())
	await(source, "factory", "external/v1")
	await(source, "event", "resumed")
	require.NoError(t, call.SendMedia(ctx, 150*time.Millisecond))
	// A mismatched pool refuses through the real private resume endpoint. Move
	// rolls back to the compatible source and reports the typed identity error.
	mismatch := start("2", "echo", "other/v1")
	r.AddWorker(mismatch.addr)
	require.NoError(t, plane.Register("2", mismatch.addr, mismatch.remote))
	result, err := plane.Move(ctx, call.SessionID(), "2")
	require.ErrorIs(t, err, agent.ErrIncompatibleAgent)
	require.True(t, result.Result.RolledBack)
	_, err = call.Hangup(ctx)
	require.NoError(t, err)
}
