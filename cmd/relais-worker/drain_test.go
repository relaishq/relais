package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relais/internal/privateapi"
	"github.com/relais/pkg/controlplane"
	"github.com/stretchr/testify/require"
)

func TestDrainBeforeWorkerClose(t *testing.T) {
	var count atomic.Int32
	count.Store(1)
	requested := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/workers/worker-0/drain", r.URL.Path)
		require.Equal(t, http.MethodPost, r.Method)
		close(requested)
		privateapi.Write(w, controlplane.DrainResult{})
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- drainWorker(ctx, server.URL, "worker-0", func() int { return int(count.Load()) }) }()
	<-requested
	select {
	case <-done:
		t.Fatal("drain returned before the worker was empty")
	case <-time.After(40 * time.Millisecond):
	}
	count.Store(0)
	require.NoError(t, <-done)
}
func TestDrainBoundedWhenSessionsRemain(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { privateapi.Write(w, controlplane.DrainResult{}) }))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, drainWorker(ctx, server.URL, "0", func() int { return 1 }), context.DeadlineExceeded)
}
func TestDrainUnreachable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.Error(t, drainWorker(ctx, "http://127.0.0.1:1", "0", func() int { return 1 }))
}

func TestPartialDrainWaitsForRemainingSessions(t *testing.T) {
	var count atomic.Int32
	count.Store(1)
	requested := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(requested)
		privateapi.Write(w, controlplane.DrainResult{Error: "one move is still pending"})
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- drainWorker(ctx, server.URL, "0", func() int { return int(count.Load()) }) }()
	<-requested
	select {
	case <-done:
		t.Fatal("partial drain closed a still-running session before the deadline")
	case <-time.After(40 * time.Millisecond):
	}
	count.Store(0)
	require.ErrorContains(t, <-done, "one move is still pending")
}
