package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/relais/internal/clusterprocess"
	"github.com/relais/internal/privateapi"
	"github.com/relais/internal/processrun"
	"github.com/relais/pkg/callharness"
	"github.com/relais/pkg/controlplane"
	"github.com/stretchr/testify/require"
)

// The hook delegates to this test's process supervisor. This keeps every real
// SIGKILL, spawn and registration owned and reaped by the test manager. The
// hook itself is a separate executable with the production argv/JSON contract.
func TestAttachHookHelper(t *testing.T) {
	endpoint := os.Getenv("RELAIS_ATTACH_TEST_SUPERVISOR")
	if endpoint == "" {
		return
	}
	args := os.Args
	marker := -1
	for i, arg := range args {
		if arg == "--" {
			marker = i
			break
		}
	}
	if marker < 0 || len(args) < marker+4 {
		os.Exit(2)
	}
	args = args[marker+1:]
	if args[1] != "--worker" || (args[0] != "kill" && args[0] != "replace") {
		os.Exit(2)
	}
	request := map[string]string{"worker": args[2]}
	if args[0] == "kill" {
		if len(args) != 5 || args[3] != "--session" {
			os.Exit(2)
		}
		request["session"] = args[4]
	} else if len(args) != 3 {
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var receipt hookReceipt
	if privateapi.Do(ctx, &http.Client{Timeout: 20 * time.Second}, endpoint, http.MethodPost, "/"+args[0], request, &receipt, nil) != nil {
		os.Exit(1)
	}
	if json.NewEncoder(os.Stdout).Encode(receipt) != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

func TestDemoAttachExternalSmoke(t *testing.T) {
	if os.Getenv("RELAIS_TEST_REDIS_REQUIRE") != "1" || os.Getenv("RELAIS_HARNESS_REQUIRE_FFMPEG") != "1" {
		t.Skip("requires RELAIS_TEST_REDIS_REQUIRE=1 and RELAIS_HARNESS_REQUIRE_FFMPEG=1; owns throwaway Redis")
	}
	_, err := exec.LookPath("ffmpeg")
	require.NoError(t, err)
	redisBinary, err := exec.LookPath("redis-server")
	require.NoError(t, err)
	buildCtx, stopBuild := context.WithTimeout(context.Background(), 3*time.Minute)
	defer stopBuild()
	bin, err := filepath.Abs(filepath.Join("..", "..", "bin"))
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(bin, 0755))
	dir, err := os.MkdirTemp(bin, "attach-smoke-")
	require.NoError(t, err)
	// Keep binaries and process logs when the test fails; CI uploads them.
	t.Cleanup(func() {
		if !t.Failed() {
			require.NoError(t, os.RemoveAll(dir))
		}
	})
	for _, name := range []string{"relais-relay", "relais-worker", "relais-control", "relais-demo"} {
		cmd := exec.CommandContext(buildCtx, "go", "build", "-o", filepath.Join(dir, name), "./cmd/"+name)
		cmd.Dir = filepath.Join("..", "..")
		output, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", output)
	}
	stopBuild()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	manager := &clusterprocess.Manager{}
	defer manager.Stop(false)
	redisAddr, err := clusterprocess.FreeTCP()
	require.NoError(t, err)
	require.NoError(t, processrun.ValidateRedis(redisAddr))
	_, port, err := net.SplitHostPort(redisAddr)
	require.NoError(t, err)
	redis, err := manager.Start(dir, "redis", os.Environ(), redisBinary, "--bind", "127.0.0.1", "--port", port, "--save", "", "--appendonly", "no", "--dir", dir)
	require.NoError(t, err)
	require.NoError(t, redis.WaitLog(ctx, "Ready to accept connections"))
	prefix := "relais:attach-test:" + filepath.Base(dir) + ":"
	env := processEnv(strings.Repeat("42", 32), redisAddr, prefix, true)
	start := func(name string, childEnv []string, args ...string) (*clusterprocess.Child, clusterprocess.Ready, error) {
		child, err := manager.Start(dir, name, childEnv, args...)
		if err != nil {
			return nil, clusterprocess.Ready{}, err
		}
		readyCtx, stop := context.WithTimeout(ctx, 10*time.Second)
		defer stop()
		ready, err := readyChild(readyCtx, child)
		return child, ready, err
	}
	_, relay, err := start("relay", processEnv("", redisAddr, prefix, false), filepath.Join(dir, "relais-relay"), "-media", "127.0.0.1:0", "-leg", "127.0.0.1:0", "-http", "127.0.0.1:0")
	require.NoError(t, err)
	controlAddr, err := clusterprocess.FreeTCP()
	require.NoError(t, err)
	controlURL := "http://" + controlAddr
	var mu sync.Mutex
	workers := map[string]*workerProcess{}
	used := map[string]bool{}
	spawn := func(name string) (*workerProcess, error) {
		media, err := freshUDP(used)
		if err != nil {
			return nil, err
		}
		child, ready, err := start("worker-"+name, env, filepath.Join(dir, "relais-worker"), "-media", media, "-http", "127.0.0.1:0", "-name", name, "-control", controlURL, "-relay-leg", relay.Leg, "-relay-media", relay.Media)
		if err != nil {
			return nil, err
		}
		return &workerProcess{child: child, URL: ready.HTTP}, nil
	}
	controlArgs := []string{filepath.Join(dir, "relais-control"), "-http", controlAddr, "-relay", relay.HTTP, "-demo-register"}
	for _, name := range []string{"a", "b"} {
		worker, err := spawn(name)
		require.NoError(t, err)
		workers[name] = worker
		controlArgs = append(controlArgs, "-worker", name+"="+worker.URL)
	}
	_, _, err = start("control", env, controlArgs...)
	require.NoError(t, err)
	next := 0
	killedAt := map[string]time.Time{}
	supervisor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		var req map[string]string
		if !privateapi.Read(w, r, &req) {
			return
		}
		worker := workers[req["worker"]]
		if worker == nil {
			http.Error(w, "unknown test-owned worker", http.StatusConflict)
			return
		}
		switch r.URL.Path {
		case "/kill":
			var status controlplane.Status
			if err := privateapi.Do(ctx, http.DefaultClient, controlURL, http.MethodGet, "/status", nil, &status, nil); err != nil {
				http.Error(w, err.Error(), http.StatusConflict)
				return
			}
			owns := false
			for _, call := range status.Calls {
				if call.ID == req["session"] && call.Owner == req["worker"] {
					owns = true
				}
			}
			if !owns {
				http.Error(w, "test worker no longer owns session", http.StatusConflict)
				return
			}
			at := time.Now().UTC()
			if err := worker.child.SignalGroup(syscall.SIGKILL); err != nil {
				http.Error(w, err.Error(), http.StatusConflict)
				return
			}
			select {
			case <-worker.child.Done():
			case <-ctx.Done():
				http.Error(w, "kill timeout", http.StatusConflict)
				return
			}
			killedAt[req["worker"]] = at
			privateapi.Write(w, hookReceipt{OK: true, Action: "kill", Worker: req["worker"], PID: worker.child.PID(), KilledAt: at})
		case "/replace":
			next++
			name := fmt.Sprintf("replacement-%d", next)
			replacement, err := spawn(name)
			if err == nil {
				err = privateapi.Do(ctx, http.DefaultClient, controlURL, http.MethodPost, "/demo/register", map[string]string{"name": name, "url": replacement.URL}, nil, nil)
			}
			if err != nil {
				http.Error(w, err.Error(), http.StatusConflict)
				return
			}
			workers[name] = replacement
			privateapi.Write(w, hookReceipt{OK: true, Action: "replace", Worker: req["worker"], Replacement: name, ReadyAt: time.Now().UTC()})
		default:
			http.NotFound(w, r)
		}
	}))
	defer supervisor.Close()
	helper, err := os.Executable()
	require.NoError(t, err)
	// Shell quoting handles paths; the production launcher executes this script
	// directly and never constructs a shell command from worker/session names.
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	hook := filepath.Join(dir, "fake-fault-hook")
	require.NoError(t, os.WriteFile(hook, []byte("#!/bin/sh\nexec "+quote(helper)+" -test.run='^TestAttachHookHelper$' -- \"$@\"\n"), 0700))
	demoEnv := append(os.Environ(), "RELAIS_ATTACH_TEST_SUPERVISOR="+supervisor.URL)
	page, pageReady, err := start("attached-page", demoEnv, filepath.Join(dir, "relais-demo"), "-http", "127.0.0.1:0", "-attach-control", controlURL, "-relay-buffer", "on", "-fault-hook", hook, "-action-timeout", "20s", "-results-dir", filepath.Join(dir, "results"))
	require.NoError(t, err)
	require.NoError(t, page.WaitLog(ctx, "cluster demo:"))
	logData, err := os.ReadFile(filepath.Join(dir, "attached-page.log"))
	require.NoError(t, err)
	// Extract only the capability for the owned caller. Never include it in logs.
	token := ""
	for _, line := range strings.Split(string(logData), "\n") {
		if _, value, ok := strings.Cut(line, "#token="); ok {
			token = value
		}
	}
	require.NotEmpty(t, token)
	pageURL, err := url.Parse(pageReady.HTTP)
	require.NoError(t, err)
	target := httputil.NewSingleHostReverseProxy(pageURL)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set("X-Relais-Demo-Token", token)
		if r.Method == http.MethodPost && r.URL.Path == "/calls" {
			var status demoStatus
			if err := privateapi.Do(ctx, http.DefaultClient, pageReady.HTTP, http.MethodGet, "/demo/status", nil, &status, nil); err != nil {
				http.Error(w, "status failed", http.StatusBadGateway)
				return
			}
			r.Header.Set("X-Relais-Demo-Launch-ID", status.LaunchID)
		}
		target.ServeHTTP(w, r)
	}))
	defer proxy.Close()
	relayAddr, err := netip.ParseAddrPort(relay.Media)
	require.NoError(t, err)
	h, err := callharness.Start(callharness.Options{External: &callharness.ExternalTopology{SignalingURL: proxy.URL + "/calls", RelayAddr: relayAddr}})
	require.NoError(t, err)
	defer func() { _ = h.Close() }()
	call, err := h.Dial(ctx, callharness.CallOptions{Video: true, Worker: -1})
	require.NoError(t, err)
	mediaCtx, stopMedia := context.WithCancel(ctx)
	sent := make(chan error, 1)
	go func() { sent <- call.SendMedia(mediaCtx, 60*time.Second) }()
	defer func() { stopMedia(); <-sent }()
	read := func() demoStatus {
		var status demoStatus
		require.NoError(t, privateapi.Do(ctx, http.DefaultClient, proxy.URL, http.MethodGet, "/demo/status", nil, &status, nil))
		require.Equal(t, 2, status.ExpectedPoolSize)
		require.Equal(t, 2, status.PoolSize)
		require.Empty(t, status.WorkerPIDs)
		require.Len(t, status.Calls, 1)
		return status
	}
	time.Sleep(1500 * time.Millisecond)
	for i := 0; i < 2; i++ {
		before := read()
		var move controlplane.MoveResult
		require.NoError(t, privateapi.Do(ctx, http.DefaultClient, proxy.URL, http.MethodPost, "/calls/"+call.SessionID()+"/move", nil, &move, nil))
		require.Empty(t, move.Error)
		require.False(t, move.End.IsZero())
		require.NotEqual(t, before.Calls[0].Owner, move.To)
		after := read()
		require.Equal(t, move.To, after.Calls[0].Owner)
		time.Sleep(500 * time.Millisecond)
		var kill actionResult
		require.NoError(t, privateapi.Do(ctx, http.DefaultClient, proxy.URL, http.MethodPost, "/demo/kill", map[string]string{"id": call.SessionID()}, &kill, nil))
		require.Empty(t, kill.Error)
		require.False(t, kill.At.IsZero())
		mu.Lock()
		actual := killedAt[kill.From]
		mu.Unlock()
		require.True(t, actual.Equal(kill.At))
		after = read()
		require.Equal(t, kill.To, after.Calls[0].Owner)
		require.Equal(t, uint64(i+1), after.Calls[0].TakeoverCount)
		completed := false
		for _, event := range after.Takeovers {
			if event.ID == call.SessionID() && event.From == kill.From {
				completed = !event.End.IsZero() && !event.Lost
			}
		}
		require.True(t, completed)
		healthyReplacement := false
		for _, worker := range after.Workers {
			if worker.Name == kill.Replacement {
				healthyReplacement = healthyWorker(worker)
			}
		}
		require.True(t, healthyReplacement)
		h.RecordProcessKill(kill.From, kill.At)
		time.Sleep(500 * time.Millisecond)
	}
	report, err := call.Hangup(ctx)
	require.NoError(t, err)
	require.True(t, report.ConnectedThroughout())
	require.Equal(t, 0, report.ICERestarts)
	require.Equal(t, 0, report.Renegotiations)
	require.Len(t, report.Tracks, 2)
	for _, track := range report.Tracks {
		require.Greater(t, track.Packets, 0)
	}
	page.Stop()
	// Stopping attach mode must leave the external pool and control alive.
	var external controlplane.Status
	require.NoError(t, privateapi.Do(ctx, http.DefaultClient, controlURL, http.MethodGet, "/status", nil, &external, nil))
	require.Equal(t, 2, healthyPool(external.Workers))
	manager.Stop(false)
	select {
	case <-redis.Done():
	default:
		t.Fatal("owned Redis not stopped")
	}
	mu.Lock()
	defer mu.Unlock()
	for _, worker := range workers {
		select {
		case <-worker.child.Done():
		default:
			t.Errorf("owned worker %d not stopped", worker.child.PID())
		}
	}
}
