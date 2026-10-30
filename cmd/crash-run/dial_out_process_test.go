//go:build !windows

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/clusterprocess"
	"github.com/relaishq/relais/internal/redistestenv"
	"github.com/relaishq/relais/pkg/callharness"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/stretchr/testify/require"
)

// This opt-in local process gate uses the same process manager and caller as
// crash-run. Default unit jobs do not assume that process binaries exist.
func TestDialOutProcessMoveRestartAndSIGKILL(t *testing.T) {
	if os.Getenv("RELAIS_TEST_DIAL_OUT_PROCESS") != "1" {
		t.Skip("set RELAIS_TEST_DIAL_OUT_PROCESS=1 after make build")
	}
	addr := redistestenv.Address(t)
	require.NotEmpty(t, addr)
	bin, err := filepath.Abs("../../bin")
	require.NoError(t, err)
	for _, name := range []string{"relais-control", "relais-worker", "relais-relay"} {
		_, err := os.Stat(filepath.Join(bin, name))
		require.NoError(t, err)
	}
	var key [32]byte
	_, err = rand.Read(key[:])
	require.NoError(t, err)
	env := processEnv(hex.EncodeToString(key[:]), addr, "dial-out-process:"+rand.Text()+":")
	manager := &clusterprocess.Manager{}

	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
	defer cancel()
	dir, err := os.MkdirTemp(bin, "dial-out-process-")
	require.NoError(t, err)
	t.Cleanup(func() {
		manager.Stop(true)
		if !t.Failed() && os.Getenv("RELAIS_TEST_KEEP_PROCESS_LOGS") != "1" {
			_ = os.RemoveAll(dir)
		}
	})
	t.Logf("process logs: %s", dir)
	topology, err := newPairTopology(manager, dir, env, "", 0, 0)
	require.NoError(t, err)
	release := &pairRelease{Dir: bin, Version: "local"}
	require.NoError(t, topology.startRelay(ctx, release))
	controlArgs := []string{release.binary("relais-control"), "-http", topology.controlAddr, "-relay", "http://" + topology.relayHTTP, "-worker", "0=dial-out", "-worker", "1=dial-out"}
	startControl := func() *pairProc {
		p, err := topology.launch("control", "control", release, env, controlArgs...)
		require.NoError(t, err)
		require.NoError(t, topology.ready(ctx, p, 5*time.Second))
		topology.control = p
		return p
	}
	control := startControl()
	for i := range topology.workers {
		args := []string{release.binary("relais-worker"), "-connect=dial-out", "-name", strconv.Itoa(i), "-media", topology.workerMedia[i], "-relay-leg", topology.relayLeg, "-relay-media", topology.relayMedia, "-control", "http://" + topology.controlAddr}
		p, err := topology.launch(pairWorkerName(i), "worker", release, env, args...)
		require.NoError(t, err)
		topology.workers[i] = p
		require.NoError(t, topology.ready(ctx, p, 8*time.Second))
		require.Empty(t, p.Ready.HTTP, "dial-out workers must not need an HTTP listener")
	}
	_, err = topology.waitControl(ctx, 8*time.Second, "dial-out readiness", func(s controlplane.Status) bool { return s.Ready && len(s.Workers) == 2 })
	require.NoError(t, err)
	h, err := callharness.Start(callharness.Options{External: &callharness.ExternalTopology{SignalingURL: "http://" + topology.controlAddr + "/calls", RelayAddr: netip.MustParseAddrPort(topology.relayMedia)}})
	require.NoError(t, err)
	defer func() { _ = h.Close() }()
	call, err := h.Dial(ctx, callharness.CallOptions{Video: true, Worker: 0})
	require.NoError(t, err)
	mediaCtx, stop := context.WithCancel(ctx)
	defer stop()
	sent := make(chan error, 1)
	go func() { sent <- call.SendMedia(mediaCtx, 60*time.Second) }()
	defer func() {
		stop()
		select {
		case <-sent:
		case <-time.After(5 * time.Second):
			t.Error("caller media sender did not stop")
		}
	}()
	time.Sleep(600 * time.Millisecond)
	for i := range 20 {
		require.NoError(t, call.MoveExternal(ctx, (i+1)%2))
		time.Sleep(200 * time.Millisecond)
	}
	// SIGKILL control while the same worker processes retain their media/leases.
	restartedAt := time.Now()
	require.NoError(t, control.Child.SignalGroup(syscall.SIGKILL))
	select {
	case <-control.Child.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("control did not exit")
	}
	control = startControl()
	_ = control
	status, err := topology.waitControl(ctx, 8*time.Second, "restarted control with original links", func(s controlplane.Status) bool {
		return s.Ready && pairWorkerLive(s, "0", restartedAt) && pairWorkerLive(s, "1", restartedAt) && pairWorker(s, "0").Link != nil && pairWorker(s, "0").Link.Connected && pairWorker(s, "1").Link != nil && pairWorker(s, "1").Link.Connected
	})
	require.NoError(t, err)
	restartElapsed := time.Since(restartedAt)
	require.Empty(t, status.Takeovers)
	require.Zero(t, status.LostCount)
	require.Len(t, status.Calls, 1)
	require.Equal(t, "0", status.Calls[0].Owner)
	// Both workers' counters are readable solely through their outbound links.
	checkWorker := func(name string) {
		values, available, err := scrapeMetrics(ctx, &http.Client{Timeout: 2 * time.Second}, "http://"+topology.controlAddr+"/private/workers/"+name+"/metrics")
		require.NoError(t, err)
		require.True(t, available["relais_worker_decryption_failures_total"])
		require.Zero(t, values["relais_worker_decryption_failures_total"])
	}
	checkWorker("0")
	checkWorker("1")
	killed := time.Now()
	h.RecordProcessKill("0", killed)
	require.NoError(t, topology.workers[0].Child.SignalGroup(syscall.SIGKILL))
	select {
	case <-topology.workers[0].Child.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not exit")
	}
	status, err = topology.waitControl(ctx, 2*time.Second, "SIGKILL takeover", func(s controlplane.Status) bool { return len(s.Takeovers) == 1 })
	require.NoError(t, err)
	takeoverElapsed := time.Since(killed)
	require.Less(t, takeoverElapsed, 2*time.Second)
	require.Zero(t, status.LostCount)
	require.False(t, status.Takeovers[0].Lost)
	require.Equal(t, "1", status.Calls[0].Owner)
	time.Sleep(2 * time.Second)
	checkWorker("1")
	report, err := call.Hangup(ctx)
	require.NoError(t, err)
	require.True(t, report.ConnectedThroughout())
	require.Zero(t, report.DecryptionFailures.Total())
	require.Zero(t, report.ICERestarts)
	require.Zero(t, report.Renegotiations)
	var gaps []time.Duration
	for _, move := range report.Moves {
		if move.Kind == "takeover" {
			continue
		}
		var gap time.Duration
		for _, track := range move.Tracks {
			gap = max(gap, track.Gap)
		}
		gaps = append(gaps, gap)
	}
	require.Len(t, gaps, 20)
	slices.Sort(gaps)
	p95 := gaps[18]
	require.LessOrEqual(t, p95, 150*time.Millisecond)
	for _, track := range report.Tracks {
		require.Less(t, track.MediaGap, 2*time.Second)
	}
	t.Logf("DIAL_OUT_PROCESS moves=20 pooled_p95=%s control_restart=%s takeover=%s false_takeovers_before_kill=0 caller_decryption_failures=0 observed_worker_decryption_failures=0 logs=%s", p95, restartElapsed, takeoverElapsed, topology.dir)
}
