package loadgen

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/callharness"
)

func TestCancelledCallerPollDoesNotFailAnotherHangup(t *testing.T) {
	system, err := callharness.Start(callharness.Options{Relay: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = system.Close() }()
	target, err := url.Parse(strings.TrimSuffix(system.SignalingURL(), "/calls"))
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/status" {
			if requests.Add(1) == 1 {
				close(entered)
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
			}
		}
		proxy.ServeHTTP(w, r)
	}))
	defer server.Close()
	poller := newStatusPoller(server.URL+"/calls", &Meter{}, nil)
	h, err := callharness.Start(callharness.Options{External: &callharness.ExternalTopology{SignalingURL: server.URL + "/calls", RelayAddr: system.RelayAddr(), Status: poller.Get, FinalStatus: poller.GetFinal}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.Close() }()
	ctx, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	b, err := h.Dial(ctx, callharness.CallOptions{Worker: -1})
	if err != nil {
		t.Fatal(err)
	}
	// A's maintenance context is cancelled by close(), while its status read
	// is in flight. B then performs the actual final collection in Hangup.
	a, cancelA := context.WithCancel(ctx)
	defer cancelA()
	done := make(chan error, 1)
	go func() { _, err := h.Status(a); done <- err }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("status request never started")
	}
	cancelA()
	unblock()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("A error = %v", err)
	}
	report, err := b.Hangup(ctx)
	if err != nil || report == nil {
		t.Fatalf("A cancellation poisoned B's final collection: %v", err)
	}
	if requests.Load() != 2 {
		t.Fatalf("final collection did not refresh status: %d requests", requests.Load())
	}
}

func TestFinalStatusRefreshSurvivesSlowPeriodicTimeout(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		timer := time.NewTimer(1100 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
			_ = json.NewEncoder(w).Encode(map[string]any{"calls": []any{}})
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	meter := &Meter{}
	poller := newStatusPoller(server.URL+"/calls", meter, nil)
	if poller.client.Timeout != 0 || poller.pollTimeout != time.Second || poller.finalTimeout != 10*time.Second {
		t.Fatal("final collection still has a hard one-second client deadline")
	}
	if _, err := poller.Get(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("periodic poll should time out: %v", err)
	}
	// The fresh final read bypasses that cached timeout. Its longer deadline
	// permits the same slow endpoint, and all final readers share its result.
	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := poller.GetFinal(context.Background()); err != nil {
				t.Errorf("final status: %v", err)
			}
		}()
	}
	wg.Wait()
	if requests.Load() != 2 || meter.sample().requests != 2 {
		t.Fatalf("final readers did not share one fresh request: %d", requests.Load())
	}
}

func TestCallerDeadlineIsNotCachedAsPollerError(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var requests atomic.Int64
	var releaseOnce, enterOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		enterOnce.Do(func() { close(entered) })
		select {
		case <-release:
			_ = json.NewEncoder(w).Encode(map[string]any{"calls": []any{}})
		case <-r.Context().Done():
		}
	}))
	defer func() { unblock(); server.Close() }()
	poller := newStatusPoller(server.URL+"/calls", &Meter{}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := poller.Get(ctx); done <- err }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("poll did not start")
	}
	<-ctx.Done()
	unblock()
	if err := <-done; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("caller deadline: %v", err)
	}
	if _, err := poller.Get(context.Background()); err != nil {
		t.Fatalf("caller deadline was cached: %v", err)
	}
	if requests.Load() != 1 {
		t.Fatal("successful shared snapshot was discarded")
	}
}
