package operatorgates

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/operatorcaller"
	"github.com/relaishq/relais/internal/operatorevidence"
	"github.com/relaishq/relais/internal/operatorgates/observe"
	"github.com/relaishq/relais/pkg/operator"
)

type oracleCallerClock struct{ clock Clock }

func (c oracleCallerClock) Now() time.Time { return c.clock.Wall() }
func (c oracleCallerClock) Sleep(context.Context, time.Duration) error {
	return fmt.Errorf("unexpected caller wait")
}

type oracleCallerFence struct{ latched bool }

func (f *oracleCallerFence) Latched() bool { return f.latched }
func (f *oracleCallerFence) Halt(string)   { f.latched = true }

type oracleCallerBox struct {
	t                     *testing.T
	oracle                *procedureOracle
	manifest              operatorcaller.Manifest
	values                map[string][]byte
	feeds, arms, receipts int
}

func (b *oracleCallerBox) Read(_ context.Context, key string) (operatorcaller.ReadResult, error) {
	v, ok := b.values[key]
	return operatorcaller.ReadResult{Found: ok, Data: v}, nil
}
func (b *oracleCallerBox) Create(ctx context.Context, key string, data []byte) operatorcaller.CreateState {
	if _, exists := b.values[key]; exists {
		return operatorcaller.Conflict
	}
	var feed operatorcaller.Feed
	if err := json.Unmarshal(data, &feed); err != nil {
		b.t.Fatal(err)
	}
	b.oracle.sync()
	request := b.oracle.events[b.oracle.pos]
	var operation string
	_ = json.Unmarshal(request.Inputs[0], &operation)
	if operation != feed.OperationID {
		b.t.Fatal("MUTATION: composed-caller-operation", operation, feed.OperationID)
	}
	suffix := ".broker.arm"
	switch feed.Payload.Kind {
	case "arm":
		var ids []Identity
		_ = json.Unmarshal(request.Inputs[1], &ids)
		if !reflect.DeepEqual(ids, feed.Payload.Identities) {
			b.t.Fatal("MUTATION: composed-caller-identities")
		}
	case "receipt":
		suffix = ".broker.feed"
		var original struct {
			Receipts []Receipt `json:"receipts"`
		}
		if err := json.Unmarshal(request.Inputs[1], &original); err != nil {
			b.t.Fatal(err)
		}
		var payload struct{ Receipts []operatorcaller.Receipt }
		for _, receipt := range original.Receipts {
			converted := operatorcaller.Receipt{Identity: receipt.Identity, Kind: receipt.Kind, Start: receipt.Start, End: receipt.End, Uncertain: receipt.Uncertain}
			// Historical callback fixtures supplied partial receipts. The composed
			// boundary now deliberately quarantines them rather than laundering
			// missing required members into zero-filled complete evidence.
			err := observe.DecodeEvidenceLimit(receipt.Result, &converted.Result, 256000)
			if observe.IsIncompleteEvidence(err) {
				converted.Uncertain = true
			} else if err != nil {
				b.t.Fatal(err)
			}
			payload.Receipts = append(payload.Receipts, converted)
		}
		if !reflect.DeepEqual(payload.Receipts, feed.Payload.Receipts) {
			b.t.Fatalf("MUTATION: composed-caller-receipts got %s want %s", oracleJSON(feed.Payload.Receipts), request.Inputs[1])
		}
	default:
		b.t.Fatal("unexpected feed", feed.Payload.Kind)
	}
	result := b.oracle.call(suffix, operation)
	if result.Completion == "threw" {
		return operatorcaller.CreateUnknown
	}
	local := func(_ context.Context, path string, input, output any) error {
		switch path {
		case "/arm-event":
			b.arms++
		case "/record-events":
			b.receipts++
		case "/clock-probes":
			*output.(*json.RawMessage) = json.RawMessage(`[]`)
		default:
			b.t.Fatal("unexpected child API", path)
		}
		return nil
	}
	ack, err := operatorcaller.Deliver(ctx, feed, []string{"original"}, local, func() { b.t.Fatal("unexpected hangup") }, oracleCallerClock{b.oracle.clock})
	if err != nil {
		b.t.Fatal(err)
	}
	b.feeds++
	b.values[key] = append([]byte{}, data...)
	b.values[fmt.Sprintf("%s/acks/%05d.json", b.manifest.Job(), feed.Sequence)] = oracleJSON(ack)
	return operatorcaller.Committed
}

// The frozen host callbacks stay injected. Procedure arm/feed calls now cross
// the actual broker, create-only mailbox and bridge delivery code, rather than
// returning directly from a broker stub. Child HTTP remains the offline seam.
func TestComposedTrialOracle(t *testing.T) {
	corpus, err := loadProcedureCorpus(t)
	if err != nil {
		t.Fatal(err)
	}
	for name := range corpus {
		if strings.Contains(name, "/") {
			continue
		}
		fixture, err := loadProcedureFixture(corpus, name)
		if err != nil {
			t.Fatal(err)
		}
		for _, op := range []string{"trials.trialWarmup", "trials.workerTrials"} {
			for _, root := range procedureRoots(fixture, op) {
				subtest := name + "/" + op + fmt.Sprint(root.req.ID)
				if name == "0d898d5eb5647856.json" {
					subtest = op
				}
				t.Run(subtest, func(t *testing.T) {
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
					if err := json.Unmarshal(root.req.Inputs[0], &input); err != nil {
						t.Fatal(err)
					}
					if input.Broker.Registration == nil {
						t.Skip("frozen invocation has no registered caller; procedure negative replay owns this branch")
					}
					o := newProcedureOracle(t, root.req, root.res, root.events, op+".arg0")
					for _, e := range root.events {
						if e.Kind == "request" && (strings.HasSuffix(e.Operation, ".step") || strings.HasSuffix(e.Operation, ".sleep") || strings.HasSuffix(e.Operation, ".broker.arm") || strings.HasSuffix(e.Operation, ".broker.feed")) {
							o.callbacks = append(o.callbacks, e.Operation)
						}
					}
					o.pos = 0
					o.sync()
					now := o.clock.Wall()
					identity := operatorcaller.Identity{RunID: input.Broker.M.RunID, Gate: operator.Rollout, Phase: operatorcaller.Survivor, CallerID: "1", Attempt: "1", PublicSHA: strings.Repeat("a", 40)}
					manifest := operatorcaller.Manifest{Version: 1, Identity: identity, ArmedAt: now.UnixMilli(), CreatedAt: now.UnixMilli(), Deadline: input.Deadline, Plan: operatorcaller.NewPlan(identity.Phase, 60, identity.PublicSHA), Endpoints: operatorcaller.Endpoints{RunID: identity.RunID, Signaling: "https://signal.test/calls", Relay: "192.0.2.1:24700", FrontendControlBoundNS: 1, FrontendWorkerBoundNS: 1, DriftPPM: 100}}
					box := &oracleCallerBox{t: t, oracle: o, manifest: manifest, values: map[string][]byte{}}
					broker, err := operatorcaller.NewBroker(manifest, operatorcaller.NewMailbox(box), oracleCallerClock{o.clock}, &oracleCallerFence{})
					if err != nil {
						if name == "ae05714cbfb25ed6.json" || name == "9df1f6468012b315.json" {
							t.Skip("frozen deadline cannot admit a fresh caller manifest; negative procedure is replayed at callback seam")
						}
						t.Fatal(err)
					}
					if input.Broker.ReadyObservedAt != nil {
						broker.ReadyObservedAt = time.UnixMilli(*input.Broker.ReadyObservedAt)
					}
					if input.Broker.MediaStartedAt != nil {
						broker.MediaStartedAt = time.UnixMilli(*input.Broker.MediaStartedAt)
					}
					broker.Registration = &operatorcaller.Registration{Near: input.Broker.Registration.Near, Sessions: input.Broker.Registration.Sessions, RegisteredAt: *input.Broker.Registration.RegisteredAt}
					adapter := NewCallerRuntime(broker, o.clock, time.Duration(input.Deadline)*time.Millisecond, nil, operatorevidence.Phase{})
					ports := o.ports()
					ports.Broker = adapter
					var actual error
					if op == "trials.trialWarmup" {
						actual = TrialWarmup(context.Background(), ports, time.Duration(input.Deadline)*time.Millisecond)
					} else {
						var target TrialTarget
						var prefix string
						_ = json.Unmarshal(root.req.Inputs[1], &target)
						_ = json.Unmarshal(root.req.Inputs[2], &prefix)
						config := ProcessConfig{Parameters: input.Parameters, ArmedAt: now}
						actual = WorkerTrials(context.Background(), &config, ports, time.Duration(input.Deadline)*time.Millisecond, target, prefix)
						expectedArms, expectedReceipts := 0, 0
						for _, e := range root.events {
							if e.Kind == "request" && strings.HasSuffix(e.Operation, ".broker.arm") {
								expectedArms++
							}
							if e.Kind == "request" && strings.HasSuffix(e.Operation, ".broker.feed") {
								expectedReceipts++
							}
						}
						if box.feeds != expectedArms+expectedReceipts || box.arms != expectedArms || box.receipts != expectedReceipts || len(adapter.Events()) != expectedReceipts {
							t.Fatal("MUTATION: composed-caller-delivery", box.feeds, box.arms, box.receipts, len(adapter.Events()))
						}
					}
					if code(actual) != code(oracleError(root.res)) {
						t.Fatal("MUTATION: composed-trial-oracle", actual, oracleError(root.res))
					}
					o.sync()
					if o.pos < len(o.events) {
						t.Fatal("unconsumed composed callback", o.events[o.pos].Operation)
					}
				})
			}
		}
	}
}
