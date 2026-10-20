package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/callharness"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/relaishq/relais/pkg/storage"
	"github.com/stretchr/testify/require"
)

// Regression: no control restart. A worker dies while a create to it is in
// flight; the existing call on that worker must still be taken over.
func TestWorkerDeathDuringCreateStillTakesOverProcesses(t *testing.T) {
	if testing.Short() {
		t.Skip("process recovery regression")
	}
	addr := standaloneRestartRedis(t)
	bin := restartBinaries(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	sys := newRestartProcesses(t, ctx, bin, addr, false)
	target, err := url.Parse(sys.workerURLs[0])
	require.NoError(t, err)
	var armed atomic.Bool
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ModifyResponse = func(response *http.Response) error {
		if response.Request.Method == http.MethodPost && strings.HasSuffix(response.Request.URL.Path, "/sessions") && armed.CompareAndSwap(true, false) {
			close(entered)
			select {
			case <-release:
			case <-response.Request.Context().Done():
			}
		}
		return nil
	}
	server := httptest.NewServer(proxy)
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
	require.True(t, restartProcessWait(ctx, 2*time.Second))
	armed.Store(true)
	createDone := make(chan error, 1)
	go func() {
		_, err := h.Dial(ctx, callharness.CallOptions{Worker: 0})
		createDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(8 * time.Second):
		t.Fatal("create never reached the worker")
	}
	sys.kill(sys.workerChildren[0])
	server.CloseClientConnections()
	t.Logf("create result after worker death: %v", <-createDone)
	var lastOwner string
	var lastTakeovers uint64
	ok := restartEventually(15*time.Second, func() bool {
		status, err := sys.status()
		if err != nil {
			return false
		}
		for _, call := range status.Calls {
			if call.ID == existing.SessionID() {
				lastOwner, lastTakeovers = call.Owner, call.TakeoverCount
				return call.Owner == "1" && call.TakeoverCount > 0
			}
		}
		return false
	})
	status, _ := sys.status()
	t.Logf("existing call owner=%s takeovers=%d workers=%+v pool=%+v", lastOwner, lastTakeovers, status.Workers, status.Pool)
	require.True(t, ok, "existing call on the dead worker was never taken over")
	require.Equal(t, 2, len(status.Calls)+int(status.LostCount), "every committed create is managed or a counted loss")
	// Takeover deliberately ignores unknown-create reservations. Their durable
	// records and capacity settle independently on the next reconciliation tick.
	require.Eventually(t, func() bool {
		status, err := sys.status()
		return err == nil && status.Pool.Reservations == 0
	}, 3*time.Second, 20*time.Millisecond)
	sys.assertOwners(t)
}

func restartEventually(limit time.Duration, check func() bool) bool {
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if check() {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// Regression: a worker with live calls dies while control is down. The successor
// starts with that worker down and must take over or count every call.
func TestWorkerDiesWhileControlDownProcesses(t *testing.T) {
	for _, delay := range []time.Duration{0, time.Second} {
		t.Run(delay.String(), func(t *testing.T) { testWorkerDiesWhileControlDown(t, delay) })
	}
}
func testWorkerDiesWhileControlDown(t *testing.T, preStart time.Duration) {
	if testing.Short() {
		t.Skip("process recovery regression")
	}
	addr := standaloneRestartRedis(t)
	bin := restartBinaries(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	sys := newRestartProcesses(t, ctx, bin, addr, false)
	sys.startControl()
	h := sys.harness(t)
	calls := make([]*callharness.Call, 16)
	for i := range calls {
		var err error
		calls[i], err = h.Dial(ctx, callharness.CallOptions{Worker: 1})
		require.NoError(t, err)
	}
	flowCtx, stopFlow := context.WithCancel(ctx)
	var flow sync.WaitGroup
	for _, call := range calls {
		flow.Go(func() { _ = call.SendMedia(flowCtx, 40*time.Second) })
	}
	defer func() { stopFlow(); flow.Wait() }()
	require.True(t, restartProcessWait(ctx, 2*time.Second))
	sys.kill(sys.controlChild)
	sys.kill(sys.workerChildren[1])
	require.True(t, restartProcessWait(ctx, preStart))
	elapsed := sys.startControl()
	t.Logf("REBUILD_DOWN delay=%s process_to_ready=%s calls=%d", preStart, elapsed, len(calls))
	settled := restartEventually(15*time.Second, func() bool {
		status, err := sys.status()
		if err != nil {
			return false
		}
		for _, call := range status.Calls {
			if call.Owner != "0" || call.TakeoverCount == 0 {
				return false
			}
		}
		return true
	})
	status, err := sys.status()
	require.NoError(t, err)
	for _, takeover := range status.Takeovers {
		t.Logf("takeover id=%s to=%s lost=%t cause=%s checkpoint_age=%s", takeover.ID, takeover.To, takeover.Lost, takeover.LossCause, takeover.CheckpointAge)
	}
	t.Logf("calls=%d lost=%d outcomes=%+v", len(status.Calls), status.LostCount, status.Rebuild.Outcomes)
	require.True(t, settled, "every remaining call must be owned by worker 0 after takeover")
	require.Len(t, status.Calls, len(calls))
	require.Zero(t, status.LostCount)
	sys.assertOwners(t)
}

// Regression: a create reaches a worker that already crashed (connection refused)
// before detection. Nothing can have been accepted, yet takeover must proceed.
func TestCreateToAlreadyDeadWorkerStillTakesOverProcesses(t *testing.T) {
	if testing.Short() {
		t.Skip("process recovery regression")
	}
	addr := standaloneRestartRedis(t)
	bin := restartBinaries(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	sys := newRestartProcesses(t, ctx, bin, addr, false)
	sys.startControl()
	h := sys.harness(t)
	existing, err := h.Dial(ctx, callharness.CallOptions{Worker: 0})
	require.NoError(t, err)
	flowCtx, stopFlow := context.WithCancel(ctx)
	flowDone := make(chan struct{})
	go func() { defer close(flowDone); _ = existing.SendMedia(flowCtx, 40*time.Second) }()
	defer func() { stopFlow(); <-flowDone }()
	require.True(t, restartProcessWait(ctx, 2*time.Second))
	sys.kill(sys.workerChildren[0])
	_, createErr := h.Dial(ctx, callharness.CallOptions{Worker: 0})
	t.Logf("create to crashed worker: %v", createErr)
	var lastOwner string
	ok := restartEventually(15*time.Second, func() bool {
		status, err := sys.status()
		if err != nil {
			return false
		}
		for _, call := range status.Calls {
			if call.ID == existing.SessionID() {
				lastOwner = call.Owner
				return call.Owner == "1" && call.TakeoverCount > 0
			}
		}
		return false
	})
	status, _ := sys.status()
	t.Logf("existing owner=%s reservations=%d lost=%d", lastOwner, status.Pool.Reservations, status.LostCount)
	require.True(t, ok, "existing call on the crashed worker was never taken over")
	require.Len(t, status.Calls, 1)
	require.Zero(t, status.LostCount)
	sys.assertOwners(t)
}

func TestControlRestartWithCreateOnDeadWorkerProcesses(t *testing.T) {
	if testing.Short() {
		t.Skip("process recovery regression")
	}
	addr := standaloneRestartRedis(t)
	bin := restartBinaries(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	sys := newRestartProcesses(t, ctx, bin, addr, false)
	target, err := url.Parse(sys.workerURLs[0])
	require.NoError(t, err)
	var armed atomic.Bool
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ModifyResponse = func(response *http.Response) error {
		if response.Request.Method == http.MethodPost && strings.HasSuffix(response.Request.URL.Path, "/sessions") && armed.CompareAndSwap(true, false) {
			close(entered)
			select {
			case <-release:
			case <-response.Request.Context().Done():
			}
		}
		return nil
	}
	server := httptest.NewServer(proxy)
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
	require.True(t, restartProcessWait(ctx, 2*time.Second))
	armed.Store(true)
	createDone := make(chan error, 1)
	go func() {
		_, err := h.Dial(ctx, callharness.CallOptions{Worker: 0})
		createDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(8 * time.Second):
		t.Fatal("create never reached the worker")
	}
	sys.kill(sys.controlChild)
	sys.kill(sys.workerChildren[0])
	server.CloseClientConnections()
	t.Logf("create interrupted by control and worker death: %v", <-createDone)
	elapsed := sys.startControl()
	t.Logf("CREATE_REBUILD process_to_ready=%s", elapsed)
	var lastOwner string
	var lastTakeovers uint64
	ok := restartEventually(15*time.Second, func() bool {
		status, err := sys.status()
		if err != nil {
			return false
		}
		for _, call := range status.Calls {
			if call.ID == existing.SessionID() {
				lastOwner, lastTakeovers = call.Owner, call.TakeoverCount
				return call.Owner == "1" && call.TakeoverCount > 0
			}
		}
		return false
	})
	status, _ := sys.status()
	t.Logf("existing call owner=%s takeovers=%d workers=%+v pool=%+v", lastOwner, lastTakeovers, status.Workers, status.Pool)
	require.True(t, ok, "existing call on the dead worker was never taken over")
	require.Equal(t, 2, len(status.Calls)+int(status.LostCount), "every committed create is managed or a counted loss")
	require.Zero(t, status.Pool.Reservations)
	sys.assertOwners(t)
}

func TestControlGracefulStopRetainsLeaseForHandlersProcesses(t *testing.T) {
	if testing.Short() {
		t.Skip("separate process shutdown")
	}
	addr := standaloneRestartRedis(t)
	bin := restartBinaries(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sys := newRestartProcesses(t, ctx, bin, addr, false)
	target, err := url.Parse(sys.workerURLs[0])
	require.NoError(t, err)
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	var armed atomic.Bool
	defer releaseOnce.Do(func() { close(release) })
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ModifyResponse = func(response *http.Response) error {
		if response.Request.Method == http.MethodPost && strings.HasSuffix(response.Request.URL.Path, "/sessions") && armed.CompareAndSwap(true, false) {
			close(entered)
			select {
			case <-release:
			case <-response.Request.Context().Done():
			}
		}
		return nil
	}
	server := httptest.NewServer(proxy)
	defer server.Close()
	sys.workerURLs[0] = server.URL
	sys.startControl()
	h := sys.harness(t)
	existing, err := h.Dial(ctx, callharness.CallOptions{Worker: 0})
	require.NoError(t, err)
	mediaCtx, stopMedia := context.WithCancel(ctx)
	mediaDone := make(chan struct{})
	go func() { defer close(mediaDone); _ = existing.SendMedia(mediaCtx, 15*time.Second) }()
	defer func() { stopMedia(); <-mediaDone }()
	armed.Store(true)
	created := make(chan error, 1)
	go func() { _, err := h.Dial(ctx, callharness.CallOptions{Worker: 0}); created <- err }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("create did not reach worker")
	}
	prefix := ""
	for _, entry := range sys.env {
		if strings.HasPrefix(entry, "RELAIS_REDIS_PREFIX=") {
			prefix = strings.TrimPrefix(entry, "RELAIS_REDIS_PREFIX=")
		}
	}
	store, err := sessionstore.NewRedis(ctx, storage.RedisConfig{Addr: addr, Prefix: prefix}, make([]byte, 32))
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()
	before, err := store.Get(ctx, existing.SessionID())
	require.NoError(t, err)
	started := time.Now()
	require.NoError(t, sys.controlChild.SignalGroup(syscall.SIGTERM))
	waiting := true
	for waiting {
		select {
		case <-created:
			waiting = false
			continue
		case <-time.After(20 * time.Millisecond):
		}
		lease, claimErr := store.ClaimControl(ctx, "shutdown-contender", controlplane.ControlLeaseTTL)
		if claimErr == nil {
			require.NoError(t, store.ReleaseControl(ctx, lease))
			select {
			case <-created:
				waiting = false
			default:
				t.Error("control lease lapsed while create handler was still running")
				waiting = false
			}
		} else {
			require.ErrorIs(t, claimErr, sessionstore.ErrLeaseHeld)
		}
	}
	<-sys.controlChild.Done()
	after, err := store.Get(ctx, existing.SessionID())
	require.NoError(t, err)
	require.Equal(t, before.Worker, after.Worker, "graceful control stop changed a live call owner")
	require.Equal(t, before.Epoch, after.Epoch, "graceful control stop triggered takeover")

	require.Greater(t, time.Since(started), time.Second)
	t.Logf("GRACEFUL shutdown handler drain=%s", time.Since(started))
}
