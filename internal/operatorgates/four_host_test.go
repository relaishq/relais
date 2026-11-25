package operatorgates

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/operatorgates/host"
	"github.com/relaishq/relais/internal/operatorgates/inventory"
	"github.com/relaishq/relais/internal/releasecompat"
	"github.com/relaishq/relais/pkg/operator"
)

// Pin the default topology and its refusal before adding an optional host.
func TestFourHostProcessScope(t *testing.T) {
	if !reflect.DeepEqual(inventory.Slots, []string{"relay", "w1", "w2", "shared"}) {
		t.Fatal("default host set changed")
	}
	r, _, _ := runtimeFixture(t)
	for _, row := range []struct{ op, slot string }{{"relay-status", "w1"}, {"status", "relay"}, {"telemetry", "other"}, {"relay-status", "relay-two"}} {
		assertScopeRefusal(t, r, row.op, row.slot, ProcessExtra{})
	}
}

func assertScopeRefusal(t *testing.T, r *ProcessRuntime, op, slot string, extra ProcessExtra) {
	t.Helper()
	_, err := r.Step(context.Background(), op, slot, time.Minute, extra)
	if err == nil || err.Error() != "invalid-process-scope" {
		t.Fatalf("scope refusal: %v", err)
	}
}

func TestRelayTwoScenarioAdmission(t *testing.T) {
	r, peer, order := runtimeFixture(t)
	r.Session.scenario.RelayTwo = true
	r.Config.Parameters.RelayTwo = "http://192.0.2.5:24700"
	previous := peer.respond
	peer.respond = func(req operator.Request) operator.Reply {
		if req.Action != "inventory" || req.Target != "relay-two" {
			return previous(req)
		}
		first := req
		first.Target = "relay"
		reply := previous(first)
		var out operator.HostOutput
		if err := json.Unmarshal(reply.Payload, &out); err != nil {
			t.Fatal(err)
		}
		data, err := host.Unpack(out.Stdout, 256000)
		if err != nil {
			t.Fatal(err)
		}
		var envelope struct {
			ObservedAt   int64                       `json:"observedAt"`
			Participants []releasecompat.Participant `json:"participants"`
		}
		if err = json.Unmarshal(data, &envelope); err != nil {
			t.Fatal(err)
		}
		envelope.Participants = envelope.Participants[:1]
		envelope.Participants[0].Name = "relay-two"
		encoded, err := json.Marshal(envelope)
		if err != nil {
			t.Fatal(err)
		}
		return hostReply(t, encoded)
	}
	// Fresh inventory includes one additional participant; all default participants remain.
	pool, err := r.Session.FreshInventory(context.Background(), peer, r.Pins, time.Minute, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(pool.Participants) != 7 {
		t.Fatalf("participant count: %d", len(pool.Participants))
	}
	slot, ok := inventory.ComponentSlot(r.Session.inventoryComponents, "relay-two")
	if !ok || slot != "relay-two" {
		t.Fatal("second relay identity absent")
	}
	// Reset leases so admission must read the optional host as well.
	testRuntimeLeases(t, r)
	for _, op := range []string{"relay-isolate", "relay-restore"} {
		if _, err = r.Step(context.Background(), op, "relay-two", time.Minute, ProcessExtra{}); err != nil {
			t.Fatal(op, err)
		}
	}
	if !slices.Contains(*order, "relay-isolate:relay-two") || !slices.Contains(*order, "relay-restore:relay-two") {
		t.Fatal("optional host not dispatched", *order)
	}
	assertScopeRefusal(t, r, "relay-isolate", "w1", ProcessExtra{})
	assertScopeRefusal(t, r, "relay-isolate", "relay-two", ProcessExtra{IsolationSeconds: 301})

	// The batch transport must join all five hosts, including lease reads.
	r.Session.capabilities[operator.HostReadBatch] = true
	r.Session.capabilities[operator.HostLeaseReadBatch] = true
	individual := peer.respond
	peer.respond = func(req operator.Request) operator.Reply {
		if req.Operation != operator.HostReadBatch && req.Operation != operator.HostLeaseReadBatch {
			return individual(req)
		}
		var args operator.HostReadBatchArgs
		if err := json.Unmarshal(req.Args, &args); err != nil {
			t.Fatal(err)
		}
		if len(args.Reads) != 5 {
			t.Fatal("batch omitted optional host")
		}
		out := operator.HostReadBatchOutput{}
		for _, read := range args.Reads {
			child := individual(operator.Request{Operation: req.Operation, Action: req.Action, Target: read.Target})
			child.ID = read.Target
			out.Replies = append(out.Replies, child)
		}
		zero := 0
		return operator.Reply{Transport: operator.Succeeded, ExitCode: &zero, Payload: oracleJSON(out)}
	}
	if _, err = r.Session.FreshInventory(context.Background(), peer, r.Pins, time.Minute, "", false); err != nil {
		t.Fatal(err)
	}
	read, err := r.Session.ReadLeases(context.Background(), peer, time.Minute)
	if err != nil || len(read.Evidence.Clocks) != 5 || len(read.Sent) != 5 {
		t.Fatal("five-host lease batch", err, read)
	}
	inputs := operator.HostLeaseReadArgs{}
	for _, target := range r.Session.hostSlots() {
		inputs.Reads = append(inputs.Reads, operator.HostLeaseReadInput{Target: target})
	}
	scripts, err := host.LeaseReadScripts(operator.Request{Operation: operator.HostLeaseReadBatch, Action: "clock", Args: oracleJSON(inputs)}, nil)
	if err != nil || len(scripts) != 5 {
		t.Fatal("five-host fixed lease scripts", err)
	}
	r.Config.Parameters.RelayTwo = "https://invalid:443"
	if ValidateProcessParameters(r.Config.Parameters) == nil {
		t.Fatal("invalid second endpoint admitted")
	}
}
