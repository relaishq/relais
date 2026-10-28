package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/loadgen"
	"github.com/relaishq/relais/internal/operatorcaller"
	"github.com/relaishq/relais/internal/operatorevidence"
	"github.com/relaishq/relais/internal/operatorgates"
	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/relaishq/relais/internal/operatorgates/observe"
	"github.com/relaishq/relais/internal/releasecompat"
	"github.com/relaishq/relais/pkg/callharness"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/operator"
)

type composedClock struct{ at time.Time }

func (c composedClock) Now() time.Time { return c.at }
func (c composedClock) Sleep(context.Context, time.Duration) error {
	return fmt.Errorf("unexpected caller wait")
}

type composedFence struct{ latched bool }

func (f *composedFence) Latched() bool { return f.latched }
func (f *composedFence) Halt(string)   { f.latched = true }

type composedJudge struct{}

type retentionRequired struct{}

func checkRetention(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if ctx.Value(retentionRequired{}) == true {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > 31*time.Second {
			return fmt.Errorf("unbounded evidence retention")
		}
	}
	return nil
}

func (composedJudge) Judge(ctx context.Context, pool operatorevidence.Pool) (operatorevidence.Summary, error) {
	if err := checkRetention(ctx); err != nil {
		return operatorevidence.Summary{}, err
	}
	raw, _ := json.Marshal(pool)
	input, err := decodePool(raw)
	if err != nil {
		return operatorevidence.Summary{}, err
	}
	raw, _ = json.Marshal(pooledPreviewMedia(&input))
	if err := ctx.Err(); err != nil {
		return operatorevidence.Summary{}, err
	}
	return operatorevidence.DecodeSummary(raw)
}

type composedBox struct {
	t         *testing.T
	values    map[string][]byte
	manifest  operatorcaller.Manifest
	sessions  []string
	clock     composedClock
	reads     int
	delivered int
	started   chan struct{}
}

func (b *composedBox) Read(ctx context.Context, key string) (operatorcaller.ReadResult, error) {
	if err := ctx.Err(); err != nil {
		return operatorcaller.ReadResult{}, err
	}
	if b.started != nil && strings.HasSuffix(key, "/finished.json") {
		close(b.started)
		<-ctx.Done()
		return operatorcaller.ReadResult{}, ctx.Err()
	}
	if strings.HasSuffix(key, "/caller.jsonl") {
		if err := checkRetention(ctx); err != nil {
			return operatorcaller.ReadResult{}, err
		}
		b.reads++
	}
	v, ok := b.values[key]
	return operatorcaller.ReadResult{Found: ok, Data: v}, nil
}
func (b *composedBox) Create(ctx context.Context, key string, raw []byte) operatorcaller.CreateState {
	if _, exists := b.values[key]; exists {
		return operatorcaller.Conflict
	}
	var feed operatorcaller.Feed
	if err := json.Unmarshal(raw, &feed); err != nil {
		b.t.Fatal(err)
	}
	ack, err := operatorcaller.Deliver(ctx, feed, b.sessions, func(_ context.Context, path string, in, out any) error {
		switch path {
		case "/clock-probes":
			*out.(*json.RawMessage) = json.RawMessage(`[]`)
		case "/record-events":
			b.delivered += len(in.([]operatorcaller.Receipt))
		}
		return nil
	}, func() { b.t.Fatal("unexpected bridge hangup") }, b.clock)
	if err != nil {
		b.t.Fatal(err)
	}
	b.values[key] = append([]byte{}, raw...)
	b.values[fmt.Sprintf("%s/acks/%05d.json", b.manifest.Job(), feed.Sequence)], _ = json.Marshal(ack)
	return operatorcaller.Committed
}

func TestComposedProcedureCallerPool(t *testing.T) {
	t.Parallel()
	scenarios := []string{"complete", "missing-clock", "missing-report", "uncertain-receipt", "stopped", "nonzero", "incomplete", "deadline", "cancel"}
	for _, field := range []string{"checkpoint_stored_at", "checkpoint", "result"} {
		for _, mode := range []string{"delete", "null"} {
			name := "receipt-" + field + "-" + mode
			scenarios = append(scenarios, name, name+"-loss")
		}
	}
	for _, scenario := range scenarios {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			receiptIncomplete := strings.HasPrefix(scenario, "receipt-")
			failedCompletion := scenario == "stopped" || scenario == "nonzero" || scenario == "incomplete"
			exitCollection := scenario == "deadline" || scenario == "cancel"
			failure := !receiptIncomplete && scenario != "complete" || strings.HasSuffix(scenario, "-loss")
			input := rawPoolSample()
			// Synthetic population IDs predate the production broker grammar.
			// Pad them consistently at this test boundary; all media stays identical.
			for i := range input.Phases {
				for n := range 30 {
					old, next := fmt.Sprint("event-", n), fmt.Sprintf("event-%04d", n)
					input.Phases[i].CallerJSONL = strings.ReplaceAll(input.Phases[i].CallerJSONL, `"`+old+`"`, `"`+next+`"`)
					for j := range input.Phases[i].Receipts {
						if input.Phases[i].Receipts[j].Identity.OperationID == old {
							input.Phases[i].Receipts[j].Identity.OperationID = next
						}
					}
				}
			}
			if scenario == "missing-clock" || failedCompletion || exitCollection {
				replaceCall(t, &input, func(c *callharness.Report) { c.DecryptionFailures.AuthTag = 1 })
			}
			if scenario == "missing-report" || strings.HasSuffix(scenario, "-loss") {
				input.Phases[0].Receipts[0].Result.Lost = true
			}
			pool := &operatorgates.EvidenceRuntime{RunID: input.RunID, Judge: composedJudge{}}
			for index, phase := range input.Phases {
				base := phase.Probes[0].Send
				at := base.Add(123 * time.Second)
				identity := operatorcaller.Identity{RunID: input.RunID, Gate: operator.Load, Phase: operatorcaller.Survivor, CallerID: fmt.Sprint(index + 1), Attempt: "1", PublicSHA: strings.Repeat("a", 40)}
				manifest := operatorcaller.Manifest{Version: 1, Identity: identity, ArmedAt: base.UnixMilli(), CreatedAt: base.UnixMilli(), Deadline: base.Add(20 * time.Minute).UnixMilli(), Plan: operatorcaller.NewPlan(identity.Phase, 120, identity.PublicSHA), Endpoints: operatorcaller.Endpoints{RunID: input.RunID, Signaling: "https://signal.test/calls", Relay: "192.0.2.1:24700", FrontendControlBoundNS: 1, FrontendWorkerBoundNS: 1, DriftPPM: 100}}
				clock := composedClock{at}
				box := &composedBox{t: t, manifest: manifest, sessions: phase.Sessions, clock: clock, values: map[string][]byte{}}
				ctx, cancel := context.WithCancel(context.WithValue(context.Background(), retentionRequired{}, exitCollection && index == 0))
				defer cancel()
				if exitCollection && index == 0 {
					box.started = make(chan struct{})
				}
				broker, err := operatorcaller.NewBroker(manifest, operatorcaller.NewMailbox(box), clock, &composedFence{})
				if err != nil {
					t.Fatal(err)
				}
				var probes []operatorcaller.Probe
				for _, p := range phase.Probes {
					probes = append(probes, operatorcaller.Probe{RTTNS: p.RTT, CallerSend: p.Send, CallerReceive: p.Receive, ServerReceive: p.ServerReceive, ServerSend: p.ServerSend})
				}
				burst := operatorcaller.Burst{StartedAt: probes[0].CallerSend, FinishedAt: probes[len(probes)-1].CallerReceive, Probes: probes}
				host := operatorcaller.Host{SchemaVersion: 1, AvailableCPUs: 2, LogicalCPUs: 2, CPUModel: "synthetic", CallerGOMAXPROCS: 2, CapturedAt: base}
				reg, err := operatorcaller.Register(manifest.Plan, input.RunID, phase.Sessions, burst, host, base.Add(time.Second), nil)
				if err != nil {
					t.Fatal(err)
				}
				broker.Registration = &reg
				fault := base.Add(39 * time.Second)
				broker.FirstFaultAt = &fault
				metadata := operatorevidence.Phase{DeployedSHA: identity.PublicSHA, Clocks: clocks.Evidence{At: base}, Inventory: releasecompat.Pool{Complete: true}}
				if scenario == "missing-clock" {
					metadata.Clocks = clocks.Evidence{}
				}
				procedureClock := &operatorgates.FakeClock{Now: at}
				adapter := operatorgates.NewCallerRuntime(broker, procedureClock, 10*time.Minute, pool, metadata)
				for receiptIndex, receipt := range phase.Receipts {
					raw, _ := json.Marshal(receipt.Result)
					if receiptIncomplete && index == 0 && receiptIndex == 0 {
						parts := strings.Split(scenario, "-")
						var object map[string]any
						if err := json.Unmarshal(raw, &object); err != nil {
							t.Fatal(err)
						}
						if _, exists := object[parts[1]]; !exists {
							t.Fatal("missing mutation target")
						}
						if parts[2] == "delete" {
							delete(object, parts[1])
						} else {
							object[parts[1]] = nil
						}
						raw, _ = json.Marshal(object)
					}
					if scenario == "uncertain-receipt" && index == 0 && receiptIndex == 0 {
						receipt.Uncertain = true
						raw = []byte(`{"lost":true}`)
					}
					if err := adapter.Arm(context.Background(), receipt.Identity.OperationID, []operatorgates.Identity{receipt.Identity}); err != nil {
						t.Fatal(err)
					}
					if err := adapter.Feed(context.Background(), receipt.Identity.OperationID, []operatorgates.Receipt{{Identity: receipt.Identity, Kind: receipt.Kind, Start: receipt.Start, End: receipt.End, Result: raw, Uncertain: receipt.Uncertain}}); err != nil {
						t.Fatal("MUTATION: composed-caller-uncertain", err)
					}
				}
				completion := map[string]any{"runId": input.RunID, "job": "caller-1", "done": true, "stopped": false, "exitCode": 0, "sessions": phase.Sessions}
				if failedCompletion && index == 0 {
					switch scenario {
					case "stopped":
						completion["stopped"] = true
					case "nonzero":
						completion["exitCode"] = 1
					case "incomplete":
						completion["done"] = false
					}
				}
				box.values[manifest.Job()+"/finished.json"], _ = json.Marshal(completion)
				if scenario != "missing-report" || index != 0 {
					box.values[manifest.Job()+"/caller.jsonl"] = []byte(phase.CallerJSONL)
				}
				worker1, worker2 := netip.MustParseAddrPort("192.0.2.1:24711"), netip.MustParseAddrPort("192.0.2.2:24711")
				config := operatorgates.ProcessConfig{ArmedAt: base, Parameters: operatorgates.ProcessParameters{Workers: map[string]operatorgates.WorkerEndpoint{"w1": {Media: worker1}, "w2": {Media: worker2}}}}
				ports := operatorgates.MeasurePorts{ProcedurePorts: operatorgates.ProcedurePorts{Clock: procedureClock, Broker: adapter, Sleep: func(ctx context.Context, _ time.Duration) error {
					if exitCollection && index == 0 {
						<-box.started
						if scenario == "deadline" {
							procedureClock.Advance(10 * time.Minute)
						} else {
							cancel()
						}
						return context.Canceled
					}
					select {
					case <-time.After(time.Millisecond):
					case <-ctx.Done():
						return ctx.Err()
					}
					return nil
				}}, Calls: 1, Collect: adapter.Collect, Observe: func(context.Context, time.Duration) (operatorgates.MeasurementObservation, error) {
					return operatorgates.MeasurementObservation{At: at, Status: &controlplane.Status{Ready: true, Pool: controlplane.PoolSample{Cap: 2}, Calls: []controlplane.CallStatus{{ID: phase.Sessions[0], Owner: "w1", Epoch: 1}}, Workers: []controlplane.WorkerStatus{{Name: "w1", Address: worker1}, {Name: "w2", Address: worker2}}}}, nil
				}}
				result := operatorgates.RunMeasureGate(ctx, operator.Load, &config, ports, 10*time.Minute)
				want := operator.Inconclusive
				if failure {
					want = operator.Failure
				}
				if result.Status != want || result.Media == nil {
					t.Fatal("MUTATION: composed-procedure-pool", result)
				}
				if index == 1 && scenario == "complete" && (result.Media.Status != operator.Passed || *result.Media.QualifiedMoves != 20 || *result.Media.QualifiedTakeovers != 10) {
					t.Fatal("MUTATION: composed-pool-qualification", result.Media)
				}
				if receiptIncomplete && !failure && result.Media.Status != operator.Inconclusive {
					t.Fatal("MUTATION: composed-receipt-incomplete", result.Media)
				}
				if failure && result.Media.Status != operator.Failure {
					t.Fatal("MUTATION: composed-pool-failure", result.Media)
				}
				// RunMeasureGate must already have joined publication: a final pool
				// snapshot sees this phase, even on deadline/cancellation exits.
				if exitCollection && index == 0 {
					final, err := pool.Media(context.Background())
					if err != nil || final == nil || final.Status != operator.Failure {
						t.Fatal("MUTATION: collection-ownership-joined", final, err)
					}
				}
				_, collectErr := adapter.Media(context.Background())
				if (collectErr != nil) != ((scenario == "missing-report" || failedCompletion || exitCollection) && index == 0) {
					t.Fatal("MUTATION: composed-collection-error", collectErr)
				}

				if box.reads != 1 || box.delivered != len(phase.Receipts) {
					t.Fatal("MUTATION: composed-collection-once", box.reads, box.delivered)
				}
			}
		})
	}
}

func TestCorrectedFrozenCallerRows(t *testing.T) {
	t.Parallel()
	corpus, err := operatorgates.LoadCorpus("../../operatorgates/testdata/transcripts")
	if err != nil {
		t.Fatal(err)
	}
	streams, rows, restored, sentinels := 0, 0, 0, 0
	for name := range corpus {
		if strings.Contains(name, "/") {
			continue
		}
		// Expand only the callback values owned by this test. Resolving every
		// unrelated host inventory/transport input dominates race-test time.
		var fixture struct {
			SchemaVersion      int    `json:"schemaVersion"`
			ExpectedTestResult string `json:"expectedTestResult"`
			Events             []struct {
				Kind       string          `json:"kind"`
				Operation  string          `json:"operation"`
				Completion string          `json:"completion"`
				Value      json.RawMessage `json:"value"`
			} `json:"events"`
		}
		if err := json.Unmarshal(corpus[name], &fixture); err != nil {
			t.Fatal(err)
		}
		if fixture.SchemaVersion != 1 || fixture.ExpectedTestResult != "passed" {
			t.Fatal("oracle fixture schema")
		}
		for _, event := range fixture.Events {
			if event.Kind != "result" || event.Operation != "measure.runMeasureGate.arg2.collect" || event.Completion != "returned" {
				continue
			}
			raw, err := corpus.Resolve(event.Value)
			if err != nil {
				t.Fatal(err)
			}
			var stream string
			if err := json.Unmarshal(raw, &stream); err != nil {
				t.Fatal(err)
			}
			streams++
			if strings.TrimSpace(stream) == "" {
				continue
			}
			for _, line := range strings.Split(strings.TrimSpace(stream), "\n") {
				var message loadgen.Message
				if err := observe.DecodeLimit([]byte(line), &message, nil, 64<<20); err != nil {
					if name != "0ad44702da6d6aa6.json" || line != "malformed" {
						t.Fatal("MUTATION: frozen-caller-json", name, err)
					}
					sentinels++
					continue
				}
				rows++
				if message.Call != nil && message.Call.HungUpAt == 181*time.Second {
					restored++
				}
			}
		}
	}
	if streams != 22 || rows != 38 || restored != 20 || sentinels != 1 {
		t.Fatal("MUTATION: frozen-caller-rows", streams, rows, restored, sentinels)
	}
}
