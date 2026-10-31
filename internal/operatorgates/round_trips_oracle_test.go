package operatorgates

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/netip"
	"reflect"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/operator"
	"github.com/relaishq/relais/pkg/relay"
)

// A deterministic, immediate-success product world. The real procedures and
// ProcessRuntime generate every host request. Fixed waits advance a fake clock.
// This pins transport counts, not media qualification or real readiness latency.
type roundTripWorld struct {
	runtime        *ProcessRuntime
	clock          *FakeClock
	call           controlplane.CallStatus
	dead, draining map[string]bool
	history        []controlplane.MoveResult
	extra          ProcessExtra
	relayEpoch     uint64
	noCalls        bool
	counts         map[string]int
}

func (w *roundTripWorld) status() controlplane.Status {
	s := controlplane.Status{Ready: true, Pool: controlplane.PoolSample{Cap: 2}, Calls: []controlplane.CallStatus{}, Workers: []controlplane.WorkerStatus{}, Takeovers: w.history}
	if !w.noCalls {
		s.Calls = append(s.Calls, w.call)
	}
	for _, name := range []string{"w1", "w2"} {
		n := 0
		if !w.noCalls && w.call.Owner == name {
			n = 1
		}
		s.Workers = append(s.Workers, controlplane.WorkerStatus{Name: name, Address: w.runtime.Config.Parameters.Workers[name].Media, Dead: w.dead[name], Draining: w.draining[name], Calls: n})
	}
	return s
}
func (w *roundTripWorld) move(to, kind string) controlplane.MoveResult {
	r := controlplane.MoveResult{ID: w.call.ID, From: w.call.Owner, To: to, Kind: kind, Start: w.clock.Wall(), End: w.clock.Wall()}
	w.call.Owner = to
	w.call.Epoch++
	if kind == "takeover" {
		w.history = append(w.history, r)
	}
	return r
}
func (w *roundTripWorld) data(op string) any {
	e := w.extra
	switch op {
	case "status":
		return w.status()
	case "drain":
		w.draining[e.Worker] = true
		moves := []controlplane.MoveResult{}
		if w.call.Owner == e.Worker {
			moves = append(moves, w.move(other(e.Worker), "move"))
		}
		return controlplane.DrainResult{Moves: moves}
	case "move":
		return w.move(e.Worker, "move")
	case "worker-down":
		w.dead[e.Worker] = true
		if w.call.Owner == e.Worker {
			w.move(other(e.Worker), "takeover")
		}
		return map[string]any{"completed": true, "signalBeforeUnixNs": "1792886400000000000", "signalAfterUnixNs": "1792886400000000000"}
	case "worker-stop":
		w.dead[e.Worker] = true
	case "worker-start":
		w.dead[e.Worker] = false
		w.draining[e.Worker] = false
	case "control-start":
		return map[string]any{"completed": true, "readyMs": 1}
	case "relay-stop":
		w.relayEpoch++
	case "relay-inspect":
		return map[string]any{"active": "relais-relay.service", "pids": map[string]int{"relais-relay.service": 1, "relais-relay-standby.service": 0}, "status": relay.RelayStatus{Instance: strconv.FormatUint(w.relayEpoch, 10), Public: netip.MustParseAddrPort("192.0.2.1:24780"), Private: netip.MustParseAddrPort("192.0.2.1:24781"), Continuity: &relay.ContinuityStatus{Epoch: w.relayEpoch}, Stats: relay.Stats{Workers: 2, WorkerPackets: 100, RoutesRestored: 1}}}
	case "worker-status":
		installed := []mediaworker.InstalledSession{}
		if !w.noCalls && w.call.Owner == e.Worker {
			installed = append(installed, mediaworker.InstalledSession{ID: w.call.ID, Epoch: w.call.Epoch, Owner: w.runtime.Config.Parameters.Workers[e.Worker].Media})
		}
		return mediaworker.WorkerStatus{InventoryKnown: true, Sessions: len(installed), Inventory: installed}
	case "telemetry":
		return HostTelemetry{}
	}
	return map[string]any{"completed": true}
}
func newRoundTripWorld(t *testing.T, batch bool) (*roundTripWorld, ProcedurePorts) {
	t.Helper()
	r, peer, _ := runtimeFixture(t)
	clock := r.Session.clock.(*FakeClock)
	// Match the observed remaining call-age wait. Earlier preparation is outside
	// this procedure count; all later waits remain the production constants.
	registered := clock.Wall().Add(-100 * time.Second).UnixMilli()
	r.Config.ArmedAt = clock.Wall()
	r.Config.Trials = true
	r.Config.Mixed = true
	r.BeforeMutation = nil
	if batch {
		r.Session.capabilities[operator.HostReadBatch] = true
	}
	w := &roundTripWorld{runtime: r, clock: clock, call: controlplane.CallStatus{ID: "original", Owner: "w1", Epoch: 1}, dead: map[string]bool{}, draining: map[string]bool{}, relayEpoch: 1, counts: map[string]int{}}
	original := peer.respond
	single := func(req operator.Request) operator.Reply {
		if req.Action == "inventory" || req.Action == "clock" {
			return original(req)
		}
		if req.Action == "worker-status" {
			match := regexp.MustCompile(`p=json.loads\(base64.b64decode\('([^']+)'\)\)`).FindSubmatch(req.Script)
			if len(match) != 2 {
				t.Fatal("missing fixed process parameters")
			}
			raw, err := base64.StdEncoding.DecodeString(string(match[1]))
			if err != nil {
				t.Fatal(err)
			}
			if err = json.Unmarshal(raw, &w.extra); err != nil {
				t.Fatal(err)
			}
		}
		data := w.data(req.Action)
		return hostReply(t, oracleJSON(map[string]any{"kind": "ok", "started": clock.Wall(), "ended": clock.Wall(), "data": data}))
	}
	peer.respond = func(req operator.Request) operator.Reply {
		w.counts[req.Action]++
		if req.Operation != operator.HostReadBatch {
			return single(req)
		}
		var args operator.HostReadBatchArgs
		if err := json.Unmarshal(req.Args, &args); err != nil {
			t.Fatal(err)
		}
		out := operator.HostReadBatchOutput{}
		for _, read := range args.Reads {
			child := single(operator.Request{Action: req.Action, Target: read.Target})
			child.ID = read.Target
			out.Replies = append(out.Replies, child)
		}
		zero := 0
		return operator.Reply{Transport: operator.Succeeded, ExitCode: &zero, Payload: oracleJSON(out)}
	}
	broker := &testGateBroker{reg: Registration{Near: true, Sessions: []string{"original"}, RegisteredAt: &registered}}
	p := ProcedurePorts{Clock: clock, Broker: broker, Sleep: func(_ context.Context, d time.Duration) error { clock.Advance(d); return nil }, Step: func(ctx context.Context, op, slot string, end time.Duration, extra ProcessExtra) (StepResult, error) {
		w.extra = extra
		return r.Step(ctx, op, slot, end, extra)
	}}
	// The fixture inventories describe a single release. Target identity and
	// require-running checks have their own oracle; count the same four reads.
	p.Inventory = func(ctx context.Context, _ string, end time.Duration) error {
		_, err := r.Session.FreshInventory(ctx, peer, r.Pins, end, "", true)
		return err
	}
	p.Media = func(context.Context) (*MediaSummary, error) {
		w.noCalls = true
		return &MediaSummary{Status: operator.Passed}, nil
	}
	return w, p
}

var procedureHostCounts = map[operator.Gate]map[string]int{
	operator.Rollout:         {"clock": 44, "drain": 2, "freeze": 1, "inventory": 96, "move": 5, "replace": 5, "status": 31, "unfreeze": 1, "worker-down": 3, "worker-empty": 2, "worker-start": 5, "worker-stop": 2},
	operator.ControlRestart:  {"clock": 48, "control-start": 2, "control-stop": 2, "drain": 1, "inventory": 116, "move": 6, "replace": 6, "status": 38, "worker-down": 5, "worker-empty": 1, "worker-start": 6, "worker-stop": 1},
	operator.UpgradeRollback: {"clock": 40, "control-start": 2, "control-stop": 2, "drain": 4, "freeze": 2, "inventory": 188, "move": 3, "relay-inspect": 4, "relay-replenish": 2, "relay-stage": 2, "relay-stop": 2, "replace": 7, "signal-start": 2, "signal-stop": 2, "status": 39, "unfreeze": 1, "worker-down": 3, "worker-empty": 4, "worker-start": 7, "worker-stop": 4},
	operator.RelayDeploy:     {"clock": 4, "inventory": 12, "relay-inspect": 2, "relay-replenish": 1, "relay-stage": 1, "relay-stop": 1, "status": 1},
	operator.Failover:        {"clock": 12, "inventory": 20, "move": 1, "replace": 1, "status": 9, "telemetry": 12, "worker-down": 1, "worker-start": 1, "worker-status": 6},
	operator.Load:            {"status": 1, "telemetry": 4, "worker-status": 2},
}

func checkRoundTripCounts(t *testing.T, gate operator.Gate, batch bool, got map[string]int) {
	t.Helper()
	want := map[string]int{}
	for action, n := range procedureHostCounts[gate] {
		if batch && (action == "inventory" || action == "clock") {
			n /= 4
		}
		want[action] = n
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("MUTATION: procedure-round-trip-counts got %v want %v", got, want)
	}
}
func TestProcedureRoundTripCounts(t *testing.T) {
	for _, gate := range []operator.Gate{operator.Rollout, operator.ControlRestart, operator.UpgradeRollback, operator.RelayDeploy} {
		for _, batch := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/batch=%t", gate, batch), func(t *testing.T) {
				w, p := newRoundTripWorld(t, batch)
				var out ProcedureResult
				if gate == operator.UpgradeRollback {
					out = RunReleaseGate(context.Background(), w.runtime.Config, p, GateMaximum)
				} else {
					out = RunProcessGate(context.Background(), gate, w.runtime.Config, p, GateMaximum)
				}
				if want := map[operator.Gate]time.Duration{operator.Rollout: 690 * time.Second, operator.ControlRestart: 750 * time.Second, operator.UpgradeRollback: 720 * time.Second, operator.RelayDeploy: 90 * time.Second}[gate]; w.clock.Elapsed() != want {
					t.Fatalf("fixed waits changed: got %s want %s", w.clock.Elapsed(), want)
				}
				if !out.Completed {
					t.Fatalf("procedure incomplete: %+v counts=%v", out, w.counts)
				}
				checkRoundTripCounts(t, gate, batch, w.counts)
				total := 0
				for _, n := range w.counts {
					total += n
				}
				t.Logf("hosts=%d counts=%v fixed=%s events=%d", total, w.counts, w.clock.Elapsed(), len(p.Broker.Events()))
			})
		}
	}
}

type roundTripStore struct{ changed bool }

func (s *roundTripStore) Observe(context.Context, time.Duration) (StoreObservation, error) {
	primary := "before"
	if s.changed {
		primary = "after"
	}
	return StoreObservation{Primary: primary, Available: true, Durability: "sync"}, nil
}
func (s *roundTripStore) Failover(context.Context, time.Duration) error { s.changed = true; return nil }
func TestMeasurementRoundTripCounts(t *testing.T) {
	for _, gate := range []operator.Gate{operator.Failover, operator.Load} {
		for _, batch := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/batch=%t", gate, batch), func(t *testing.T) {
				w, p := newRoundTripWorld(t, batch)
				ports := MeasurePorts{ProcedurePorts: p, Calls: 1, Store: &roundTripStore{}, MarkFault: func(context.Context) error { return nil }}
				ports.Observe = func(ctx context.Context, end time.Duration) (MeasurementObservation, error) {
					// Observe uses the same Step ingress. Supply the worker selector normally
					// passed by the runtime, so both independent inventories remain present.
					return w.runtime.Observe(ctx, end)
				}
				ports.BeforeFault = func(ctx context.Context, end time.Duration) error {
					if err := p.Inventory(ctx, "from", end); err != nil {
						return err
					}
					return w.runtime.VerifyClocks(ctx, end)
				}
				// Complete the collector before its first poll. The variable-duration
				// observation loop is separately expressed as seven requests per sample.
				ports.Collect = func(context.Context, time.Duration) <-chan MeasurementCollection {
					ch := make(chan MeasurementCollection, 1)
					ch <- MeasurementCollection{}
					close(ch)
					return ch
				}
				out := RunMeasureGate(context.Background(), gate, w.runtime.Config, ports, GateMaximum)
				if out.Reason != "" || out.Measurement.MissingSamples > out.Measurement.Samples {
					t.Fatalf("measurement incomplete: %+v", out)
				}
				checkRoundTripCounts(t, gate, batch, w.counts)
				total := 0
				for _, n := range w.counts {
					total += n
				}
				t.Logf("hosts=%d counts=%v fixed=%s events=%d samples=%d", total, w.counts, w.clock.Elapsed(), len(p.Broker.Events()), out.Measurement.Samples)
			})
		}
	}
}

// The complete release fixture has the same 92 production step calls as the
// immediate-readiness world. Expand only the outer seam to avoid double-counting
// the oracle's nested trackDownWorkers and WorkerTrials instrumentation.
func TestReleaseRequestPlanMatchesFrozenOracle(t *testing.T) {
	corpus, err := loadProcedureCorpus(t)
	if err != nil {
		t.Fatal(err)
	}
	fixture, err := loadProcedureFixture(corpus, "124967a5e204fb0b.json")
	if err != nil {
		t.Fatal(err)
	}
	roots := procedureRoots(fixture, "release.runReleaseGate")
	if len(roots) != 1 {
		t.Fatal("missing complete release oracle")
	}
	got := map[string]int{}
	for _, event := range roots[0].events {
		if event.Kind == "request" && event.Operation == "release.runReleaseGate.arg2.step" {
			var action string
			if len(event.Inputs) == 0 || json.Unmarshal(event.Inputs[0], &action) != nil {
				t.Fatal("invalid oracle step")
			}
			got[action]++
		}
	}
	want := map[string]int{}
	for action, n := range procedureHostCounts[operator.UpgradeRollback] {
		if action != "inventory" && action != "clock" {
			want[action] = n
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("MUTATION: release-request-plan got %v want %v", got, want)
	}
}
