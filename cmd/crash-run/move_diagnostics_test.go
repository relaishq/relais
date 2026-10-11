package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/relais/internal/movediag"
	"github.com/relais/internal/nettopology"
	"github.com/relais/internal/privateapi"
	"github.com/relais/pkg/mediaworker"
	"github.com/stretchr/testify/require"
)

func TestDiagnosticDeltasDoNotFabricateMissingOrResetCounters(t *testing.T) {
	before := []nettopology.TCPSnapshot{{Role: "control", Namespace: "owned", Counters: map[string]uint64{"TcpRetransSegs": 5, "TCPLossProbes": 8}}}
	after := []nettopology.TCPSnapshot{{Role: "control", Namespace: "owned", Counters: map[string]uint64{"TcpRetransSegs": 7, "TCPLossProbes": 1}}}
	deltas := tcpDeltas(before, after)
	require.Equal(t, map[string]uint64{"TcpRetransSegs": 2}, deltas[0].Counters)
	require.Contains(t, deltas[0].Unavailable, "TCPLossProbes")
	require.Contains(t, deltas[0].Unavailable, "TCPTimeouts")
	d := &moveDiagnostics{Deltas: deltas, Events: []movediag.Event{{TraceID: "measured", Kind: "move"}}}
	d.selectMove("measured", true)
	require.False(t, d.Complete)
}
func TestDiagnosticsExcludeProbesAndSummarizeSlowestMove(t *testing.T) {
	d := &moveDiagnostics{Events: []movediag.Event{{TraceID: "probe", Kind: "http", Phase: "resume"}, {TraceID: "measured", Kind: "move"}, {TraceID: "measured", Kind: "http", Phase: "export", Duration: 210 * time.Millisecond, Operation: "POST /sessions/:id/export"}, {TraceID: "measured", Kind: "redis", Phase: "transfer", Operation: "EVALSHA"}}}
	d.selectMove("measured", true)
	require.True(t, d.Complete)
	require.Len(t, d.Events, 3)
	require.Equal(t, 1, d.Phases["export"].HTTPRequests)
	require.Equal(t, 1, d.Phases["transfer"].RedisCommands)
	require.Zero(t, d.Phases["resume"].HTTPRequests)
	report := profileReport{Scenario: "move", Rows: []profileRow{{Trial: 1, AudioGap: 140 * time.Millisecond, EventResult: mediaworker.HandoverResult{Duration: 25 * time.Millisecond}}, {Trial: 2, AudioGap: 130 * time.Millisecond, EventResult: mediaworker.HandoverResult{Duration: 240 * time.Millisecond}, MoveDiagnostics: d}}}
	line := profileResultLine(report)
	require.Contains(t, line, "slowest_trial=1")
	require.Contains(t, line, "slowest_move_trial=2")
	require.Contains(t, line, "longest_call_duration=210ms")
}
func TestProxyDiagnosticsRecordUpstreamBodyAndReuse(t *testing.T) {
	t.Setenv("RELAIS_MOVE_DIAGNOSTICS", "1")
	target := httptest.NewServer(movediag.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NotEmpty(t, movediag.ID(r.Context()))
		privateapi.Write(w, map[string]string{"reply": "secret"})
	})))
	defer target.Close()
	proxy, err := newUncertainProxy("127.0.0.1", target.URL)
	require.NoError(t, err)
	defer proxy.Close()
	ctx := movediag.Phase(movediag.Begin(context.Background()), "export")
	for i := 0; i < 2; i++ {
		var output map[string]string
		require.NoError(t, privateapi.Do(ctx, &http.Client{}, proxy.URL, http.MethodPost, "/sessions/private-id/export", nil, &output, nil))
	}
	d := collectMoveDiagnostics(context.Background(), nil, nil, nil)
	var upstream []movediag.Event
	for _, e := range d.Events {
		if e.TraceID == movediag.ID(ctx) && e.Kind == "http_proxy" {
			upstream = append(upstream, e)
		}
	}
	require.Len(t, upstream, 2)
	require.Equal(t, 1, upstream[1].ReusedConnections)
	for _, e := range upstream {
		require.Equal(t, len("{\"reply\":\"secret\"}\n"), e.ResponseBytes)
		require.False(t, strings.Contains(e.Operation, "private-id"))
		require.Greater(t, e.Duration, time.Duration(0))
	}
}
