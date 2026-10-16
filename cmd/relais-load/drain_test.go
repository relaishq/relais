package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/capacity"
	"github.com/relaishq/relais/internal/loadgen"
	"github.com/stretchr/testify/require"
)

func TestDrainTimingEmptyConfirmationAndPartialFailure(t *testing.T) {
	for _, mode := range []string{"complete", "partial", "not-empty"} {
		t.Run(mode, func(t *testing.T) {
			var statusRead, drainReturned, fanoutAt time.Time
			var mu sync.Mutex
			control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if r.URL.Path == "/status" {
					statusRead = time.Now()
					_ = json.NewEncoder(w).Encode(map[string]any{"calls": []map[string]string{{"id": "a", "owner": "0"}}})
					return
				}
				drainReturned = time.Now()
				if mode == "partial" {
					w.WriteHeader(http.StatusMultiStatus)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"moves": []map[string]string{{"id": "a"}}})
			}))
			defer control.Close()
			emptyChecked := false
			worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				emptyChecked = true
				sessions := 0
				if mode == "not-empty" {
					sessions = 1
				}
				_ = json.NewEncoder(w).Encode(map[string]int{"sessions": sessions})
			}))
			defer worker.Close()
			caller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				fanoutAt = time.Now()
				time.Sleep(10 * time.Millisecond)
			}))
			defer caller.Close()
			topology := &topology{External: externalTopology(control.URL+"/calls", netip.MustParseAddrPort("127.0.0.1:20000")), Targets: []capacity.Target{{Name: "worker-0", URL: worker.URL}}}
			result := runEvent(context.Background(), topology, []generator{{url: caller.URL}}, loadgen.Event{Kind: "drain", Worker: 0})
			mu.Lock()
			defer mu.Unlock()
			require.False(t, result.At.Before(statusRead))
			require.False(t, result.FinishedAt.Before(drainReturned))
			require.True(t, result.FinishedAt.Before(fanoutAt), "drain duration must exclude result fan-out")
			require.True(t, emptyChecked)
			if mode == "complete" {
				require.Empty(t, result.Error)
				require.False(t, result.Failed)
				return
			}
			require.NotEmpty(t, result.Error)
			require.True(t, result.Failed)
			r := capacityFixture()
			r.Callers = 1
			r.Events = []loadgen.EventResult{result}
			r.Processes[0].Complete = true
			r.Judge()
			require.Equal(t, "inconclusive", r.Outcome, "event records are incomplete")
			require.Equal(t, "fail", judgeCapacity("drain", r, 100, 128<<10).Verdict, "partial drain is a known failure with valid generators")
			r.Processes[0].Saturated = true
			require.Equal(t, "inconclusive", judgeCapacity("drain", r, 100, 128<<10).Verdict)
		})
	}
}
