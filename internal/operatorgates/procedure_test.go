package operatorgates

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/relaishq/relais/internal/operatorgates/inventory"
	"github.com/relaishq/relais/internal/operatorgates/oracletest"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/operator"
)

type testGateBroker struct {
	reg    Registration
	events []Receipt
}

func (b *testGateBroker) RunID() string                                 { return "test-session" }
func (b *testGateBroker) Registration() *Registration                   { return &b.reg }
func (b *testGateBroker) Events() []Receipt                             { return b.events }
func (b *testGateBroker) Arm(context.Context, string, []Identity) error { return nil }
func (b *testGateBroker) Feed(_ context.Context, _ string, r []Receipt) error {
	b.events = append(b.events, r...)
	return nil
}
func testProcedure(t *testing.T) (*procedure, *FakeClock) {
	t.Helper()
	clock := &FakeClock{Now: time.UnixMilli(1_000_000)}
	registered := clock.Now.UnixMilli()
	config := &ProcessConfig{ArmedAt: clock.Now, Parameters: ProcessParameters{Control: "http://192.0.2.1:24710", Relay: "http://192.0.2.2:24700", Store: "store.test", StorePort: 16379, Workers: map[string]WorkerEndpoint{"w1": {HTTP: "http://192.0.2.3:24701", Media: netip.MustParseAddrPort("192.0.2.3:24711")}, "w2": {HTTP: "http://192.0.2.4:24701", Media: netip.MustParseAddrPort("192.0.2.4:24711")}}}}
	p := ProcedurePorts{Clock: clock, Broker: &testGateBroker{reg: Registration{Near: true, Sessions: []string{"original"}, RegisteredAt: &registered}}, Sleep: func(_ context.Context, d time.Duration) error { clock.Advance(d); return nil }, Step: func(context.Context, string, string, time.Duration, ProcessExtra) (StepResult, error) {
		t.Fatal("unexpected step")
		return StepResult{}, nil
	}}
	return newProcedure(config, p, 20*time.Minute), clock
}
func TestUnknownRestorationFence(t *testing.T) {
	g, _ := testProcedure(t)
	steps := 0
	g.p.Step = func(context.Context, string, string, time.Duration, ProcessExtra) (StepResult, error) {
		steps++
		return StepResult{}, nil
	}
	g.down = []string{"w1"}
	g.frozen = true
	g.unknown = true
	r := g.finish(context.Background(), fault("command-result-unknown"), false)
	if steps != 0 || r.Reason != "command-result-unknown" || r.Status != operator.Inconclusive {
		t.Fatal("MUTATION: unknown-restoration-fence")
	}
}
func TestTrackBeforeSubmission(t *testing.T) {
	g, _ := testProcedure(t)
	g.p.Step = func(context.Context, string, string, time.Duration, ProcessExtra) (StepResult, error) {
		return StepResult{}, fault("precondition-failed:signal-bracket")
	}
	_, err := g.do(context.Background(), "worker-down", "w1", ProcessExtra{Worker: "w1"})
	if err == nil || len(g.down) != 1 {
		t.Fatal("MUTATION: track-before-signal")
	}
}
func TestFreshTakeoverIdentity(t *testing.T) {
	raw := json.RawMessage(`{"id":"original","from":"w1","to":"w2","kind":"takeover","end":"1970-01-01T00:00:01Z"}`)
	before := StepResult{History: []json.RawMessage{raw}}
	move := controlplane.MoveResult{ID: "original", From: "w1", To: "w2", Kind: "takeover"}
	after := StepResult{Status: controlplane.Status{Takeovers: []controlplane.MoveResult{move}}, History: []json.RawMessage{raw}}
	if len(freshTakeovers(before, after, "original", "w1")) != 0 {
		t.Fatal("MUTATION: stale-takeover")
	}
	// Omission, explicit false and timestamp spelling are distinct history metadata.
	for _, candidate := range []json.RawMessage{json.RawMessage(strings.Replace(string(raw), `"end":`, `"lost":false,"end":`, 1)), json.RawMessage(strings.Replace(string(raw), "00:00:01Z", "00:00:01.001Z", 1)), json.RawMessage(strings.Replace(string(raw), `"end":`, `"start":"1970-01-01T00:00:00Z","end":`, 1))} {
		after.History = []json.RawMessage{candidate}
		if len(freshTakeovers(before, after, "original", "w1")) != 1 {
			t.Fatal("MUTATION: takeover-metadata")
		}
	}
}
func TestRuntimeReadCertainty(t *testing.T) {
	for _, op := range []string{"status", "telemetry", "worker-empty", "store-status", "worker-status", "relay-inspect"} {
		s, _ := fixtureSession(t)
		g, _ := testProcedure(t)
		peer := &observationPeer{respond: func(operator.Request) operator.Reply {
			return hostReply(t, []byte(`{"kind":"failed","code":"not-empty"}`))
		}}
		runtime := ProcessRuntime{Session: s, Peer: peer, Config: g.c}
		_, err := runtime.Step(context.Background(), op, "shared", time.Minute, ProcessExtra{})
		if op == "relay-inspect" {
			_, err = runtime.Step(context.Background(), op, "relay", time.Minute, ProcessExtra{})
		}
		pure := op != "status" && op != "telemetry"
		if pure {
			if code(err) != "precondition-failed:not-empty" || s.Latched() {
				t.Fatal("MUTATION: read-certainty")
			}
		} else {
			if code(err) != "command-result-unknown" || !s.Latched() {
				t.Fatal("MUTATION: impure-status")
			}
		}
	}
}
func TestRuntimeUnknownNeverResubmits(t *testing.T) {
	s, _ := fixtureSession(t)
	g, _ := testProcedure(t)
	peer := &observationPeer{respond: func(operator.Request) operator.Reply { return hostReply(t, []byte(`{"kind":"unknown"}`)) }}
	runtime := ProcessRuntime{Session: s, Peer: peer, Config: g.c}
	for range 2 {
		_, _ = runtime.Step(context.Background(), "status", "shared", time.Minute, ProcessExtra{})
	}
	if peer.sent != 1 || !s.Latched() {
		t.Fatal("MUTATION: runtime-unknown-once")
	}
}
func TestMinute105AndKnownFailure(t *testing.T) {
	g, c := testProcedure(t)
	c.Advance(SessionMaximum)
	steps := 0
	g.p.Step = func(context.Context, string, string, time.Duration, ProcessExtra) (StepResult, error) {
		steps++
		return StepResult{}, nil
	}
	g.p.Media = func(context.Context) (*MediaSummary, error) { return &MediaSummary{Status: operator.Failure}, nil }
	r := RunProcessGate(context.Background(), operator.Rollout, g.c, g.p, c.Elapsed()+20*time.Minute)
	if steps != 0 || r.Status != operator.Failure {
		t.Fatal("MUTATION: minute-105-failure")
	}
}
func TestSequencePoolStaysInsideOriginalBudget(t *testing.T) {
	s, c := fixtureSession(t)
	c.Advance(104 * time.Minute)
	commands := 0
	var cutoff time.Duration
	r := s.RunSequence(context.Background(), SequencePorts{Minimum: func(operator.Gate) time.Duration { return time.Minute }, Command: func(context.Context, operator.Gate, time.Duration) ProcedureResult {
		commands++
		return ProcedureResult{Status: operator.Passed}
	}, Finalize: func(_ context.Context, end time.Duration) (*MediaSummary, error) {
		cutoff = end
		c.Advance(2 * time.Minute)
		return &MediaSummary{Status: operator.Passed}, nil
	}})
	if commands != 0 || cutoff != 105*time.Minute || r.Pool != nil || r.Status == operator.Passed {
		t.Fatal("MUTATION: final-pool-cutoff")
	}
}
func TestSequenceUnknownStopsEverySuccessor(t *testing.T) {
	s, _ := fixtureSession(t)
	s.gates = []operator.Gate{operator.Rollout, operator.ControlRestart}
	commands := 0
	r := s.RunSequence(context.Background(), SequencePorts{Command: func(context.Context, operator.Gate, time.Duration) ProcedureResult {
		commands++
		return ProcedureResult{Status: operator.Inconclusive, Reason: "command-result-unknown"}
	}})
	if commands != 1 || r.Gates[1].Reason != "prior-gate-stopped" {
		t.Fatal("MUTATION: sequence-unknown-fence")
	}
}
func TestAdmissionWaitsForIndependentIdle(t *testing.T) {
	calls := []string{}
	p := AdmissionPorts{Survivor: func(context.Context, time.Duration) MeasureResult {
		calls = append(calls, "survivor")
		return MeasureResult{Status: operator.Inconclusive}
	}, StopSurvivor: func(context.Context, time.Duration) bool { calls = append(calls, "stop"); return true }, Idle: func(context.Context, time.Duration) error {
		calls = append(calls, "idle")
		return fault("command-result-unknown")
	}, Boundary: func(context.Context, time.Duration) MeasureResult {
		calls = append(calls, "boundary")
		return MeasureResult{}
	}}
	r := RunAdmission(context.Background(), p, time.Minute)
	if strings.Join(calls, ",") != "survivor,stop,idle" || r.Reason != "command-result-unknown" {
		t.Fatal("MUTATION: admission-idle-fence")
	}
}

func runtimeFixture(t *testing.T) (*ProcessRuntime, *observationPeer, *[]string) {
	t.Helper()
	fixtures := map[string]json.RawMessage{}
	var pins []inventory.Pin
	for _, c := range oracletest.Cases(t, "inventory.parseInventory") {
		if c.Result.Completion != "returned" {
			continue
		}
		slot := oracletest.JSON[string](t, c.Request.Inputs[0])
		var encoded struct {
			Value json.RawMessage `json:"value"`
		}
		_ = json.Unmarshal([]byte(oracletest.JSON[string](t, c.Request.Inputs[1])), &encoded)
		if _, ok := fixtures[slot]; !ok {
			fixtures[slot] = oracletest.ProductJSON(t, encoded.Value)
			pins = oracletest.JSON[[]inventory.Pin](t, c.Request.Inputs[2])
		}
	}
	g, clock := testProcedure(t)
	selected := operator.Scenario{SchemaVersion: 1, Digest: strings.Repeat("a", 64), Gates: []operator.Gate{operator.Rollout}}
	for i := range pins {
		pins[i].BootSHA256 = strings.Repeat("d", 64)
		identity := operator.Identity{SourceCommit: pins[i].Commit, SHA256: pins[i].ArchiveSHA256}
		if i == 0 {
			selected.ReleaseA = identity
		} else {
			selected.ReleaseB = identity
		}
	}
	s, err := NewSession("session", operator.Init{Scenario: selected, ArmedAtUnixMS: "1000000", EndUnixMS: "7300000", Capabilities: []operator.Operation{operator.HostExecute}}, clock)
	if err != nil {
		t.Fatal(err)
	}
	order := []string{}
	peer := &observationPeer{respond: func(req operator.Request) operator.Reply {
		order = append(order, req.Action+":"+req.Target)
		switch req.Action {
		case "inventory":
			return hostReply(t, fixtures[req.Target])
		case "clock":
			return hostReply(t, []byte(`{"errorBoundNs":0,"rateErrorPpm":0,"measuredUnixNs":"1792886400000000000"}`))
		}
		return hostReply(t, []byte(`{"kind":"ok","started":"1970-01-01T00:16:40Z","ended":"1970-01-01T00:16:40Z","data":{"completed":true}}`))
	}}
	runtime := &ProcessRuntime{Session: s, Peer: peer, Config: g.c, Pins: pins, Bounds: &clocks.Bounds{}, BeforeMutation: func(context.Context) error { order = append(order, "fault"); return nil }}
	return runtime, peer, &order
}
func TestRuntimeFreshProofAndFaultOrder(t *testing.T) {
	runtime, _, order := runtimeFixture(t)
	for _, op := range []string{"freeze", "unfreeze"} {
		if _, err := runtime.Step(context.Background(), op, "shared", time.Minute, ProcessExtra{}); err != nil {
			t.Fatal(err)
		}
	}
	expected := "inventory:relay,inventory:w1,inventory:w2,inventory:shared,clock:relay,clock:w1,clock:w2,clock:shared,fault,freeze:shared,inventory:relay,inventory:w1,inventory:w2,inventory:shared,fault,unfreeze:shared"
	if strings.Join(*order, ",") != expected {
		t.Fatalf("MUTATION: fresh-proof-fault-order got %v", *order)
	}
}
func TestRuntimeMissingClockStopsMutation(t *testing.T) {
	runtime, peer, _ := runtimeFixture(t)
	runtime.Bounds = nil
	_, err := runtime.Step(context.Background(), "freeze", "shared", time.Minute, ProcessExtra{})
	if code(err) != "missing-clock-assumptions" || peer.sent != 4 {
		t.Fatal("MUTATION: runtime-clock-proof")
	}
}
func TestRuntimeUnknownMutationStaysUnknown(t *testing.T) {
	runtime, peer, _ := runtimeFixture(t)
	respond := peer.respond
	peer.respond = func(req operator.Request) operator.Reply {
		if req.Action == "freeze" {
			return hostReply(t, []byte(`{"kind":"unknown"}`))
		}
		return respond(req)
	}
	_, err := runtime.Step(context.Background(), "freeze", "shared", time.Minute, ProcessExtra{})
	sent := peer.sent
	_, again := runtime.Step(context.Background(), "freeze", "shared", time.Minute, ProcessExtra{})
	if code(err) != "command-result-unknown" || code(again) != "command-result-unknown" || peer.sent != sent || !runtime.Session.Latched() {
		t.Fatal("MUTATION: unknown-host-fence")
	}
}

func TestPreconditionRepairsOnlyDefiniteFailure(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		g, _ := testProcedure(t)
		running := false
		order := []string{}
		g.p.Inventory = func(context.Context, string, time.Duration) error {
			if !running {
				return fault("inventory-unavailable")
			}
			return nil
		}
		g.p.Step = func(_ context.Context, op, slot string, end time.Duration, extra ProcessExtra) (StepResult, error) {
			order = append(order, op)
			if op == "control-start" {
				running = true
				return StepResult{}, nil
			}
			if !running {
				if unknown {
					return StepResult{}, fault("command-result-unknown")
				}
				return StepResult{}, fault("precondition-failed:control-unavailable")
			}
			return StepResult{LossKnown: true, Status: controlplane.Status{Ready: true, Workers: []controlplane.WorkerStatus{{Name: "w1", Address: g.c.Parameters.Workers["w1"].Media}, {Name: "w2", Address: g.c.Parameters.Workers["w2"].Media}}}}, nil
		}
		err := GatePrecondition(context.Background(), g.c, g.p, g.end)
		if unknown {
			if code(err) != "command-result-unknown" || strings.Join(order, ",") != "status" {
				t.Fatal("MUTATION: precondition-unknown-fence")
			}
		} else {
			if err != nil || strings.Join(order, ",") != "status,control-start,status" {
				t.Fatal("MUTATION: precondition-control-repair")
			}
		}
	}
}
func TestPreconditionDrainingWorkerMustBeEmpty(t *testing.T) {
	for _, empty := range []bool{false, true} {
		g, _ := testProcedure(t)
		down, draining := false, true
		stopped := 0
		g.p.Inventory = func(context.Context, string, time.Duration) error { return nil }
		g.p.Step = func(_ context.Context, op, slot string, end time.Duration, extra ProcessExtra) (StepResult, error) {
			switch op {
			case "worker-empty":
				if !empty {
					return StepResult{}, fault("precondition-failed:worker-not-empty")
				}
			case "worker-stop":
				down = true
				stopped++
			case "worker-start":
				down = false
				draining = false
			}
			return StepResult{LossKnown: true, Status: controlplane.Status{Ready: true, Workers: []controlplane.WorkerStatus{{Name: "w1", Address: g.c.Parameters.Workers["w1"].Media, Dead: down, Draining: draining}, {Name: "w2", Address: g.c.Parameters.Workers["w2"].Media}}}}, nil
		}
		err := GatePrecondition(context.Background(), g.c, g.p, g.end)
		if empty {
			if err != nil || stopped != 1 || draining || down {
				t.Fatal("MUTATION: precondition-draining-repair")
			}
		} else {
			if code(err) != "precondition-unhealthy" || stopped != 0 {
				t.Fatal("MUTATION: precondition-empty-proof")
			}
		}
	}
}

func TestRestoreWorkersWaitsForConfirmedStopDespiteLiveHeartbeat(t *testing.T) {
	for _, mode := range []string{"confirmed-stop", "refused-stop", "unknown-stop", "unsettled-stop"} {
		t.Run(mode, func(t *testing.T) {
			g, _ := testProcedure(t)
			running, draining, reads := true, true, 0
			var effects []string
			g.p.Step = func(_ context.Context, op, _ string, _ time.Duration, _ ProcessExtra) (StepResult, error) {
				if op != "status" {
					effects = append(effects, op)
				}
				switch op {
				case "worker-stop":
					if mode == "refused-stop" {
						return StepResult{}, fault("precondition-failed:worker-not-empty")
					}
					if mode == "unknown-stop" {
						return StepResult{Certainty: operator.Unknown}, fault("command-result-unknown")
					}
					running = false
				case "status":
					reads++
				case "replace":
					if running || reads < 3 {
						t.Fatal("replace before settled death")
					}
					draining = false
				case "worker-start":
					if draining {
						t.Fatal("start before replace")
					}
					running = true
				}
				dead := !running && reads >= 3 && mode != "unsettled-stop"
				return StepResult{Certainty: operator.Succeeded, LossKnown: true, Status: controlplane.Status{Ready: true, Workers: []controlplane.WorkerStatus{{Name: "w1", Address: g.c.Parameters.Workers["w1"].Media, Dead: dead, Draining: draining}}}}, nil
			}
			_, _ = g.do(context.Background(), "worker-stop", "w1", ProcessExtra{Worker: "w1"})
			err := g.restoreWorkers(context.Background())
			switch mode {
			case "confirmed-stop":
				if err != nil || strings.Join(effects, ",") != "worker-stop,replace,worker-start" || !running || draining || len(g.stopped) != 0 {
					t.Fatal(err, effects, running, draining)
				}
			case "refused-stop":
				if err != nil || len(effects) != 1 || !running {
					t.Fatal("refused stop changed a live worker", err, effects)
				}
			case "unknown-stop":
				if code(err) != "command-result-unknown" || len(effects) != 1 || reads != 0 {
					t.Fatal("unknown stop authorized restoration", err, effects)
				}
			case "unsettled-stop":
				if code(err) != "restore-unsettled" || len(effects) != 1 || running {
					t.Fatal("missing death proof authorized restoration", err, effects)
				}
			}
		})
	}
}

func TestTrialStartsBeforeDeadline(t *testing.T) {
	g, clock := testProcedure(t)
	clock.Advance(GateMaximum)
	calls := 0
	g.p.Step = func(context.Context, string, string, time.Duration, ProcessExtra) (StepResult, error) {
		calls++
		return StepResult{}, nil
	}
	err := g.trials(context.Background(), TrialTarget{Moves: 1}, "cutoff")
	if code(err) != "gate-deadline" || calls != 0 {
		t.Fatal("MUTATION: trial-cutoff")
	}
}
func TestPreconditionUnhealthyRejected(t *testing.T) {
	g, _ := testProcedure(t)
	g.p.Inventory = func(context.Context, string, time.Duration) error { return fault("inventory-unavailable") }
	g.p.Step = func(context.Context, string, string, time.Duration, ProcessExtra) (StepResult, error) {
		return StepResult{LossKnown: true, Status: controlplane.Status{Ready: true, Workers: []controlplane.WorkerStatus{{Name: "w1", Address: g.c.Parameters.Workers["w1"].Media}, {Name: "w2", Address: g.c.Parameters.Workers["w2"].Media}}}}, nil
	}
	if err := GatePrecondition(context.Background(), g.c, g.p, g.end); code(err) != "precondition-unhealthy" {
		t.Fatal("MUTATION: precondition-unhealthy")
	}
}

func TestMeasurementKnownAllowanceFailureDominates(t *testing.T) {
	g, _ := testProcedure(t)
	p := MeasurePorts{ProcedurePorts: g.p, Calls: 1}
	p.Observe = func(context.Context, time.Duration) (MeasurementObservation, error) {
		s := controlplane.Status{Ready: true, Calls: []controlplane.CallStatus{{ID: "original", Owner: "w1", Epoch: 1}}, Workers: []controlplane.WorkerStatus{{Name: "w1", Address: g.c.Parameters.Workers["w1"].Media}, {Name: "w2", Address: g.c.Parameters.Workers["w2"].Media}}, Pool: controlplane.PoolSample{Cap: 2}}
		return MeasurementObservation{Status: &s, Local: map[string]mediaworker.WorkerStatus{"w1": {InventoryKnown: true, Sessions: 1, Inventory: []mediaworker.InstalledSession{{ID: "original", Epoch: 1, Owner: g.c.Parameters.Workers["w1"].Media}}}, "w2": {InventoryKnown: true, Inventory: []mediaworker.InstalledSession{}}}}, nil
	}
	p.Collect = func(context.Context, time.Duration) <-chan MeasurementCollection {
		ch := make(chan MeasurementCollection, 1)
		ch <- MeasurementCollection{Evidence: MeasurementEvidence{KnownAllowanceViolation: true}}
		return ch
	}
	r := RunMeasureGate(context.Background(), operator.Load, g.c, p, g.end)
	if r.Status != operator.Failure {
		t.Fatal("MUTATION: known-allowance-failure")
	}
}

func TestRelayUnknownStopsPolling(t *testing.T) {
	g, clock := testProcedure(t)
	reads, calls := 0, 0
	g.p.Step = func(_ context.Context, op, slot string, end time.Duration, extra ProcessExtra) (StepResult, error) {
		calls++
		if op == "relay-inspect" {
			reads++
			if reads == 2 {
				return StepResult{}, fault("command-result-unknown")
			}
		}
		return StepResult{}, nil
	}
	err := g.relay(context.Background(), false, 1)
	if code(err) != "command-result-unknown" || calls != 4 || reads != 2 || clock.Elapsed() != 0 {
		t.Fatal("MUTATION: relay-unknown-no-poll")
	}
}
func TestArbitraryErrorCannotAuthorizeRestore(t *testing.T) {
	g, _ := testProcedure(t)
	g.down = []string{"w1"}
	g.frozen = true
	calls := 0
	g.p.Step = func(context.Context, string, string, time.Duration, ProcessExtra) (StepResult, error) {
		calls++
		return StepResult{}, errors.New("precondition-failed:invented")
	}
	_, err := g.do(context.Background(), "status", "shared", ProcessExtra{})
	r := g.finish(context.Background(), err, false)
	if !g.unknown || calls != 1 || r.Reason != "command-result-unknown" {
		t.Fatal("MUTATION: untyped-error-restoration")
	}
}
func TestOptionalStoreUnknownIsNeverReplayed(t *testing.T) {
	s, _ := fixtureSession(t)
	peer := &observationPeer{respond: func(req operator.Request) operator.Reply {
		r := operator.Reply{Transport: operator.Succeeded, ExitCode: new(int), Payload: json.RawMessage(`{"completed":true}`)}
		if req.Operation == operator.StoreFailover {
			r.Transport = operator.Unknown
		}
		return r
	}}
	if s.OptionalStore(peer) != nil {
		t.Fatal("missing optional capability became available")
	}
	s.capabilities[operator.StoreObserve] = true
	s.capabilities[operator.StoreFailover] = true
	store := s.OptionalStore(peer)
	if store == nil {
		t.Fatal("complete optional capability missing")
	}
	if code(store.Failover(context.Background(), time.Minute)) != "command-result-unknown" {
		t.Fatal("store unknown was lost")
	}
	_ = store.Failover(context.Background(), time.Minute)
	if peer.sent != 1 || !s.Latched() {
		t.Fatal("MUTATION: optional-store-unknown-fence")
	}
}

func TestProcessEndpointsAreResourceNeutral(t *testing.T) {
	g, _ := testProcedure(t)
	p := g.c.Parameters
	p.Control = "http://control.test:31010"
	p.Relay = "http://relay.test:31000"
	p.Workers["w1"] = WorkerEndpoint{HTTP: "http://worker-one.test:31001", Media: netip.MustParseAddrPort("192.0.2.30:31011")}
	if err := ValidateProcessParameters(p); err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []string{"http://control.test:0", "http://control.test:65536", "https://control.test:31010", "http://token@control.test:31010", "http://control.test:31010/other"} {
		p.Control = endpoint
		if ValidateProcessParameters(p) == nil {
			t.Fatal("invalid endpoint admitted", endpoint)
		}
	}
}

func TestUnknownMediaCannotEnterPool(t *testing.T) {
	g, _ := testProcedure(t)
	g.c.Pool = true
	g.p.Media = func(context.Context) (*MediaSummary, error) {
		return &MediaSummary{Status: operator.Passed}, fault("command-result-unknown")
	}
	r := g.finish(context.Background(), nil, false)
	if r.AwaitingPool || r.Reason != "command-result-unknown" || r.Status == operator.Passed {
		t.Fatal("unknown media was promoted into pool")
	}
}
func TestMissingPhaseCannotBePooled(t *testing.T) {
	s, _ := fixtureSession(t)
	r := s.RunSequence(context.Background(), SequencePorts{Command: func(context.Context, operator.Gate, time.Duration) ProcedureResult {
		return ProcedureResult{Status: operator.Inconclusive, AwaitingPool: true}
	}, Finalize: func(context.Context, time.Duration) (*MediaSummary, error) {
		return &MediaSummary{Status: operator.Passed}, nil
	}})
	if r.Status == operator.Passed || r.Gates[0].Status == operator.Passed {
		t.Fatal("a phase without media was promoted by final pool")
	}
}

func TestIncompleteStatusKeepsKnownLossAndUnknown(t *testing.T) {
	s, _ := fixtureSession(t)
	g, _ := testProcedure(t)
	peer := &observationPeer{respond: func(operator.Request) operator.Reply {
		return hostReply(t, []byte(`{"kind":"ok","started":"1970-01-01T00:16:40Z","ended":"1970-01-01T00:16:40Z","data":{"lost_count":1,"calls":"malformed-unrelated-field"}}`))
	}}
	runtime := ProcessRuntime{Session: s, Peer: peer, Config: g.c}
	g.p.Step = runtime.Step
	_, err := g.do(context.Background(), "status", "shared", ProcessExtra{})
	r := g.finish(context.Background(), err, false)
	if r.Status != operator.Failure || r.Reason != "command-result-unknown" || !s.Latched() || peer.sent != 1 {
		t.Fatal("known loss was erased by missing evidence or cleared uncertainty")
	}
}
func TestIncompleteRelayKeepsKnownSelfFence(t *testing.T) {
	s, _ := fixtureSession(t)
	g, _ := testProcedure(t)
	peer := &observationPeer{respond: func(operator.Request) operator.Reply {
		return hostReply(t, []byte(`{"kind":"ok","started":"1970-01-01T00:16:40Z","ended":"1970-01-01T00:16:40Z","data":{"status":{"stats":{"SelfFences":1}}}}`))
	}}
	runtime := ProcessRuntime{Session: s, Peer: peer, Config: g.c}
	g.p.Step = runtime.Step
	err := g.relay(context.Background(), false, 1)
	r := g.finish(context.Background(), err, false)
	if r.Status != operator.Failure || s.Latched() || peer.sent != 1 {
		t.Fatal("known self-fence was erased or a definite read was replayed")
	}
}

func TestWorkerStartRequiresCompletionProof(t *testing.T) {
	runtime, peer, _ := runtimeFixture(t)
	respond := peer.respond
	peer.respond = func(req operator.Request) operator.Reply {
		if req.Action == "worker-start" {
			return hostReply(t, []byte(`{"kind":"ok","started":"1970-01-01T00:16:40Z","ended":"1970-01-01T00:16:40Z","data":{"completed":false}}`))
		}
		return respond(req)
	}
	g := newProcedure(runtime.Config, ProcedurePorts{Clock: runtime.Session.clock, Step: runtime.Step}, time.Minute)
	g.down = []string{"w1"}
	_, err := g.do(context.Background(), "worker-start", "w1", ProcessExtra{Worker: "w1"})
	if code(err) != "command-result-unknown" || !g.unknown || len(g.down) != 1 || !runtime.Session.Latched() {
		t.Fatal("an unconfirmed start cleared tracked restoration ownership")
	}
}

type uncertainStoreRead struct{ reads, failovers, unknownAt int }

func (s *uncertainStoreRead) Observe(context.Context, time.Duration) (StoreObservation, error) {
	s.reads++
	if s.reads >= max(2, s.unknownAt) {
		return StoreObservation{}, fault("command-result-unknown")
	}
	return StoreObservation{Primary: "primary-a", Available: true, Durability: "sync"}, nil
}
func (s *uncertainStoreRead) Failover(context.Context, time.Duration) error {
	s.failovers++
	return nil
}
func TestFailoverDoesNotReplayUnknownStoreRead(t *testing.T) {
	g, _ := testProcedure(t)
	store := &uncertainStoreRead{}
	p := MeasurePorts{ProcedurePorts: g.p, Calls: 1, Store: store, BeforeFault: func(context.Context, time.Duration) error { return nil }, MarkFault: func(context.Context) error { return nil }}
	p.Observe = func(context.Context, time.Duration) (MeasurementObservation, error) {
		s := controlplane.Status{Ready: true, Pool: controlplane.PoolSample{Cap: 2}, Calls: []controlplane.CallStatus{{ID: "original", Owner: "w1", Epoch: 1}}, Workers: []controlplane.WorkerStatus{{Name: "w1", Address: g.c.Parameters.Workers["w1"].Media}, {Name: "w2", Address: g.c.Parameters.Workers["w2"].Media}}}
		return MeasurementObservation{Status: &s}, nil
	}
	r := RunMeasureGate(context.Background(), operator.Failover, g.c, p, g.end)
	if store.reads != 2 || store.failovers != 1 || r.Reason != "command-result-unknown" {
		t.Fatal("an unknown store observation was replayed")
	}
}

func TestSignalBracketKeepsUnsignedNanoseconds(t *testing.T) {
	runtime, peer, _ := runtimeFixture(t)
	respond := peer.respond
	peer.respond = func(req operator.Request) operator.Reply {
		if req.Action == "worker-down" {
			return hostReply(t, []byte(`{"kind":"ok","started":"1970-01-01T00:16:40Z","ended":"1970-01-01T00:16:40Z","data":{"completed":true,"signalBeforeUnixNs":"9999999999999999998","signalAfterUnixNs":"9999999999999999998"}}`))
		}
		return respond(req)
	}
	r, err := runtime.Step(context.Background(), "worker-down", "w1", time.Minute, ProcessExtra{Worker: "w1"})
	if err != nil || !r.Started.Equal(time.Unix(9999999999, 999999998)) {
		t.Fatal("signal nanoseconds were rounded or overflowed", err, r.Started)
	}
}

func TestWarmupKeepsSafeIntegerAndDeadlineBounds(t *testing.T) {
	for _, registered := range []int64{1 << 53, -1 << 53, 1<<63 - 1} {
		g, _ := testProcedure(t)
		g.p.Broker.Registration().RegisteredAt = &registered
		if code(TrialWarmup(context.Background(), g.p, g.end)) != "Missing pre-fault registration" {
			t.Fatal("unsafe registration timestamp was accepted")
		}
	}
	g, clock := testProcedure(t)
	clock.Advance(time.Second)
	registered := int64(maxSafeEpoch)
	g.p.Broker.Registration().RegisteredAt = &registered
	if code(TrialWarmup(context.Background(), g.p, g.end)) != "gate-deadline" {
		t.Fatal("warmup deadline overflowed")
	}
}

// A complete control restart exercises reconstruction, the drain/move and the
// second fault before the test chooses whether ownership actually advances.
func controlRestartProcedure(t *testing.T, advanceEpoch bool) (*procedure, *FakeClock, *[]string) {
	t.Helper()
	g, clock := testProcedure(t)
	operations := []string{}
	call := controlplane.CallStatus{ID: "original", Owner: "w1", Epoch: 1}
	dead, draining := map[string]bool{}, map[string]bool{}
	var takeover *controlplane.MoveResult
	kills := 0
	g.p.Step = func(_ context.Context, op, _ string, _ time.Duration, extra ProcessExtra) (StepResult, error) {
		operations = append(operations, op+":"+extra.Worker)
		r := StepResult{Started: clock.Wall(), Ended: clock.Wall()}
		switch op {
		case "status":
			r.Status = controlplane.Status{Ready: true, Pool: controlplane.PoolSample{Cap: 2}, Calls: []controlplane.CallStatus{call}}
			for _, w := range []string{"w1", "w2"} {
				r.Status.Workers = append(r.Status.Workers, controlplane.WorkerStatus{Name: w, Address: g.c.Parameters.Workers[w].Media, Dead: dead[w], Draining: draining[w]})
			}
			if takeover != nil {
				r.Status.Takeovers = []controlplane.MoveResult{*takeover}
				// Preserve the original omitted-lost representation.
				r.History = []json.RawMessage{json.RawMessage(`{"id":"original","from":"w2","to":"w1","kind":"takeover","end":"1970-01-01T00:16:40Z"}`)}
			}
		case "control-start":
			ready := int64(1)
			r.ReadyMS = &ready
		case "worker-down":
			dead[extra.Worker] = true
			kills++
			if kills == 2 {
				call.Owner = "w1"
				if advanceEpoch {
					call.Epoch++
				}
				takeover = &controlplane.MoveResult{ID: call.ID, From: "w2", To: "w1", Kind: "takeover", End: clock.Wall()}
			}
		case "worker-stop":
			dead[extra.Worker] = true
		case "worker-start":
			dead[extra.Worker], draining[extra.Worker] = false, false
		case "drain":
			draining[extra.Worker] = true
			move := controlplane.MoveResult{ID: call.ID, From: call.Owner, To: "w2", Kind: "move", Start: clock.Wall(), End: clock.Wall()}
			r.Drain.Moves = []controlplane.MoveResult{move}
			r.Moves = []json.RawMessage{oracleJSON(move)}
			call.Owner, call.Epoch = "w2", call.Epoch+1
		case "control-stop", "replace", "worker-empty":
		default:
			t.Fatalf("unexpected control procedure step %s", op)
		}
		return r, nil
	}
	g.p.Media = func(context.Context) (*MediaSummary, error) { return &MediaSummary{Status: operator.Passed}, nil }
	return g, clock, &operations
}

func TestControlTakeoverRequiresOwnershipBeforeDeadline(t *testing.T) {
	g, clock, operations := controlRestartProcedure(t, false)
	r := RunProcessGate(context.Background(), operator.ControlRestart, g.c, g.p, g.end)
	events := g.p.Broker.Events()
	starts := 0
	for _, op := range *operations {
		if op == "worker-start:w2" {
			starts++
		}
	}
	if r.Status != operator.Inconclusive || r.Reason != "gate-deadline" || clock.Elapsed() != g.end || len(events) != 1 || events[0].Kind != "move" || starts != 1 || (*operations)[len(*operations)-1] != "status:" {
		t.Fatalf("MUTATION: takeover without ownership must expire without feed or restoration: result=%+v events=%+v operations=%v", r, events, *operations)
	}
}

func TestFinalControlStatusPreservesKnownLoss(t *testing.T) {
	for _, malformed := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "malformed-unknown"}[malformed], func(t *testing.T) {
			g, _, _ := controlRestartProcedure(t, true)
			g.c.Pool = true
			collected := false
			step := g.p.Step
			g.p.Step = func(ctx context.Context, op, slot string, end time.Duration, extra ProcessExtra) (StepResult, error) {
				if collected && op == "status" {
					r := StepResult{Status: controlplane.Status{LostCount: 1}}
					if malformed {
						session, _ := fixtureSession(t)
						peer := &observationPeer{respond: func(operator.Request) operator.Reply {
							return hostReply(t, []byte(`{"kind":"ok","started":"1970-01-01T00:16:40Z","ended":"1970-01-01T00:16:40Z","data":{"lost_count":1,"calls":"malformed-unrelated-field"}}`))
						}}
						runtime := ProcessRuntime{Session: session, Peer: peer, Config: g.c}
						return runtime.Step(ctx, op, slot, end, extra)
					}
					return r, nil
				}
				return step(ctx, op, slot, end, extra)
			}
			g.p.Media = func(context.Context) (*MediaSummary, error) {
				collected = true
				return &MediaSummary{Status: operator.Passed}, nil
			}
			r := RunProcessGate(context.Background(), operator.ControlRestart, g.c, g.p, g.end)
			if !collected || r.Status != operator.Failure || r.AwaitingPool || (malformed && r.Reason != "command-result-unknown") {
				t.Fatalf("MUTATION: final status loss must fail, retaining uncertainty: %+v", r)
			}
		})
	}
}

func TestSequenceDefiniteStopsAndPreconditionsContinue(t *testing.T) {
	for _, tc := range []struct {
		name, prep, reason, category string
		status                       operator.Verdict
		stops                        bool
	}{
		{name: "failure", status: operator.Failure, stops: true},
		{name: "gate-deadline", status: operator.Inconclusive, reason: "gate-deadline", stops: true},
		{name: "plain-precondition", prep: "precondition-failed", reason: "precondition-failed"},
		{name: "categorized-precondition", prep: "precondition-failed:inventory", reason: "precondition-failed", category: "inventory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, clock := fixtureSession(t)
			s.gates = []operator.Gate{operator.Rollout, operator.ControlRestart}
			commands := []operator.Gate{}
			r := s.RunSequence(context.Background(), SequencePorts{
				Prepare: func(_ context.Context, gate operator.Gate, _, _ time.Duration) error {
					if gate == operator.Rollout && tc.prep != "" {
						return fault(tc.prep)
					}
					return nil
				},
				Command: func(_ context.Context, gate operator.Gate, end time.Duration) ProcedureResult {
					commands = append(commands, gate)
					if gate == operator.Rollout {
						if tc.reason == "gate-deadline" {
							clock.Advance(end - clock.Elapsed())
						}
						return ProcedureResult{Status: tc.status, Reason: tc.reason, Media: &MediaSummary{Status: operator.Passed}}
					}
					return ProcedureResult{Status: operator.Passed, Media: &MediaSummary{Status: operator.Passed}}
				},
			})
			if tc.stops {
				if s.Budget.Remaining() <= 0 {
					t.Fatal("gate stop must be tested while session time remains")
				}
				if len(commands) != 1 || commands[0] != operator.Rollout || r.Gates[0].Status != tc.status || r.Gates[0].Reason != tc.reason || r.Gates[1].Reason != "prior-gate-stopped" {
					t.Fatalf("MUTATION: definite gate stop policy: %+v commands=%v", r, commands)
				}
			} else if len(commands) != 1 || commands[0] != operator.ControlRestart || s.Latched() || r.Gates[0].Reason != tc.reason || r.Procedures[0].Category != tc.category || r.Gates[1].Status != operator.Passed {
				t.Fatalf("MUTATION: definite precondition must preserve classification and continue: %+v commands=%v", r, commands)
			}
		})
	}
}

func TestSequenceInconclusiveMediaCannotPass(t *testing.T) {
	s, _ := fixtureSession(t)
	s.gates = []operator.Gate{operator.Rollout}
	r := s.RunSequence(context.Background(), SequencePorts{Command: func(context.Context, operator.Gate, time.Duration) ProcedureResult {
		return ProcedureResult{Status: operator.Passed, Media: &MediaSummary{Status: operator.Inconclusive}}
	}})
	if r.Gates[0].Status != operator.Inconclusive || r.Status != operator.Inconclusive {
		t.Fatalf("MUTATION: inconclusive media cannot pass: %+v", r)
	}
}

func TestCollectionUnknownStopsObservationsAndPreservesFailure(t *testing.T) {
	for _, failure := range []string{"none", "observation", "prior-observation", "collection"} {
		t.Run(failure, func(t *testing.T) {
			g, clock := testProcedure(t)
			observations := 0
			collected := make(chan MeasurementCollection, 1)
			p := MeasurePorts{ProcedurePorts: g.p, Calls: 1}
			p.Observe = func(context.Context, time.Duration) (MeasurementObservation, error) {
				observations++
				if observations == 2 && failure == "prior-observation" {
					return MeasurementObservation{Status: &controlplane.Status{LostCount: 1}}, nil
				}
				if observations > 1 {
					s := &controlplane.Status{}
					if failure == "observation" {
						s.LostCount = 1
					}
					return MeasurementObservation{Status: s}, fault("command-result-unknown")
				}
				s := controlplane.Status{Ready: true, Calls: []controlplane.CallStatus{{ID: "original", Owner: "w1", Epoch: 1}}, Workers: []controlplane.WorkerStatus{{Name: "w1", Address: g.c.Parameters.Workers["w1"].Media}, {Name: "w2", Address: g.c.Parameters.Workers["w2"].Media}}, Pool: controlplane.PoolSample{Cap: 2}}
				return MeasurementObservation{Status: &s, Local: map[string]mediaworker.WorkerStatus{"w1": {InventoryKnown: true, Sessions: 1, Inventory: []mediaworker.InstalledSession{{ID: "original", Epoch: 1, Owner: g.c.Parameters.Workers["w1"].Media}}}, "w2": {InventoryKnown: true, Inventory: []mediaworker.InstalledSession{}}}}, nil
			}
			p.Collect = func(ctx context.Context, _ time.Duration) <-chan MeasurementCollection {
				if failure == "none" {
					go func() { <-ctx.Done(); close(collected) }()
				}
				return collected
			}
			p.Sleep = func(_ context.Context, d time.Duration) error {
				clock.Advance(d)
				// Retain terminal evidence that arrives after the unknown observation.
				doneAt := 30 * time.Second
				if failure == "prior-observation" {
					doneAt = 45 * time.Second
				}
				if clock.Elapsed() == doneAt && failure != "none" {
					collected <- MeasurementCollection{Evidence: MeasurementEvidence{KnownAllowanceViolation: failure == "collection", Media: &MediaSummary{Status: operator.Inconclusive}}}
				}
				return nil
			}
			r := RunMeasureGate(context.Background(), operator.Load, g.c, p, time.Minute)
			want := operator.Inconclusive
			if failure != "none" {
				want = operator.Failure
			}
			reads, samples := 2, 1
			if failure == "prior-observation" {
				reads, samples = 3, 2
			}
			if observations != reads || r.Reason != "command-result-unknown" || r.Status != want || r.Measurement.Samples != samples {
				t.Fatalf("MUTATION: collection uncertainty must stop reads and retain failures: reads=%d result=%+v", observations, r)
			}
		})
	}
}

func TestSequenceReleasesCallers(t *testing.T) {
	for _, mode := range []string{"precondition", "skipped", "completed", "failed", "unknown-stop"} {
		t.Run(mode, func(t *testing.T) {
			s, clock := fixtureSession(t)
			s.gates = []operator.Gate{operator.Rollout, operator.ControlRestart}
			released, commands := 0, 0
			result := s.RunSequence(context.Background(), SequencePorts{
				Minimum: func(g operator.Gate) time.Duration {
					if mode == "skipped" && g == operator.Rollout {
						return 21 * time.Minute
					}
					return 0
				},
				Prepare: func(_ context.Context, gate operator.Gate, _, _ time.Duration) error {
					if gate == operator.Rollout {
						if mode == "precondition" {
							return fault("precondition-failed:inventory")
						}
					}
					return nil
				},
				Command: func(context.Context, operator.Gate, time.Duration) ProcedureResult {
					commands++
					status := operator.Passed
					if mode == "failed" {
						status = operator.Failure
					}
					return ProcedureResult{Status: status, Media: &MediaSummary{Status: status}}
				},
				ReleaseCallers: func(_ context.Context, _ string, end time.Duration) bool {
					released++
					if end > s.Budget.end || end > clock.Elapsed()+time.Minute {
						t.Fatal("MUTATION: caller-release-deadline")
					}
					return mode != "unknown-stop"
				},
			})
			if mode == "skipped" {
				if released != 2 || commands != 1 || result.Gates[0].Status != operator.NotRun {
					t.Fatal("MUTATION: skipped-caller-release", released, commands)
				}
			} else if mode == "failed" || mode == "unknown-stop" {
				if released != 1 || commands != 1 || result.Gates[1].Reason != "prior-gate-stopped" {
					t.Fatal("MUTATION: unconfirmed-caller-release", released, commands, result)
				}
			} else if released != 2 || commands != map[bool]int{true: 1, false: 2}[mode == "precondition"] {
				t.Fatal("MUTATION: caller-release-once", released, commands)
			}
			if mode == "unknown-stop" && (result.Status == operator.Passed || result.Gates[0].Reason != "command-result-unknown") {
				t.Fatal("MUTATION: release-unknown-retained", result)
			}
		})
	}
}

func TestSequenceCannotInventPooledMedia(t *testing.T) {
	for _, awaiting := range []bool{false, true} {
		s, _ := fixtureSession(t)
		s.gates = []operator.Gate{operator.Rollout}
		result := s.RunSequence(context.Background(), SequencePorts{Command: func(context.Context, operator.Gate, time.Duration) ProcedureResult {
			r := ProcedureResult{Status: operator.Passed, AwaitingPool: awaiting}
			if awaiting {
				r.Status = operator.Inconclusive
				r.Media = &MediaSummary{Status: operator.Inconclusive}
			}
			return r
		}, Finalize: func(context.Context, time.Duration) (*MediaSummary, error) { return nil, nil }})
		if result.Status == operator.Passed || result.Gates[0].Status == operator.Passed {
			t.Fatal("MUTATION: no-invented-media", awaiting, result)
		}
	}
}

func TestAdmissionUnknownSurvivorStops(t *testing.T) {
	for _, status := range []operator.Verdict{operator.Inconclusive, operator.Failure} {
		r := RunAdmission(context.Background(), AdmissionPorts{
			Survivor: func(context.Context, time.Duration) MeasureResult {
				return MeasureResult{Status: status, Reason: "command-result-unknown"}
			},
			StopSurvivor: func(context.Context, time.Duration) bool { t.Fatal("MUTATION: admission-after-unknown"); return true },
		}, time.Minute)
		if r.Reason != "command-result-unknown" || r.Status != status {
			t.Fatal("MUTATION: admission-after-unknown", r)
		}
	}
}

func TestPreconditionCategoryPublication(t *testing.T) {
	for _, category := range []string{"inventory", "inventory:raw", "inventory:raw:extra", "raw text", "", strings.Repeat("a", 65)} {
		s, _ := fixtureSession(t)
		s.gates = []operator.Gate{operator.Rollout}
		result := s.RunSequence(context.Background(), SequencePorts{Prepare: func(context.Context, operator.Gate, time.Duration, time.Duration) error {
			return fault("precondition-failed:" + category)
		}})
		want := ""
		if category == "inventory" || category == "inventory:raw" {
			want = category
		}
		if result.Procedures[0].Category != want || result.Gates[0].Reason != "precondition-failed" {
			t.Fatal("MUTATION: precondition-public-category", category, result)
		}
	}
}

func TestMeasurementPublicationFields(t *testing.T) {
	var measure Measurement
	if err := json.Unmarshal([]byte(`{"samples":1,"missingSamples":0,"raw":"untrusted","caller":"untrusted","unexpected":12}`), &measure); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(measure)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || len(fields) != 2 || fields["samples"] == nil || fields["missingSamples"] == nil {
		t.Fatal("MUTATION: typed-public-measurement", string(raw), err)
	}
}

// A real clock advances between deadline creation and admission. Caller setup
// (including the probe burst) precedes the fresh gate window.
type advancingGateClock struct{ FakeClock }

func (c *advancingGateClock) Elapsed() time.Duration {
	c.Advance(time.Microsecond)
	return c.Tick
}

func TestSequenceFullLengthCampaignAfterPreparation(t *testing.T) {
	for _, elapsed := range []time.Duration{5 * time.Minute, 84 * time.Minute} {
		t.Run(elapsed.String(), func(t *testing.T) {
			s, initial := fixtureSession(t)
			clock := &advancingGateClock{FakeClock: *initial}
			s.clock, s.Budget.clock = clock, clock
			clock.Advance(elapsed)
			prepared, commands := 0, 0
			result := s.RunSequence(context.Background(), SequencePorts{
				Minimum: func(operator.Gate) time.Duration { return 1020*time.Second + 180*time.Second },
				Prepare: func(_ context.Context, _ operator.Gate, end, gateEnd time.Duration) error {
					prepared++
					if end > gateEnd || end <= clock.Elapsed()+120*time.Second {
						t.Fatal("preparation lost its separate budget")
					}
					clock.Advance(120 * time.Second)
					return nil
				},
				Command: func(_ context.Context, _ operator.Gate, end time.Duration) ProcedureResult {
					commands++
					if end-clock.Elapsed() > GateMaximum || end > s.Budget.MutationEnd() {
						t.Fatal("gate exceeded policy cutoff")
					}
					return ProcedureResult{Status: operator.Passed, Media: &MediaSummary{Status: operator.Passed}}
				},
				Finalize: func(context.Context, time.Duration) (*MediaSummary, error) {
					return &MediaSummary{Status: operator.Passed}, nil
				},
			})
			if elapsed == 5*time.Minute {
				if prepared != 1 || commands != 1 || result.Gates[0].Reason != "" || result.Status != operator.Passed {
					t.Fatalf("full-length campaign skipped after setup: prepared=%d commands=%d result=%+v", prepared, commands, result)
				}
			} else if commands != 0 || result.Gates[0].Reason != "gate-deadline" {
				t.Fatalf("late campaign escaped minute-103 cutoff: %+v", result)
			}
		})
	}
}

func TestProcedureCompletionIsIndependentOfMediaTiming(t *testing.T) {
	for _, procedureErr := range []error{nil, fault("precondition-failed:inventory"), fault("gate-deadline"), fault("command-result-unknown")} {
		g, _ := testProcedure(t)
		g.p.Media = func(context.Context) (*MediaSummary, error) { return &MediaSummary{Status: operator.Inconclusive}, nil }
		result := g.finish(context.Background(), procedureErr, false)
		if result.Completed != (procedureErr == nil) || result.Status != operator.Inconclusive {
			t.Fatal("procedure completion confused with strict timing", result, procedureErr)
		}
	}
}
