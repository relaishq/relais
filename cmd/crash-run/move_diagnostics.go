package main

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/relais/internal/movediag"
	"github.com/relais/internal/nettopology"
	"github.com/relais/internal/privateapi"
)

type movePhaseCalls struct {
	ProxyHTTPRequests        int `json:"proxy_http_requests"`
	HTTPRequests             int `json:"http_requests"`
	RedisCommands            int `json:"redis_commands"`
	HTTPRequestPayloadBytes  int `json:"http_request_payload_bytes"`
	HTTPResponsePayloadBytes int `json:"http_response_payload_bytes"`
}
type namespaceTCPDelta struct {
	Role        string            `json:"role"`
	Counters    map[string]uint64 `json:"counters"`
	Unavailable []string          `json:"unavailable,omitempty"`
}
type moveDiagnostics struct {
	TraceID  string                    `json:"trace_id,omitempty"`
	Complete bool                      `json:"complete"`
	Scope    string                    `json:"scope"`
	Before   []nettopology.TCPSnapshot `json:"tcp_before"`
	After    []nettopology.TCPSnapshot `json:"tcp_after"`
	Deltas   []namespaceTCPDelta       `json:"tcp_deltas"`
	Events   []movediag.Event          `json:"events"`
	Phases   map[string]movePhaseCalls `json:"phase_calls"`
	Errors   []string                  `json:"errors,omitempty"`
}

func tcpDeltas(before, after []nettopology.TCPSnapshot) []namespaceTCPDelta {
	prior := map[string]nettopology.TCPSnapshot{}
	for _, s := range before {
		prior[s.Role] = s
	}
	var out []namespaceTCPDelta
	for _, s := range after {
		b, ok := prior[s.Role]
		d := namespaceTCPDelta{Role: s.Role, Counters: map[string]uint64{}}
		for _, name := range nettopology.TCPCounterNames {
			bv, bok := b.Counters[name]
			av, aok := s.Counters[name]
			if !ok || b.Namespace != s.Namespace || b.Error != "" || s.Error != "" || !bok || !aok || av < bv {
				d.Unavailable = append(d.Unavailable, name)
			} else {
				d.Counters[name] = av - bv
			}
		}
		out = append(out, d)
	}
	return out
}

func collectMoveDiagnostics(ctx context.Context, endpoints []string, before, after []nettopology.TCPSnapshot) *moveDiagnostics {
	d := &moveDiagnostics{Before: before, After: after, Deltas: tcpDeltas(before, after), Scope: "client HTTP calls (full response body), plus separately counted driver proxy upstream calls; client Redis commands (Lua internals excluded); TCP counters include all namespace traffic; HTTP bytes are JSON payload only"}
	for i, endpoint := range endpoints {
		var snapshot movediag.Snapshot
		if err := privateapi.Do(ctx, &http.Client{Timeout: privateapi.Timeout}, "http://"+endpoint, http.MethodGet, movediag.Endpoint, nil, &snapshot, nil); err != nil {
			d.Errors = append(d.Errors, fmt.Sprintf("private endpoint %d diagnostics unavailable: %v", i, err))
			continue
		}
		if snapshot.Dropped > 0 {
			d.Errors = append(d.Errors, fmt.Sprintf("private endpoint %d dropped %d trace events", i, snapshot.Dropped))
		}
		for _, e := range snapshot.Events {
			// Listener order is relay, control, worker-0, worker-1.
			names := []string{"relay", "control", "worker-0", "worker-1"}
			if i < len(names) {
				e.Process = names[i]
			}
			d.Events = append(d.Events, e)
		}
	}
	for _, e := range movediag.Default.Snapshot().Events {
		e.Process = "driver-proxy"
		d.Events = append(d.Events, e)
	}
	if dropped := movediag.Default.Snapshot().Dropped; dropped > 0 {
		d.Errors = append(d.Errors, fmt.Sprintf("driver proxy dropped %d trace events", dropped))
	}
	for _, snapshots := range [][]nettopology.TCPSnapshot{before, after} {
		for _, s := range snapshots {
			if s.Error != "" {
				d.Errors = append(d.Errors, s.Role+": "+s.Error)
			}
			if len(s.Missing) > 0 {
				d.Errors = append(d.Errors, s.Role+": missing "+strings.Join(s.Missing, ","))
			}
		}
	}
	return d
}

// Select by the move's returned trace ID, so deliberate pre-baseline fault
// probes cannot contaminate the measured event. Unreturned/error outcomes keep
// the window's events and are explicitly incomplete.
func (d *moveDiagnostics) selectMove(id string, normal bool) {
	if id == "" && len(d.Before) > 0 && len(d.After) > 0 {
		lower, upper := d.Before[len(d.Before)-1].End, d.After[0].Start
		var roots []movediag.Event
		for _, e := range d.Events {
			if e.Kind == "move" && !e.Start.Before(lower) && !e.End.After(upper) {
				roots = append(roots, e)
			}
		}
		if len(roots) == 1 {
			id = roots[0].TraceID
		}
		if id == "" {
			events := d.Events[:0]
			for _, e := range d.Events {
				if !e.Start.Before(lower) && !e.End.After(upper) {
					events = append(events, e)
				}
			}
			d.Events = events
		}
	}
	d.TraceID = id
	for _, delta := range d.Deltas {
		if len(delta.Unavailable) > 0 {
			d.Errors = append(d.Errors, delta.Role+": counter delta unavailable")
		}
	}
	d.Complete = id != "" && normal && len(d.Errors) == 0
	if id == "" {
		d.Errors = append(d.Errors, "measured move trace ID unavailable")
		return
	}
	events := d.Events[:0]
	for _, e := range d.Events {
		if e.TraceID == id {
			events = append(events, e)
		}
	}
	d.Events = events
	sort.Slice(d.Events, func(i, j int) bool { return d.Events[i].Start.Before(d.Events[j].Start) })
	d.Phases = map[string]movePhaseCalls{}
	for _, phase := range []string{"prepare", "barrier", "export", "transfer", "resume", "release"} {
		d.Phases[phase] = movePhaseCalls{}
	}
	root := false
	for _, e := range events {
		if e.Kind == "move" {
			root = true
			continue
		}
		counts := d.Phases[e.Phase]
		switch e.Kind {
		case "http":
			counts.HTTPRequests++
			counts.HTTPRequestPayloadBytes += e.RequestBytes
			counts.HTTPResponsePayloadBytes += e.ResponseBytes
		case "http_proxy":
			counts.ProxyHTTPRequests++
		case "redis":
			counts.RedisCommands++
		}
		d.Phases[e.Phase] = counts
	}
	if !root {
		d.Complete = false
		d.Errors = append(d.Errors, "control move completion event unavailable")
	}
	if !normal {
		d.Errors = append(d.Errors, "failed/rolled-back move: detached recovery calls may be outside the trace")
	}
}
func (d *moveDiagnostics) summary() string {
	if d == nil {
		return " move_diagnostics=unavailable"
	}
	var httpCalls, proxyCalls, redisCommands int
	for _, p := range d.Phases {
		httpCalls += p.HTTPRequests
		proxyCalls += p.ProxyHTTPRequests
		redisCommands += p.RedisCommands
	}
	var slowest movediag.Event
	for _, e := range d.Events {
		if e.Kind != "move" && e.Duration > slowest.Duration {
			slowest = e
		}
	}
	counterTotal := func(name string) string {
		if len(d.Deltas) == 0 {
			return "unavailable"
		}
		var total uint64
		for _, delta := range d.Deltas {
			value, ok := delta.Counters[name]
			if !ok {
				return "unavailable"
			}
			total += value
		}
		return fmt.Sprint(total)
	}
	return fmt.Sprintf(" move_diagnostics_complete=%t move_http_calls=%d move_proxy_http_calls=%d move_redis_commands=%d longest_call=%s/%s/%s longest_call_duration=%s namespace_retrans_segs=%s namespace_loss_probes=%s namespace_timeouts=%s", d.Complete, httpCalls, proxyCalls, redisCommands, slowest.Process, slowest.Phase, slowest.Operation, slowest.Duration, counterTotal("TcpRetransSegs"), counterTotal("TCPLossProbes"), counterTotal("TCPTimeouts"))
}
