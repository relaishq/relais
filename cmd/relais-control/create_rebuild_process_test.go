package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/callharness"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/stretchr/testify/require"
)

func TestWorkerDiesDuringSuccessorRebuildWithCreateRecordProcesses(t *testing.T) {
	if testing.Short() {
		t.Skip("separate process rebuild recovery")
	}
	addr := standaloneRestartRedis(t)
	bin := restartBinaries(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	sys := newRestartProcesses(t, ctx, bin, addr, false)
	target, err := url.Parse(sys.workerURLs[0])
	require.NoError(t, err)
	var gateArmed, killArmed, failStatus atomic.Bool
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ModifyResponse = func(response *http.Response) error {
		if response.Request.Method == http.MethodPost && strings.HasSuffix(response.Request.URL.Path, "/sessions") && gateArmed.CompareAndSwap(true, false) {
			close(entered)
			select {
			case <-release:
			case <-response.Request.Context().Done():
			}
		}
		if response.Request.Method == http.MethodGet && response.Request.URL.Path == "/status" && killArmed.CompareAndSwap(true, false) {
			failStatus.Store(true)
			go func() { _ = sys.workerChildren[0].SignalGroup(syscall.SIGKILL) }()
		}
		return nil
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failStatus.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	defer server.Close()
	sys.workerURLs[0] = server.URL
	sys.startControl()
	h := sys.harness(t)
	existing, err := h.Dial(ctx, callharness.CallOptions{Worker: 0})
	require.NoError(t, err)
	flowCtx, stopFlow := context.WithCancel(ctx)
	flowDone := make(chan struct{})
	go func() { defer close(flowDone); _ = existing.SendMedia(flowCtx, 40*time.Second) }()
	defer func() { stopFlow(); <-flowDone }()
	gateArmed.Store(true)
	created := make(chan error, 1)
	go func() { _, err := h.Dial(ctx, callharness.CallOptions{Worker: 0}); created <- err }()
	select {
	case <-entered:
	case <-time.After(8 * time.Second):
		t.Fatal("create never reached worker 0")
	}
	sys.kill(sys.controlChild)
	releaseOnce.Do(func() { close(release) })
	<-created
	killArmed.Store(true)
	sys.generation++
	started := time.Now()
	sys.controlChild = sys.start("control-successor", filepath.Join(sys.bin, "relais-control"), "-http", sys.control, "-relay", "http://"+sys.relay, "-worker", "0="+sys.workerURLs[0], "-worker", "1="+sys.workerURLs[1])
	var last controlplane.Status
	ready := restartEventually(15*time.Second, func() bool {
		status, err := sys.status()
		if err == nil {
			last = status
		}
		return err == nil && status.Ready
	})
	if last.Rebuild != nil {
		t.Logf("successor ready=%t after=%s phase=%s error=%q", last.Ready, time.Since(started), last.Rebuild.Phase, last.Rebuild.Error)
	}
	require.True(t, ready, "successor never became mutation-ready after worker 0 died during its rebuild")
	recovered := restartEventually(10*time.Second, func() bool {
		status, err := sys.status()
		if err != nil {
			return false
		}
		for _, call := range status.Calls {
			if call.ID == existing.SessionID() {
				return call.Owner == "1" && call.TakeoverCount > 0
			}
		}
		return false
	})
	require.True(t, recovered, "worker 0's existing call must be recovered")
	require.Eventually(t, func() bool {
		status, err := sys.status()
		return err == nil && status.Pool.Reservations == 0 && len(status.Calls)+int(status.LostCount) == 2
	}, 3*time.Second, 20*time.Millisecond)
	status, err := sys.status()
	require.NoError(t, err)
	t.Logf("settled calls=%d losses=%d reservations=%d", len(status.Calls), status.LostCount, status.Pool.Reservations)
	sys.assertOwners(t)
}
