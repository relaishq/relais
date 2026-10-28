package operatorgates

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/operatorgates/host"
	"github.com/relaishq/relais/internal/operatorgates/observe"

	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/operator"
)

// These cases pin explicit safety differences that the frozen TS corpus did not
// exercise. They never count as TypeScript replay or silently waive a mismatch.
func TestNativeDeltaFixtures(t *testing.T) {
	raw, err := os.ReadFile("testdata/deltas/safety.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture OracleFixture
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.SchemaVersion != 1 || len(fixture.Events) != 6 {
		t.Fatal("native safety fixture changed")
	}
	for i := 0; i < len(fixture.Events); i += 2 {
		request, result := fixture.Events[i], fixture.Events[i+1]
		if request.ID != result.ID || request.Operation != result.Operation {
			t.Fatal("native delta invocation mismatch")
		}
		switch request.Operation {
		case "processes.freshTakeovers":
			var move controlplane.MoveResult
			_ = json.Unmarshal(request.Inputs[0], &move)
			before := StepResult{History: []json.RawMessage{request.Inputs[0]}}
			after := StepResult{History: []json.RawMessage{request.Inputs[0]}, Status: controlplane.Status{Takeovers: []controlplane.MoveResult{move}}}
			var want int
			_ = json.Unmarshal(result.Value, &want)
			if len(freshTakeovers(before, after, move.ID, move.From)) != want {
				t.Fatal("MUTATION: delta-fresh-history")
			}
		case "processes.relay":
			var unknownAt int
			_ = json.Unmarshal(request.Inputs[0], &unknownAt)
			var want struct {
				Reason       string
				Reads, Calls int
				Elapsed      time.Duration
			}
			_ = json.Unmarshal(result.Value, &want)
			g, clock := testProcedure(t)
			calls, reads := 0, 0
			g.p.Step = func(_ context.Context, op, _ string, _ time.Duration, _ ProcessExtra) (StepResult, error) {
				calls++
				if op == "relay-inspect" {
					reads++
					if reads == unknownAt {
						return StepResult{}, fault("command-result-unknown")
					}
				}
				return StepResult{}, nil
			}
			actual := g.relay(context.Background(), false, 1)
			if code(actual) != want.Reason || reads != want.Reads || calls != want.Calls || clock.Elapsed() != want.Elapsed {
				t.Fatal("MUTATION: delta-relay-unknown", actual, calls, reads)
			}
		case "measure.store":
			var unknownAt int
			_ = json.Unmarshal(request.Inputs[0], &unknownAt)
			var want struct {
				Reason           string
				Reads, Failovers int
			}
			_ = json.Unmarshal(result.Value, &want)
			g, _ := testProcedure(t)
			store := &uncertainStoreRead{unknownAt: unknownAt}
			ports := MeasurePorts{ProcedurePorts: g.p, Calls: 1, Store: store, BeforeFault: func(context.Context, time.Duration) error { return nil }, MarkFault: func(context.Context) error { return nil }, Observe: func(context.Context, time.Duration) (MeasurementObservation, error) {
				status := controlplane.Status{Ready: true, Pool: controlplane.PoolSample{Cap: 2}, Calls: []controlplane.CallStatus{{ID: "original", Owner: "w1", Epoch: 1}}, Workers: []controlplane.WorkerStatus{{Name: "w1", Address: g.c.Parameters.Workers["w1"].Media}, {Name: "w2", Address: g.c.Parameters.Workers["w2"].Media}}}
				return MeasurementObservation{Status: &status}, nil
			}}
			actual := RunMeasureGate(context.Background(), operator.Failover, g.c, ports, g.end)
			if actual.Reason != want.Reason || store.reads != want.Reads || store.failovers != want.Failovers {
				t.Fatal("MUTATION: delta-store-unknown", actual, store)
			}
		default:
			t.Fatal("unreplayed native delta", request.Operation)
		}
	}
}

func TestRepresentationFixtures(t *testing.T) {
	raw, err := os.ReadFile("testdata/deltas/representations.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture OracleFixture
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Events) != 10 {
		t.Fatal("representation invocations changed")
	}
	for i := 0; i < len(fixture.Events); i += 2 {
		req, res := fixture.Events[i], fixture.Events[i+1]
		if req.ID != res.ID || req.Operation != res.Operation {
			t.Fatal("representation identity")
		}
		switch req.Operation {
		case "host.architecture":
			accepted := 0
			for _, input := range req.Inputs {
				var arch string
				_ = json.Unmarshal(input, &arch)
				script, err := host.Process("freeze", struct{}{}, &host.Stage{Version: "0.3.0", Arch: arch, ArchiveSHA256: strings.Repeat("a", 64), ManifestSHA256: strings.Repeat("b", 64), Pool: "e30=", Operation: "upgrade"})
				downloadArch := map[string]string{"arm64": "arm64", "amd64": "x86_64", "x86_64": "x86_64"}[arch]
				if err != nil || !strings.Contains(script, "'"+downloadArch+"'") {
					t.Fatal("MUTATION: representation-architecture", arch, err)
				}
				accepted++
			}
			var want int
			_ = json.Unmarshal(res.Value, &want)
			if accepted != want {
				t.Fatal("representation architecture count")
			}
		case "observe.historicalMove":
			move, err := observe.HistoricalMove(req.Inputs[0])
			if err != nil {
				t.Fatal("MUTATION: representation-historical-start", err)
			}
			var want time.Time
			_ = json.Unmarshal(res.Value, &want)
			if !move.Start.IsZero() || !move.End.Equal(want) {
				t.Fatal("MUTATION: representation-historical-start", move)
			}
		case "processes.endpoints":
			var parameters ProcessParameters
			if err := json.Unmarshal(req.Inputs[0], &parameters); err != nil {
				t.Fatal(err)
			}
			var want bool
			_ = json.Unmarshal(res.Value, &want)
			if (ValidateProcessParameters(parameters) == nil) != want {
				t.Fatal("MUTATION: representation-endpoints")
			}
		case "processes.completion":
			if _, err := DecodeStepResult("worker-start", req.Inputs[0]); err == nil {
				t.Fatal("MUTATION: representation-completion")
			}
			if _, err := DecodeStepResult("worker-start", req.Inputs[1]); err != nil {
				t.Fatal(err)
			}
			var want string
			_ = json.Unmarshal(res.Value, &want)
			if code(knownProductFailure("status", req.Inputs[2])) != want {
				t.Fatal("MUTATION: representation-known-loss")
			}
		case "processes.signalNanoseconds":
			runtime, peer, _ := runtimeFixture(t)
			respond := peer.respond
			peer.respond = func(request operator.Request) operator.Reply {
				if request.Action == "worker-down" {
					return hostReply(t, req.Inputs[0])
				}
				return respond(request)
			}
			actual, err := runtime.Step(context.Background(), "worker-down", "w1", time.Minute, ProcessExtra{Worker: "w1"})
			var want time.Time
			_ = json.Unmarshal(res.Value, &want)
			if err != nil || !actual.Started.Equal(want) {
				t.Fatal("MUTATION: representation-signal-ns", actual.Started, want, err)
			}
		default:
			t.Fatal("unreplayed representation", req.Operation)
		}
	}
}
