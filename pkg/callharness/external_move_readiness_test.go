package callharness

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/privateapi"
	agentv2 "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/stretchr/testify/require"
)

func externalReadinessCall(t *testing.T, handler http.HandlerFunc) *Call {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	h, err := Start(Options{External: &ExternalTopology{SignalingURL: server.URL + "/calls", RelayAddr: netip.MustParseAddrPort("127.0.0.1:20000")}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, h.Close()) })
	return &Call{harness: h, rec: newRecorder(), remoteUfrag: "test-call"}
}
func TestExternalMoveRetriesOnlyUnstartedReadinessRefusals(t *testing.T) {
	var attempts atomic.Int32
	var effects atomic.Int32
	call := externalReadinessCall(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/calls/test-call/move?to=1", r.URL.RequestURI())
		if attempts.Add(1) < 3 {
			privateapi.Error(w, controlplane.ErrNotReady, controlplane.RemoteErrors)
			return
		}
		effects.Add(1)
		started := time.Now()
		privateapi.Write(w, controlplane.MoveResult{ID: "test-call", Kind: "move", From: "0", To: "1", Start: started, End: started.Add(time.Millisecond), Result: mediaworker.HandoverResult{Duration: time.Millisecond, Resume: time.Millisecond}})
	})
	require.NoError(t, call.MoveExternal(context.Background(), 1))
	require.EqualValues(t, 3, attempts.Load())
	require.EqualValues(t, 1, effects.Load())
	reports := call.rec.moveReports()
	require.Len(t, reports, 1, "refused attempts must not invent media events")
	require.Equal(t, time.Millisecond, reports[0].Result.Duration)
	require.Empty(t, reports[0].Error)
}
func TestExternalMoveDoesNotReplayStartedOrUncertainReadinessErrors(t *testing.T) {
	for _, tc := range []struct {
		name               string
		started, uncertain bool
	}{{name: "started", started: true}, {name: "uncertain", uncertain: true}} {
		t.Run(tc.name, func(t *testing.T) {
			var attempts atomic.Int32
			call := externalReadinessCall(t, func(w http.ResponseWriter, _ *http.Request) {
				attempts.Add(1)
				cause := error(agentv2.ErrControlNotReady)
				if tc.uncertain {
					cause = privateapi.Uncertain(cause)
				}
				result := controlplane.MoveResult{}
				if tc.started {
					result.Start = time.Now()
				}
				w.WriteHeader(http.StatusServiceUnavailable)
				privateapi.Write(w, controlplane.MoveFailure{Error: cause.Error(), Codes: privateapi.ErrorCodes(cause, controlplane.RemoteErrors), Result: result})
			})
			err := call.MoveExternal(context.Background(), 1)
			require.ErrorIs(t, err, agentv2.ErrControlNotReady)
			if tc.uncertain {
				require.ErrorIs(t, err, privateapi.ErrUncertain)
			}
			require.EqualValues(t, 1, attempts.Load())
			require.Len(t, call.rec.moveReports(), 1)
		})
	}
}
func TestExternalMoveReadinessWaitKeepsOriginalRequestBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("observes the complete two-second API budget")
	}
	var attempts atomic.Int32
	call := externalReadinessCall(t, func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		privateapi.Error(w, controlplane.ErrNotReady, controlplane.RemoteErrors)
	})
	started := time.Now()
	err := call.MoveExternal(context.Background(), 1)
	elapsed := time.Since(started)
	require.ErrorIs(t, err, agentv2.ErrControlNotReady)
	require.True(t, errors.Is(err, context.DeadlineExceeded))
	require.Greater(t, attempts.Load(), int32(1))
	require.GreaterOrEqual(t, elapsed, privateapi.Timeout)
	require.Less(t, elapsed, privateapi.Timeout+500*time.Millisecond, "retries must share the original request deadline")
	require.Len(t, call.rec.moveReports(), 1)
}
