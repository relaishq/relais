package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/callharness"
	"github.com/stretchr/testify/require"
)

func TestSlowCreateStatusBlipKeepsLiveWorkerProcesses(t *testing.T) {
	if testing.Short() {
		t.Skip("separate process create reconciliation")
	}
	addr := standaloneRestartRedis(t)
	bin := restartBinaries(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	sys := newRestartProcesses(t, ctx, bin, addr, false)
	target, err := url.Parse(sys.workerURLs[0])
	require.NoError(t, err)
	var armed, failStatus atomic.Bool
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ModifyResponse = func(response *http.Response) error {
		if response.Request.Method == http.MethodPost && strings.HasSuffix(response.Request.URL.Path, "/sessions") && armed.CompareAndSwap(true, false) {
			time.AfterFunc(1900*time.Millisecond, func() { failStatus.Store(true) })
			time.AfterFunc(3400*time.Millisecond, func() { failStatus.Store(false) })
			time.Sleep(2500 * time.Millisecond)
		}
		return nil
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/status" && failStatus.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
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
	require.True(t, restartProcessWait(ctx, 2*time.Second))
	armed.Store(true)
	_, createErr := h.Dial(ctx, callharness.CallOptions{Worker: 0})
	t.Logf("slow create result: %v", createErr)
	require.Error(t, createErr)
	moved := restartEventually(8*time.Second, func() bool {
		status, err := sys.status()
		if err != nil {
			return false
		}
		for _, call := range status.Calls {
			if call.ID == existing.SessionID() {
				return call.TakeoverCount > 0
			}
		}
		return false
	})
	status, err := sys.status()
	require.NoError(t, err)
	for _, w := range status.Workers {
		t.Logf("worker name=%s dead=%t recovering=%t calls=%d", w.Name, w.Dead, w.Recovering, w.Calls)
		require.False(t, w.Dead)
		require.False(t, w.Recovering)
	}
	found := false
	for _, call := range status.Calls {
		t.Logf("call id=%s owner=%s takeovers=%d", call.ID, call.Owner, call.TakeoverCount)
		if call.ID == existing.SessionID() {
			found = true
			require.Equal(t, "0", call.Owner)
			require.Zero(t, call.TakeoverCount)
		}
	}
	require.False(t, moved, "a live worker's call was taken over after one slow create and a status blip")
	require.True(t, found, "the original call must remain live")
	require.Len(t, status.Calls, 2, "the unknown create is adopted after status recovers")
	require.Zero(t, status.LostCount)
	require.Zero(t, status.Pool.Reservations)
	sys.assertOwners(t)
}
