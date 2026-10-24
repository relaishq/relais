package privateapi_test

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/privateapi"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/relay"
	"github.com/relaishq/relais/pkg/sessionstore"
)

// These are success and error payload fixtures for every private endpoint. They
// serialize production types, including optional fields, rather than copies of
// structs. The route inventory below fails on an unrepresented new endpoint.
type endpoint struct {
	Service, Route    string
	Request, Response any
	Statuses          []int
}
type payload struct {
	JSON  any `json:"json"`
	Shape any `json:"shape"`
}
type fixture struct {
	Service  string  `json:"service"`
	Route    string  `json:"route"`
	Request  payload `json:"request"`
	Response payload `json:"response"`
	Statuses []int   `json:"statuses"`
}

func endpoints() []endpoint {
	return []endpoint{
		{"worker", "POST /sessions", mediaworker.CreateRequest{}, mediaworker.CreateReply{}, []int{200, 400, 409}},
		{"worker", "DELETE /sessions/{id}", nil, nil, []int{204, 409}},
		{"worker", "POST /sessions/{id}/export", nil, mediaworker.ExportReply{}, []int{200, 409}},
		{"worker", "POST /sessions/{id}/agent/control", agent.ControlMessage{}, agent.ControlReceipt{}, []int{200, 202, 400, 409}},
		{"worker", "POST /sessions/{id}/keyframe", nil, nil, []int{204, 409}},
		{"worker", "POST /resume", mediaworker.ResumeRequest{}, mediaworker.ResumeReply{}, []int{200, 400, 409}},
		{"worker", "GET /status", nil, mediaworker.WorkerStatus{}, []int{200, 409}},
		{"worker", "GET /metrics", nil, "Prometheus text exposition", []int{200}},
		{"relay", "POST /sessions/{id}/begin-replay", relay.ReplayRequest{}, relay.ReplayPlan{}, []int{200, 400, 409}},
		{"relay", "POST /sessions/{id}/replay", relay.ReplayRequest{}, relay.ReplayResult{}, []int{200, 400, 409}},
		{"relay", "POST /workers", relay.WorkerRequest{}, nil, []int{204, 400}},
		{"relay", "DELETE /workers", relay.WorkerRequest{}, nil, []int{204, 400}},
		{"relay", "POST /sessions/{id}/hold", relay.RouteRequest{}, nil, []int{204, 400, 409}},
		{"relay", "POST /sessions/{id}/move", relay.RouteRequest{}, nil, []int{204, 400, 409}},
		{"relay", "POST /sessions/{id}/release", relay.RouteRequest{}, relay.ReleaseReply{}, []int{200, 400, 409}},
		{"relay", "POST /sessions/{id}/move-after-export", relay.RouteRequest{}, nil, []int{204, 400, 409}},
		{"relay", "POST /sessions/{id}/release-after-export", relay.ExportReleaseRequest{}, relay.ReleaseReply{}, []int{200, 400, 409}},
		{"relay", "DELETE /sessions/{id}", nil, nil, []int{204}},
		{"relay", "POST /sessions/{id}/forget-tenure", sessionstore.Lease{}, nil, []int{204, 400, 409}},
		{"relay", "GET /status", nil, relay.RelayStatus{}, []int{200}},
		{"relay", "GET /metrics", nil, "Prometheus text exposition", []int{200}},
		{"control", "POST /calls", "SDP offer", "SDP answer", []int{201, 400, 409, 415, 503}},
		{"control", "DELETE /calls/{id}", nil, nil, []int{200, 404, 409, 503}},
		{"control", "POST /calls/{id}/agent/control", agent.ControlMessage{}, agent.ControlReceipt{}, []int{200, 202, 400, 409}},
		{"control", "POST /calls/{id}/move", nil, controlplane.MoveResult{}, []int{200, 404, 409, 503}},
		{"control", "POST /workers/{name}/drain", nil, controlplane.DrainResult{}, []int{200, 207, 500, 503}},
		{"control", "GET /status", nil, controlplane.Status{}, []int{200, 503}},
		{"control", "GET /metrics", nil, "Prometheus text exposition", []int{200}},
		{"control", "POST /private/workers/{name}/mark-draining", nil, nil, []int{204, 404, 503}},
		{"control", "POST /private/workers/{name}/replace", nil, struct{}{}, []int{200, 404, 409}},
		{"control", "POST /private/workers/{name}/heartbeat", controlplane.HeartbeatRequest{}, controlplane.HeartbeatReply{}, []int{200, 400, 409}},
		// Shared error envelopes and alternate status/error bodies are wire contracts too.
		{"errors", "conflict", nil, privateapi.Failure{}, []int{409}},
		{"errors", "move-result", nil, controlplane.MoveFailure{}, []int{409}},
		{"worker-query", "GET /status?agent-session=id", nil, mediaworker.AgentSessionStatus{}, []int{200, 409}},
	}
}

func wireShape(t reflect.Type, depth int) any {
	if t == nil {
		return nil
	}
	if depth > 20 {
		panic("recursive HTTP wire type")
	}
	if t == reflect.TypeFor[time.Time]() {
		return "RFC3339 time"
	}
	if t == reflect.TypeFor[netip.AddrPort]() {
		return "IP:port"
	}
	switch t.Kind() {
	case reflect.Pointer:
		return map[string]any{"optional": wireShape(t.Elem(), depth+1)}
	case reflect.Struct:
		fields := map[string]any{}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			tag := f.Tag.Get("json")
			if tag == "-" {
				continue
			}
			name := strings.Split(tag, ",")[0]
			if name == "" {
				name = f.Name
			}
			fields[name] = map[string]any{"type": wireShape(f.Type, depth+1), "json_options": strings.TrimPrefix(tag, strings.Split(tag, ",")[0])}
		}
		return fields
	case reflect.Slice, reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 {
			return "base64 bytes"
		}
		return []any{wireShape(t.Elem(), depth+1)}
	case reflect.Map:
		return map[string]any{"map_key": t.Key().Kind().String(), "value": wireShape(t.Elem(), depth+1)}
	default:
		return t.Kind().String()
	}
}
func wireSample(t reflect.Type, depth int) reflect.Value {
	if depth > 20 {
		panic("recursive HTTP wire type")
	}
	if t == reflect.TypeFor[json.RawMessage]() {
		return reflect.ValueOf(json.RawMessage(`{"fixture":true}`))
	}
	if t == reflect.TypeFor[time.Time]() {
		return reflect.ValueOf(time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC))
	}
	if t == reflect.TypeFor[netip.AddrPort]() {
		return reflect.ValueOf(netip.MustParseAddrPort("192.0.2.1:1234"))
	}
	v := reflect.New(t).Elem()
	switch t.Kind() {
	case reflect.Pointer:
		v.Set(reflect.New(t.Elem()))
		v.Elem().Set(wireSample(t.Elem(), depth+1))
	case reflect.Struct:
		for i := 0; i < t.NumField(); i++ {
			if v.Field(i).CanSet() {
				v.Field(i).Set(wireSample(t.Field(i).Type, depth+1))
			}
		}
	case reflect.Slice:
		v.Set(reflect.MakeSlice(t, 1, 1))
		v.Index(0).Set(wireSample(t.Elem(), depth+1))
	case reflect.Array:
		for i := 0; i < t.Len(); i++ {
			v.Index(i).Set(wireSample(t.Elem(), depth+1))
		}
	case reflect.Map:
		v.Set(reflect.MakeMap(t))
		v.SetMapIndex(wireSample(t.Key(), depth+1), wireSample(t.Elem(), depth+1))
	case reflect.String:
		v.SetString("fixture")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(1)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(1)
	}
	return v
}
func encodePayload(t *testing.T, value any) payload {
	t.Helper()
	if value == nil {
		return payload{}
	}
	ty := reflect.TypeOf(value)
	data, err := json.Marshal(wireSample(ty, 0).Interface())
	if err != nil {
		t.Fatal(err)
	}
	var sample any
	if err = json.Unmarshal(data, &sample); err != nil {
		t.Fatal(err)
	}
	return payload{JSON: sample, Shape: wireShape(ty, 0)}
}

func TestPrivateHTTPContractGolden(t *testing.T) {
	var fixtures []fixture
	wire := actualHTTPWire(t)
	if err := verifyDecodedRequests(endpoints(), wire.Handlers); err != nil {
		t.Fatal(err)
	}
	for _, e := range endpoints() {
		fixtures = append(fixtures, fixture{Service: e.Service, Route: e.Route, Request: encodePayload(t, e.Request), Response: encodePayload(t, e.Response), Statuses: e.Statuses})
	}
	data, err := json.MarshalIndent(struct {
		Payloads []fixture `json:"payloads"`
		Wire     httpWire  `json:"wire"`
	}{fixtures, wire}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	path := filepath.Join("testdata", fmt.Sprintf("contract-v%d.json", privateapi.ContractVersion))
	if os.Getenv("RELAIS_UPDATE_HTTP_GOLDEN") == "1" {
		if err = os.WriteFile(path, data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	expected, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(expected) {
		t.Fatal("private HTTP request/response golden drift; bump ContractVersion and generate the new versioned fixture with RELAIS_UPDATE_HTTP_GOLDEN=1 go test ./internal/privateapi -run TestPrivateHTTPContractGolden")
	}
}

func endpointCoverage(fixtures, registered []string) error {
	for _, route := range registered {
		if !slices.Contains(fixtures, route) {
			return fmt.Errorf("private endpoint lacks golden request/response fixtures: %s", route)
		}
	}
	for _, route := range fixtures {
		if !slices.Contains(registered, route) {
			return fmt.Errorf("golden endpoint is no longer registered: %s; revise the versioned contract", route)
		}
	}
	return nil
}

func TestPrivateHTTPEndpointCoverageRejectsAddedAndRemovedRoutes(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		fixtures, registered []string
		allowed              bool
	}{
		{"unchanged", []string{"worker GET /status"}, []string{"worker GET /status"}, true},
		{"removed", []string{"worker GET /status"}, nil, false},
		{"added", nil, []string{"worker GET /status"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := endpointCoverage(tc.fixtures, tc.registered)
			if (err == nil) != tc.allowed {
				t.Fatalf("allowed=%v error=%v", tc.allowed, err)
			}
		})
	}
}
