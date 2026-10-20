package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/callharness"
	"github.com/stretchr/testify/require"
)

func TestControlRestartWorkerStatusLivenessProcesses(t *testing.T) {
	if testing.Short() {
		t.Skip("separate-process worker liveness and takeover timing")
	}
	addr := standaloneRestartRedis(t)
	bin := restartBinaries(t)
	for _, tc := range []struct {
		name      string
		delay     time.Duration
		blackHole bool
	}{
		{name: "300ms", delay: 300 * time.Millisecond},
		{name: "900ms", delay: 900 * time.Millisecond},
		{name: "black-hole", blackHole: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			sys := newRestartProcesses(t, ctx, bin, addr, false)
			original := sys.workerURLs[1]
			target, err := url.Parse(original)
			require.NoError(t, err)
			proxy := httputil.NewSingleHostReverseProxy(target)
			var armed atomic.Bool
			blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if armed.Load() && r.Method == http.MethodGet && r.URL.Path == "/status" {
					if tc.blackHole {
						<-r.Context().Done()
						return
					}
					if !restartProcessWait(r.Context(), tc.delay) {
						return
					}
				}
				proxy.ServeHTTP(w, r)
			}))
			defer blocked.Close()
			sys.workerURLs[1] = blocked.URL
			sys.startControl()
			h := sys.harness(t)
			count := 1
			if tc.blackHole {
				count = 16
			}
			calls := make([]*callharness.Call, count)
			for i := range calls {
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
			before, err := sys.status()
			require.NoError(t, err)
			epochs := map[string]uint64{}
			for _, call := range before.Calls {
				epochs[call.ID] = call.Epoch
			}
			sys.kill(sys.controlChild)
			armed.Store(true)
			if tc.blackHole {
				sys.kill(sys.workerChildren[1])
			}
			started := time.Now()
			ready := sys.startControl()
			if tc.blackHole {
				var completed time.Duration
				recovered := restartEventually(3*time.Second, func() bool {
					status, err := sys.status()
					if err != nil || len(status.Calls) != count || status.LostCount != 0 || len(status.Takeovers) != count {
						return false
					}
					for _, call := range status.Calls {
						if call.Owner != "0" || call.TakeoverCount == 0 {
							return false
						}
					}
					for _, takeover := range status.Takeovers {
						completed = max(completed, takeover.End.Sub(started))
					}
					return true
				})
				observed := time.Since(started)
				t.Logf("BLACK_HOLE process_to_ready=%s process_to_takeover=%s observed_takeover=%s default_rate_limit=%s headroom=%s calls=%d", ready, completed, observed, defaultFreshSnapshotAgeBound, defaultFreshSnapshotAgeBound-completed, count)
				require.True(t, recovered, "every call must recover after heartbeat detection")
				require.Positive(t, completed, "must observe all completed takeovers")
				require.Less(t, completed, defaultFreshSnapshotAgeBound-250*time.Millisecond, "lease wait, unknown-worker grace and takeover must fit with headroom")
			} else {
				require.Never(t, func() bool {
					status, err := sys.status()
					if err != nil || len(status.Calls) != count || status.LostCount != 0 {
						return true
					}
					for _, call := range status.Calls {
						if call.Owner != "1" || call.Epoch != epochs[call.ID] || call.TakeoverCount != 0 {
							return true
						}
					}
					for _, worker := range status.Workers {
						if worker.Name == "1" && (worker.Dead || worker.Recovering) {
							return true
						}
					}
					return false
				}, 1300*time.Millisecond, 20*time.Millisecond, "a slow status worker that keeps heartbeating must retain its calls")
				t.Logf("SLOW_LIVE status_delay=%s process_to_ready=%s evictions=0 calls=%d", tc.delay, ready, count)
			}
			status, err := sys.status()
			require.NoError(t, err)
			require.Len(t, status.Calls, count)
			require.Zero(t, status.LostCount)
			armed.Store(false)
			sys.assertOwners(t)
		})
	}
}
