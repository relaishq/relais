package operatorgates

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/operator"
)

type procedureOracle struct {
	t         *testing.T
	events    []OracleEvent
	root      OracleEvent
	result    OracleEvent
	pos       int
	prefix    string
	clock     *FakeClock
	reg       *Registration
	run       string
	receipts  []Receipt
	callbacks []string
	source    string
}

func (o *procedureOracle) relevant(e OracleEvent) bool {
	if e.Kind != "request" {
		return false
	}
	for _, suffix := range o.callbacks {
		if strings.HasSuffix(e.Operation, suffix) {
			return true
		}
	}
	return false
}
func (o *procedureOracle) sync() {
	for o.pos < len(o.events) {
		e := o.events[o.pos]
		if o.relevant(e) {
			break
		}
		o.pos++
		if e.Kind == "result" && strings.HasSuffix(e.Operation, ".now") {
			var n int64
			if json.Unmarshal(e.Value, &n) == nil {
				o.clock.Now = time.UnixMilli(n)
				o.clock.Tick = time.Duration(n) * time.Millisecond
			}
		}
	}
}
func (o *procedureOracle) call(suffix string, args ...any) OracleEvent {
	o.t.Helper()
	o.sync()
	if o.pos >= len(o.events) {
		o.t.Fatalf("MUTATION: procedure-oracle extra callback %s", suffix)
	}
	e := o.events[o.pos]
	o.pos++
	if !strings.HasSuffix(e.Operation, suffix) {
		o.t.Fatalf("MUTATION: procedure-oracle order: got %s want %s at %d", suffix, e.Operation, e.ID)
	}
	for i, a := range args {
		if i >= len(e.Inputs) {
			o.t.Fatalf("MUTATION: procedure-oracle callback args %s", suffix)
		}
		if !oracleEqual(a, e.Inputs[i]) {
			o.t.Fatalf("MUTATION: procedure-oracle %s arg %d: got %s want %s", suffix, i, oracleJSON(a), e.Inputs[i])
		}
	}
	for o.pos < len(o.events) {
		r := o.events[o.pos]
		o.pos++
		if r.Kind == "result" && r.ID == e.ID {
			o.sync()
			return r
		}
	}
	o.t.Fatalf("MUTATION: procedure-oracle missing result %d", e.ID)
	return OracleEvent{}
}
func oracleJSON(v any) []byte { b, _ := json.Marshal(v); return b }
func oracleEqual(a any, b json.RawMessage) bool {
	var x, y any
	_ = json.Unmarshal(oracleJSON(a), &x)
	_ = json.Unmarshal(b, &y)
	return reflect.DeepEqual(x, y)
}
func oracleError(e OracleEvent) error {
	if e.Completion == "threw" {
		return fault(e.Error.Message)
	}
	return nil
}
func (o *procedureOracle) RunID() string               { return o.run }
func (o *procedureOracle) Registration() *Registration { return o.reg }
func (o *procedureOracle) Events() []Receipt           { return o.receipts }
func (o *procedureOracle) Arm(_ context.Context, id string, ids []Identity) error {
	return oracleError(o.call(".broker.arm", id, ids))
}
func (o *procedureOracle) Feed(_ context.Context, id string, receipts []Receipt) error {
	// Compare receipt instants separately: Go's RFC3339 encoder omits zero
	// fractional seconds while the oracle's JS encoder writes milliseconds.
	e := o.call(".broker.feed", id)
	var want struct {
		Receipts []Receipt `json:"receipts"`
	}
	_ = json.Unmarshal(o.eventsInput(e.ID, 1), &want)
	if len(want.Receipts) != len(receipts) {
		o.t.Fatal("MUTATION: procedure-oracle receipt count")
	}
	for i, r := range receipts {
		w := want.Receipts[i]
		if r.Identity != w.Identity || r.Kind != w.Kind || !r.Start.Equal(w.Start) || !r.End.Equal(w.End) || r.Uncertain != w.Uncertain || !equalReceiptResult(r.Result, w.Result) {
			o.t.Fatalf("MUTATION: procedure-oracle receipt mismatch got %s want %s", oracleJSON(r), oracleJSON(w))
		}
	}
	if err := oracleError(e); err != nil {
		return err
	}
	o.receipts = append(o.receipts, receipts...)
	return nil
}
func equalReceiptResult(a, b json.RawMessage) bool {
	var x, y map[string]json.RawMessage
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	for _, k := range []string{"start", "end"} {
		var sa, sb time.Time
		if json.Unmarshal(x[k], &sa) == nil && json.Unmarshal(y[k], &sb) == nil && sa.Equal(sb) {
			delete(x, k)
			delete(y, k)
		}
	}
	return oracleEqual(x, oracleJSON(y))
}
func (o *procedureOracle) eventsInput(id, index int) json.RawMessage {
	for _, e := range o.events {
		if e.Kind == "request" && e.ID == id {
			return e.Inputs[index]
		}
	}
	return nil
}
func (o *procedureOracle) ports() ProcedurePorts {
	return ProcedurePorts{Clock: o.clock, Broker: o,
		Sleep: func(_ context.Context, d time.Duration) error { return oracleError(o.call(".sleep", d.Milliseconds())) },
		Step: func(_ context.Context, op, slot string, end time.Duration, extra ProcessExtra) (StepResult, error) {
			o.sync()
			if o.pos >= len(o.events) {
				o.t.Fatalf("MUTATION: procedure-oracle extra step %s", op)
			}
			e := o.events[o.pos]
			args := []any{op, slot, end.Milliseconds()}
			// The frozen case made restoration's first status read unknown.
			// Unfreeze now comes first; preserve that exact unknown boundary.
			if o.source == "a20095066c1b93f6.json" && e.ID == 46 && op == "unfreeze" {
				unknown := false
				for _, event := range o.events {
					if event.ID == e.ID && event.Kind == "result" {
						unknown = code(oracleError(event)) == "command-result-unknown"
					}
				}
				if !unknown {
					o.t.Fatal("restoration ordering projection requires the frozen unknown result")
				}
				args[0] = "status"
			}
			if len(e.Inputs) > 3 {
				args = append(args, extra)
			}
			result := o.call(".step", args...)
			if err := oracleError(result); err != nil {
				return StepResult{}, err
			}
			var envelope struct {
				Started time.Time       `json:"started"`
				Ended   time.Time       `json:"ended"`
				Data    json.RawMessage `json:"data"`
			}
			if err := json.Unmarshal(result.Value, &envelope); err != nil {
				o.t.Fatal(err)
			}
			r, err := DecodeStepResult(op, envelope.Data)
			if err != nil {
				// The oracle's fake Step seam predates required ingress checks. Its
				// synthetic incomplete observations remain inputs to the algorithm; the
				// real runtime separately refuses them through DecodeStepResult.
				switch op {
				case "status":
					_ = json.Unmarshal(envelope.Data, &r.Status)
				case "drain":
					_ = json.Unmarshal(envelope.Data, &r.Drain)
				case "relay-inspect":
					_ = json.Unmarshal(envelope.Data, &r.Inspection)
				case "move":
					_ = json.Unmarshal(envelope.Data, &r.Move)
				case "worker-status":
					_ = json.Unmarshal(envelope.Data, &r.Local)
				case "store-status":
					_ = json.Unmarshal(envelope.Data, &r.Ownership)
				case "control-start":
					var ready struct {
						ReadyMS *int64 `json:"readyMs"`
					}
					_ = json.Unmarshal(envelope.Data, &ready)
					r.ReadyMS = ready.ReadyMS
				}
			}
			r.Started = envelope.Started
			r.Ended = envelope.Ended
			return r, nil
		},
		Media: func(_ context.Context) (*MediaSummary, error) {
			e := o.call(".media")
			if err := oracleError(e); err != nil {
				return nil, err
			}
			var m *MediaSummary
			_ = json.Unmarshal(e.Value, &m)
			if string(e.Value) == `{"absent":true}` {
				m = nil
			}
			return m, nil
		},
		Duplicates: func() *uint64 {
			e := o.call(".duplicates")
			var n uint64
			if json.Unmarshal(e.Value, &n) != nil {
				return nil
			}
			return &n
		},
	}
}
func procedureRoots(f OracleFixture, op string) []struct {
	req, res OracleEvent
	events   []OracleEvent
} {
	var out []struct {
		req, res OracleEvent
		events   []OracleEvent
	}
	for i, e := range f.Events {
		if e.Kind != "request" || e.Operation != op {
			continue
		}
		for j := i + 1; j < len(f.Events); j++ {
			r := f.Events[j]
			if r.Kind == "result" && r.ID == e.ID {
				out = append(out, struct {
					req, res OracleEvent
					events   []OracleEvent
				}{e, r, f.Events[i+1 : j]})
				break
			}
		}
	}
	return out
}
func newProcedureOracle(t *testing.T, req, res OracleEvent, events []OracleEvent, prefix string) *procedureOracle {
	o := &procedureOracle{t: t, root: req, result: res, events: events, prefix: prefix, clock: &FakeClock{}, callbacks: []string{prefix + ".step", prefix + ".sleep", prefix + ".broker.arm", prefix + ".broker.feed", prefix + ".media", prefix + ".duplicates"}}
	o.sync()
	return o
}
func TestProcedureOracle(t *testing.T) {
	corpus, err := loadProcedureCorpus(t)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for n := range corpus {
		if !strings.HasPrefix(n, "blobs/") && procedureFixtureSelected(n) {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	checked := 0
	for _, name := range names {
		f, err := loadProcedureFixture(corpus, name)
		if err != nil {
			t.Fatal(err)
		}
		roots := procedureRoots(f, "processes.runProcessGate")
		for index, root := range roots {
			t.Run(fmt.Sprintf("%s/%d", name, index), func(t *testing.T) {
				var gate operator.Gate
				_ = json.Unmarshal(root.req.Inputs[0], &gate)
				var config struct {
					Parameters  ProcessParameters `json:"parameters"`
					ArmedAt     int64             `json:"armedAt"`
					Trials      bool              `json:"trials"`
					RecordMedia any               `json:"recordMedia"`
				}
				_ = json.Unmarshal(root.req.Inputs[1], &config)
				var input struct {
					Broker struct {
						M struct {
							RunID string `json:"runId"`
						}
						Registration *Registration `json:"registration"`
					}
				}
				_ = json.Unmarshal(root.req.Inputs[2], &input)
				var end int64
				_ = json.Unmarshal(root.req.Inputs[3], &end)
				o := newProcedureOracle(t, root.req, root.res, root.events, "processes.runProcessGate.arg2")
				o.source = name
				o.reg = input.Broker.Registration
				o.run = input.Broker.M.RunID
				c := ProcessConfig{Parameters: config.Parameters, ArmedAt: time.UnixMilli(config.ArmedAt), Trials: config.Trials, Pool: config.RecordMedia != nil}
				// Replay the frozen algorithm window. Production-entry reserve
				// admission is covered by TestProcedureReservesUnfreezeAdmission.
				got := runProcessGate(context.Background(), gate, newProcedure(&c, o.ports(), time.Duration(end)*time.Millisecond))
				var want struct {
					Status   string          `json:"status"`
					Reason   string          `json:"reason"`
					Category string          `json:"category"`
					Process  ProcessEvidence `json:"process"`
					Media    *MediaSummary   `json:"media"`
				}
				_ = json.Unmarshal(root.res.Value, &want)
				if name == "52c749d6046baac5.json" && index == 2 {
					// Restoration also observed the known loss. Preserve failure
					// and add its new fixed restoration reason.
					want.Reason = "restore-failed"
				}
				status := string(got.Status)
				if got.AwaitingPool {
					status = "awaiting-pool"
				}
				if status != want.Status || got.Reason != want.Reason || got.Category != want.Category || !reflect.DeepEqual(got.Process, want.Process) || !reflect.DeepEqual(got.Media, want.Media) {
					t.Fatalf("MUTATION: procedure-oracle result got %s want %s", oracleJSON(got), root.res.Value)
				}
				o.sync()
				if o.pos < len(o.events) {
					t.Fatalf("MUTATION: procedure-oracle unconsumed callback %s", o.events[o.pos].Operation)
				}
			})
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("no procedure replay")
	}
	t.Logf("%d process invocations", checked)
}

func TestTrialOracle(t *testing.T) {
	corpus, err := loadProcedureCorpus(t)
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for name := range corpus {
		if strings.HasPrefix(name, "blobs/") || !procedureFixtureSelected(name) {
			continue
		}
		f, err := loadProcedureFixture(corpus, name)
		if err != nil {
			t.Fatal(err)
		}
		for _, op := range []string{"trials.trialWarmup", "trials.workerTrials"} {
			for index, root := range procedureRoots(f, op) {
				t.Run(fmt.Sprintf("%s/%s/%d", name, op, index), func(t *testing.T) {
					var input struct {
						Broker struct {
							M struct {
								RunID string `json:"runId"`
							}
							Registration    *Registration `json:"registration"`
							ReadyObservedAt *int64        `json:"readyObservedAt"`
							MediaStartedAt  *int64        `json:"mediaStartedAt"`
						}
						Parameters ProcessParameters `json:"parameters"`
						Deadline   int64             `json:"deadline"`
					}
					_ = json.Unmarshal(root.req.Inputs[0], &input)
					o := newProcedureOracle(t, root.req, root.res, root.events, op+".arg0")
					o.reg = input.Broker.Registration
					o.run = input.Broker.M.RunID
					if o.reg != nil {
						o.reg.ReadyObservedAt = input.Broker.ReadyObservedAt
						o.reg.MediaStartedAt = input.Broker.MediaStartedAt
					}
					// A shared fake can retain the original warmup callback name in later calls.
					for _, e := range root.events {
						if e.Kind == "request" && (strings.HasSuffix(e.Operation, ".step") || strings.HasSuffix(e.Operation, ".sleep") || strings.HasSuffix(e.Operation, ".broker.arm") || strings.HasSuffix(e.Operation, ".broker.feed")) {
							o.callbacks = append(o.callbacks, e.Operation)
						}
					}
					o.pos = 0
					o.sync()
					var actual error
					if op == "trials.trialWarmup" {
						actual = TrialWarmup(context.Background(), o.ports(), time.Duration(input.Deadline)*time.Millisecond)
					} else {
						var target TrialTarget
						var prefix string
						_ = json.Unmarshal(root.req.Inputs[1], &target)
						_ = json.Unmarshal(root.req.Inputs[2], &prefix)
						c := ProcessConfig{Parameters: input.Parameters, ArmedAt: o.clock.Wall()}
						actual = WorkerTrials(context.Background(), &c, o.ports(), time.Duration(input.Deadline)*time.Millisecond, target, prefix)
					}
					if code(actual) != code(oracleError(root.res)) {
						t.Fatalf("MUTATION: procedure-oracle error got %v want %v", actual, oracleError(root.res))
					}
					o.sync()
					if o.pos < len(o.events) {
						t.Fatalf("MUTATION: procedure-oracle unconsumed %s", o.events[o.pos].Operation)
					}
				})
				checked++
			}
		}
	}
	if checked == 0 {
		t.Fatal("no trial replay")
	}
	t.Logf("%d trial invocations", checked)
}
func TestReleaseOracle(t *testing.T) {
	corpus, err := loadProcedureCorpus(t)
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for name := range corpus {
		if strings.HasPrefix(name, "blobs/") || !procedureFixtureSelected(name) {
			continue
		}
		f, err := loadProcedureFixture(corpus, name)
		if err != nil {
			t.Fatal(err)
		}
		for index, root := range procedureRoots(f, "release.runReleaseGate") {
			t.Run(fmt.Sprintf("%s/%d", name, index), func(t *testing.T) {
				var config struct {
					Parameters ProcessParameters `json:"parameters"`
					ArmedAt    int64             `json:"armedAt"`
					Plan       struct {
						From struct {
							Commit string `json:"commit"`
						}
						To struct {
							Commit string `json:"commit"`
						}
					}
				}
				_ = json.Unmarshal(root.req.Inputs[0], &config)
				var input struct {
					Broker struct {
						M struct {
							RunID string `json:"runId"`
						}
						Registration *Registration `json:"registration"`
					}
				}
				_ = json.Unmarshal(root.req.Inputs[2], &input)
				var end int64
				_ = json.Unmarshal(root.req.Inputs[3], &end)
				o := newProcedureOracle(t, root.req, root.res, root.events, "release.runReleaseGate.arg2")
				o.reg = input.Broker.Registration
				o.run = input.Broker.M.RunID
				o.callbacks = append(o.callbacks, "inventory.freshCompatibilityPool")
				// Restore's step is intentionally the outer step (not the counted wrapper).
				o.callbacks = append(o.callbacks, "processes.restoreGate.arg1.step")
				p := o.ports()
				p.Inventory = func(_ context.Context, target string, end time.Duration) error {
					e := o.call("inventory.freshCompatibilityPool")
					requestEnd := o.eventsInput(e.ID, 4)
					if !oracleEqual(end.Milliseconds(), requestEnd) {
						t.Fatalf("target inventory deadline got %d want %s", end.Milliseconds(), requestEnd)
					}
					commit := config.Plan.From.Commit
					if target == "to" {
						commit = config.Plan.To.Commit
					}
					if !oracleEqual(commit, o.eventsInput(e.ID, 5)) {
						t.Fatal("target inventory pin")
					}
					return oracleError(e)
				}
				c := ProcessConfig{Parameters: config.Parameters, ArmedAt: time.UnixMilli(config.ArmedAt), Trials: false}
				got := runReleaseGate(context.Background(), newProcedure(&c, p, time.Duration(end)*time.Millisecond))
				var want struct {
					Status   string          `json:"status"`
					Reason   string          `json:"reason"`
					Category string          `json:"category"`
					Process  ProcessEvidence `json:"process"`
					Media    *MediaSummary   `json:"media"`
				}
				_ = json.Unmarshal(root.res.Value, &want)
				status := string(got.Status)
				if got.AwaitingPool {
					status = "awaiting-pool"
				}
				if status != want.Status || got.Reason != want.Reason || got.Category != want.Category || !reflect.DeepEqual(got.Process, want.Process) || !reflect.DeepEqual(got.Media, want.Media) {
					t.Fatalf("MUTATION: procedure-oracle result got %s want %s", oracleJSON(got), root.res.Value)
				}
				o.sync()
				if o.pos < len(o.events) {
					t.Fatalf("MUTATION: procedure-oracle unconsumed %s", o.events[o.pos].Operation)
				}
			})
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("no release replay")
	}
	t.Logf("%d release invocations", checked)
}

func TestWorkerEmptyOracle(t *testing.T) {
	corpus, err := loadProcedureCorpus(t)
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for name := range corpus {
		if strings.HasPrefix(name, "blobs/") || !procedureFixtureSelected(name) {
			continue
		}
		f, err := loadProcedureFixture(corpus, name)
		if err != nil {
			t.Fatal(err)
		}
		for index, root := range procedureRoots(f, "processes.workerEmpty") {
			t.Run(fmt.Sprintf("%s/%d", name, index), func(t *testing.T) {
				var worker string
				var end int64
				_ = json.Unmarshal(root.req.Inputs[1], &worker)
				_ = json.Unmarshal(root.req.Inputs[2], &end)
				o := newProcedureOracle(t, root.req, root.res, root.events, "processes.workerEmpty.arg0")
				// For nested calls sleep belongs to the original gate ports.
				for _, e := range root.events {
					if e.Kind == "request" && strings.HasSuffix(e.Operation, ".sleep") {
						o.callbacks = append(o.callbacks, e.Operation)
					}
				}
				o.pos = 0
				o.sync()
				actual := WorkerEmpty(context.Background(), o.ports(), worker, time.Duration(end)*time.Millisecond)
				if code(actual) != code(oracleError(root.res)) {
					t.Fatalf("MUTATION: empty-oracle error got %v want %v", actual, oracleError(root.res))
				}
				o.sync()
				if o.pos < len(o.events) {
					t.Fatalf("MUTATION: empty-oracle unconsumed %s", o.events[o.pos].Operation)
				}
			})
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("no empty-worker replay")
	}
	t.Logf("%d worker-empty invocations", checked)
}
func TestProcedureProjectionOracle(t *testing.T) {
	corpus, err := loadProcedureCorpus(t)
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for name := range corpus {
		if strings.HasPrefix(name, "blobs/") || !procedureFixtureSelected(name) {
			continue
		}
		f, err := loadProcedureFixture(corpus, name)
		if err != nil {
			t.Fatal(err)
		}
		for _, op := range []string{"processes.drainReceipt", "processes.sanitizeProcessEvidence", "processes.stopped", "processes.readFailure"} {
			for index, root := range procedureRoots(f, op) {
				t.Run(fmt.Sprintf("%s/%s/%d", name, op, index), func(t *testing.T) {
					switch op {
					case "processes.drainReceipt":
						var id Identity
						var result struct {
							Started time.Time `json:"started"`
							Ended   time.Time `json:"ended"`
						}
						var move *controlplane.MoveResult
						_ = json.Unmarshal(root.req.Inputs[0], &id)
						_ = json.Unmarshal(root.req.Inputs[1], &result)
						raw := root.req.Inputs[2]
						if string(raw) != `{"absent":true}` && string(raw) != "null" {
							_ = json.Unmarshal(raw, &move)
						} else {
							raw = nil
						}
						got := DrainReceipt(id, StepResult{Started: result.Started, Ended: result.Ended}, move, raw)
						var want Receipt
						_ = json.Unmarshal(root.res.Value, &want)
						if got.Identity != want.Identity || got.Kind != want.Kind || got.Uncertain != want.Uncertain || !got.Start.Equal(want.Start) || !got.End.Equal(want.End) || !oracleEqual(got.Result, want.Result) {
							t.Fatalf("MUTATION: receipt-oracle got %s want %s", oracleJSON(got), root.res.Value)
						}
					case "processes.sanitizeProcessEvidence":
						got, err := DecodeProcessEvidence(root.req.Inputs[0])
						wantErr := oracleError(root.res)
						if (err != nil) != (wantErr != nil) {
							t.Fatalf("MUTATION: process-projection got %v want %v", err, wantErr)
						}
						if err == nil && !oracleEqual(got, root.res.Value) {
							t.Fatalf("MUTATION: process-projection got %s want %s", oracleJSON(got), root.res.Value)
						}
					case "processes.stopped":
						var confirmed bool
						var r ProcedureResult
						_ = json.Unmarshal(root.req.Inputs[0], &confirmed)
						_ = json.Unmarshal(root.req.Inputs[1], &r)
						got := Stopped(confirmed, r)
						var want ProcedureResult
						_ = json.Unmarshal(root.res.Value, &want)
						if got.Status != want.Status || got.Reason != want.Reason {
							t.Fatalf("MUTATION: stopped-oracle got %s want %s", oracleJSON(got), root.res.Value)
						}
					case "processes.readFailure":
						var original struct{ Message string }
						_ = json.Unmarshal(root.req.Inputs[0], &original)
						reason, category := readFailure(fault(original.Message))
						var want struct{ Reason, Category string }
						_ = json.Unmarshal(root.res.Value, &want)
						if reason != want.Reason || category != want.Category {
							t.Fatal("MUTATION: read-failure-oracle")
						}
					}
				})
				checked++
			}
		}
	}
	if checked == 0 {
		t.Fatal("no procedure projection replay")
	}
	t.Logf("%d projection invocations", checked)
}

type oracleStore struct{ o *procedureOracle }

func (s oracleStore) Observe(context.Context, time.Duration) (StoreObservation, error) {
	e := s.o.call(".store.read")
	var r StoreObservation
	_ = json.Unmarshal(e.Value, &r)
	return r, oracleError(e)
}
func (s oracleStore) Failover(context.Context, time.Duration) error {
	return oracleError(s.o.call(".store.failover"))
}
func TestMeasurementDefinitionOracle(t *testing.T) {
	corpus, err := loadProcedureCorpus(t)
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for name := range corpus {
		if strings.HasPrefix(name, "blobs/") || !procedureFixtureSelected(name) {
			continue
		}
		f, err := loadProcedureFixture(corpus, name)
		if err != nil {
			t.Fatal(err)
		}
		for index, root := range procedureRoots(f, "measure.runMeasureGate") {
			t.Run(fmt.Sprintf("%s/%d", name, index), func(t *testing.T) {
				var gate operator.Gate
				_ = json.Unmarshal(root.req.Inputs[0], &gate)
				var config struct {
					Parameters ProcessParameters `json:"parameters"`
					ArmedAt    int64             `json:"armedAt"`
				}
				_ = json.Unmarshal(root.req.Inputs[1], &config)
				var input struct {
					Broker struct {
						M struct {
							RunID string `json:"runId"`
							Plan  struct {
								Calls int `json:"calls"`
							}
						}
						Registration *Registration `json:"registration"`
					}
					Store json.RawMessage `json:"store"`
				}
				_ = json.Unmarshal(root.req.Inputs[2], &input)
				var end int64
				_ = json.Unmarshal(root.req.Inputs[3], &end)
				// Replay the P3-owned procedure up to collection. Raw caller reductions,
				// telemetry and judge assembly have their own evidence-package owner.
				stop := len(root.events)
				for i, e := range root.events {
					if e.Kind == "request" && strings.HasSuffix(e.Operation, ".collect") {
						stop = i
						break
					}
				}
				events := root.events[:stop]
				o := newProcedureOracle(t, root.req, root.res, events, "measure.runMeasureGate.arg2")
				o.reg = input.Broker.Registration
				o.run = input.Broker.M.RunID
				for _, suffix := range []string{".observe", ".store.read", ".store.failover", ".beforeFault", ".broker.markFault"} {
					o.callbacks = append(o.callbacks, "measure.runMeasureGate.arg2"+suffix)
				}
				o.pos = 0
				o.sync()
				p := MeasurePorts{ProcedurePorts: o.ports(), Calls: input.Broker.M.Plan.Calls}
				if len(input.Store) > 0 && string(input.Store) != "null" {
					p.Store = oracleStore{o}
				}
				p.BeforeFault = func(context.Context, time.Duration) error { return oracleError(o.call(".beforeFault")) }
				p.MarkFault = func(context.Context) error { return oracleError(o.call(".broker.markFault")) }
				observations := 0
				observe := func() (*MeasurementObservation, error) {
					if o.clock.Elapsed() >= time.Duration(end)*time.Millisecond {
						return nil, fault("gate-deadline")
					}
					e := o.call(".observe")
					if err := oracleError(e); err != nil {
						return nil, err
					}
					observations++
					var row struct {
						At     int64                `json:"at"`
						Status *controlplane.Status `json:"status"`
						Hosts  map[string]struct {
							Local mediaworker.WorkerStatus `json:"local"`
						} `json:"hosts"`
					}
					_ = json.Unmarshal(e.Value, &row)
					r := &MeasurementObservation{At: time.UnixMilli(row.At), Status: row.Status, Local: map[string]mediaworker.WorkerStatus{}}
					for w, h := range row.Hosts {
						r.Local[w] = h.Local
					}
					return r, nil
				}
				c := ProcessConfig{Parameters: config.Parameters, ArmedAt: time.UnixMilli(config.ArmedAt)}
				g := newProcedure(&c, p.ProcedurePorts, time.Duration(end)*time.Millisecond)
				measurement := Measurement{}
				actual := g.measureDefinition(context.Background(), gate, p, observe, &measurement)
				// An early definite rejection is represented by an inconclusive outer gate;
				// compare all consumed effects and the produced admission/recovery evidence.
				var want struct {
					Status      string      `json:"status"`
					Reason      string      `json:"reason"`
					Measurement Measurement `json:"measurement"`
				}
				_ = json.Unmarshal(root.res.Value, &want)
				if (code(actual) == "command-result-unknown" || code(actual) == "gate-deadline") && want.Reason != code(actual) {
					t.Fatalf("MUTATION: measure-definition reason got %v want %s", actual, root.res.Value)
				}
				if !reflect.DeepEqual(measurement.AdmittedCalls, want.Measurement.AdmittedCalls) || !reflect.DeepEqual(measurement.RecoveredMS, want.Measurement.RecoveredMS) || !reflect.DeepEqual(measurement.PrimaryChanged, want.Measurement.PrimaryChanged) || !reflect.DeepEqual(measurement.SynchronousDurability, want.Measurement.SynchronousDurability) {
					t.Fatalf("MUTATION: measure-definition evidence got %s want %s", oracleJSON(measurement), root.res.Value)
				}
				o.sync()
				if o.pos < len(o.events) {
					t.Fatalf("MUTATION: measure-definition unconsumed %s after %v", o.events[o.pos].Operation, actual)
				}
				_ = observations
			})
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("no measurement definition replay")
	}
	t.Logf("%d measurement procedure invocations", checked)
}

var procedureCorpusOnce sync.Once
var procedureCorpus Corpus
var procedureCorpusError error
var procedureFixtures sync.Map

func loadProcedureCorpus(t *testing.T) (Corpus, error) {
	t.Helper()
	procedureCorpusOnce.Do(func() { procedureCorpus, procedureCorpusError = LoadCorpus("testdata/transcripts") })
	return procedureCorpus, procedureCorpusError
}
func loadProcedureFixture(c Corpus, name string) (OracleFixture, error) {
	if f, ok := procedureFixtures.Load(name); ok {
		return f.(OracleFixture), nil
	}
	f, err := c.Fixture(name)
	if err == nil {
		procedureFixtures.Store(name, f)
	}
	return f, err
}

func TestBoundaryOracle(t *testing.T) {
	corpus, err := loadProcedureCorpus(t)
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for name := range corpus {
		if strings.HasPrefix(name, "blobs/") || !procedureFixtureSelected(name) {
			continue
		}
		f, err := loadProcedureFixture(corpus, name)
		if err != nil {
			t.Fatal(err)
		}
		for index, root := range procedureRoots(f, "measure.waitBoundaryIdle") {
			t.Run(fmt.Sprintf("%s/%d", name, index), func(t *testing.T) {
				var end int64
				_ = json.Unmarshal(root.req.Inputs[1], &end)
				o := newProcedureOracle(t, root.req, root.res, root.events, "measure.waitBoundaryIdle.arg0")
				actual := WaitBoundaryIdle(context.Background(), o.ports(), time.Duration(end)*time.Millisecond)
				if code(actual) != code(oracleError(root.res)) {
					t.Fatalf("MUTATION: boundary-oracle error got %v want %v", actual, oracleError(root.res))
				}
				o.sync()
				if o.pos < len(o.events) {
					t.Fatalf("MUTATION: boundary-oracle unconsumed %s", o.events[o.pos].Operation)
				}
			})
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("no boundary replay")
	}
	t.Logf("%d boundary invocations", checked)
}

func TestPreconditionOracle(t *testing.T) {
	corpus, err := loadProcedureCorpus(t)
	if err != nil {
		t.Fatal(err)
	}
	checked, deltas, expectedDeltas := 0, 0, 0
	for name := range corpus {
		if strings.HasPrefix(name, "blobs/") || !procedureFixtureSelected(name) {
			continue
		}
		f, err := loadProcedureFixture(corpus, name)
		if err != nil {
			t.Fatal(err)
		}
		for index, root := range procedureRoots(f, "processes.gatePrecondition") {
			t.Run(fmt.Sprintf("%s/%d", name, index), func(t *testing.T) {
				if name == "0151dac9ba93d068.json" && index == 2 {
					expectedDeltas++
				}
				var config struct {
					Parameters ProcessParameters `json:"parameters"`
					ArmedAt    int64             `json:"armedAt"`
				}
				_ = json.Unmarshal(root.req.Inputs[0], &config)
				var end int64
				_ = json.Unmarshal(root.req.Inputs[2], &end)
				o := newProcedureOracle(t, root.req, root.res, root.events, "processes.gatePrecondition.arg1")
				o.callbacks = append(o.callbacks, "processes.ProcessRuntime.step", "inventory.freshCompatibilityPool")
				o.pos = 0
				o.sync()
				p := o.ports()
				p.Inventory = func(context.Context, string, time.Duration) error {
					return oracleError(o.call(".freshCompatibilityPool"))
				}
				c := ProcessConfig{Parameters: config.Parameters, ArmedAt: time.UnixMilli(config.ArmedAt)}
				actual := GatePrecondition(context.Background(), &c, p, time.Duration(end)*time.Millisecond)
				if code(actual) == "command-result-unknown" && code(oracleError(root.res)) != "command-result-unknown" {
					if !preconditionUnknownDelta(name, index, o) {
						t.Fatal("MUTATION: precondition-oracle unauthorized unknown delta or stopping point")
					}
					deltas++
					t.Logf("unknown-status delta %s/%d", name, index)
					return
				}
				if code(actual) != code(oracleError(root.res)) {
					t.Fatalf("MUTATION: precondition-oracle error got %v want %v", actual, oracleError(root.res))
				}
				o.sync()
				if o.pos < len(o.events) {
					t.Fatalf("MUTATION: precondition-oracle unconsumed %s", o.events[o.pos].Operation)
				}
			})
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("no precondition replay")
	}
	if !strings.Contains(flag.Lookup("test.run").Value.String(), "/") && expectedDeltas != 1 {
		t.Fatalf("MUTATION: precondition-oracle must exercise exactly one approved delta, got %d", expectedDeltas)
	}
	if deltas != expectedDeltas {
		t.Fatalf("MUTATION: precondition-oracle delta count got %d want %d", deltas, expectedDeltas)
	}
	t.Logf("%d precondition invocations, %d explicit unknown-status deltas", checked, deltas)
}

// The manifest permits only this invocation to stop after the unknown status.
// Pin both the consumed result and every remaining callback, including inventory
// nested inside the old control-start. No other unknown-result mismatch is waived.
func preconditionUnknownDelta(name string, index int, o *procedureOracle) bool {
	if name != "0151dac9ba93d068.json" || index != 2 {
		return false
	}
	o.sync()
	if o.pos >= len(o.events) || o.events[o.pos].ID != 467 {
		return false
	}
	unknownStatus := false
	for _, e := range o.events[:o.pos] {
		if e.ID == 453 && e.Kind == "result" && e.Operation == "processes.ProcessRuntime.step" && code(oracleError(e)) == "command-result-unknown" {
			unknownStatus = true
		}
	}
	var remaining []string
	for _, e := range o.events[o.pos:] {
		if !o.relevant(e) {
			continue
		}
		op := ""
		if strings.HasSuffix(e.Operation, ".step") && len(e.Inputs) > 0 {
			_ = json.Unmarshal(e.Inputs[0], &op)
		}
		remaining = append(remaining, fmt.Sprintf("%d:%s:%s", e.ID, e.Operation, op))
	}
	return unknownStatus && reflect.DeepEqual(remaining, []string{
		"467:processes.ProcessRuntime.step:control-start",
		"468:inventory.freshCompatibilityPool:",
		"539:inventory.freshCompatibilityPool:",
		"596:processes.ProcessRuntime.step:status",
	})
}

func TestPreconditionDeltaRejectsUnrelatedUnknown(t *testing.T) {
	corpus, err := loadProcedureCorpus(t)
	if err != nil {
		t.Fatal(err)
	}
	f, err := loadProcedureFixture(corpus, "0151dac9ba93d068.json")
	if err != nil {
		t.Fatal(err)
	}
	root := procedureRoots(f, "processes.gatePrecondition")[2]
	o := newProcedureOracle(t, root.req, root.res, root.events, "processes.gatePrecondition.arg1")
	o.callbacks = append(o.callbacks, "processes.ProcessRuntime.step", "inventory.freshCompatibilityPool")
	for i, e := range o.events {
		if e.Kind == "request" && e.ID == 467 {
			o.pos = i
			break
		}
	}
	if !preconditionUnknownDelta("0151dac9ba93d068.json", 2, o) {
		t.Fatal("approved delta must match its exact stopping point")
	}
	if preconditionUnknownDelta("unrelated.json", 2, o) || preconditionUnknownDelta("0151dac9ba93d068.json", 1, o) {
		t.Fatal("MUTATION: unrelated unknown must not receive the delta exemption")
	}
	// A different remaining callback must fail even at the approved position.
	modified := *o
	modified.events = append([]OracleEvent(nil), o.events...)
	modified.events[o.pos].Inputs = []json.RawMessage{json.RawMessage(`"replace"`)}
	if preconditionUnknownDelta("0151dac9ba93d068.json", 2, &modified) {
		t.Fatal("MUTATION: changed remaining callback must not receive the delta exemption")
	}
	o.pos++
	if preconditionUnknownDelta("0151dac9ba93d068.json", 2, o) {
		t.Fatal("wrong stopping point must not receive the delta exemption")
	}
}

// A named mutation subtest need only decode its selected frozen member. This
// preserves go test's filter while avoiding unrelated multi-megabyte fixtures.
func procedureFixtureSelected(name string) bool {
	filter := flag.Lookup("test.run").Value.String()
	return !strings.Contains(filter, ".json") || strings.Contains(filter, name)
}

func TestProcedureCoverageInventory(t *testing.T) {
	operations := map[string]string{"processes.runProcessGate": "TestProcedureOracle", "release.runReleaseGate": "TestReleaseOracle", "trials.trialWarmup": "TestTrialOracle", "trials.workerTrials": "TestTrialOracle", "processes.workerEmpty": "TestWorkerEmptyOracle", "processes.drainReceipt": "TestProcedureProjectionOracle", "processes.sanitizeProcessEvidence": "TestProcedureProjectionOracle", "processes.stopped": "TestProcedureProjectionOracle", "processes.readFailure": "TestProcedureProjectionOracle", "measure.runMeasureGate": "TestMeasurementDefinitionOracle", "measure.waitBoundaryIdle": "TestBoundaryOracle", "processes.gatePrecondition": "TestPreconditionOracle"}
	corpus, err := loadProcedureCorpus(t)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]map[string]int{}
	total := 0
	for name := range corpus {
		if strings.HasPrefix(name, "blobs/") {
			continue
		}
		f, err := loadProcedureFixture(corpus, name)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range f.Events {
			if e.Kind == "request" && operations[e.Operation] != "" {
				if counts[name] == nil {
					counts[name] = map[string]int{}
				}
				counts[name][e.Operation]++
				total++
			}
		}
	}
	data, err := os.ReadFile("testdata/coverage.json")
	if err != nil {
		t.Fatal(err)
	}
	var coverage struct {
		ProcedureReplay struct {
			Scenarios, FunctionInvocations, FullFixtures int
			Owner, Evidence                              string
		} `json:"procedureReplay"`
		Fixtures []struct {
			Fixture, FullReplay string
			Projections         []struct {
				Operation, Owner, Evidence, Test string
				Invocations                      int
			} `json:"procedureProjections"`
		} `json:"fixtures"`
	}
	if err = json.Unmarshal(data, &coverage); err != nil {
		t.Fatal(err)
	}
	if coverage.ProcedureReplay.Owner != "P3" || coverage.ProcedureReplay.Evidence != "replayed" || coverage.ProcedureReplay.Scenarios != len(counts) || coverage.ProcedureReplay.FunctionInvocations != total {
		t.Fatal("procedure replay count differs")
	}
	full := 0
	for _, f := range coverage.Fixtures {
		if f.FullReplay == "replayed" && len(f.Projections) > 0 {
			full++
		}
		for _, p := range f.Projections {
			if p.Owner != "P3" || p.Evidence != "replayed" || p.Test != "operatorgates."+operations[p.Operation] || counts[f.Fixture][p.Operation] != p.Invocations {
				t.Fatal("invalid procedure projection", f.Fixture, p.Operation)
			}
			delete(counts[f.Fixture], p.Operation)
		}
		if len(counts[f.Fixture]) == 0 {
			delete(counts, f.Fixture)
		}
	}
	if len(counts) != 0 || full != coverage.ProcedureReplay.FullFixtures {
		t.Fatal("procedure replay inventory incomplete")
	}
}
