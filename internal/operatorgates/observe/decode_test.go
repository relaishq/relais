package observe

import (
	"encoding/json"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/relay"
)

func marshal(t *testing.T, v any) []byte {
	t.Helper()
	b, e := json.Marshal(v)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func without(t *testing.T, data []byte, path ...string) []byte {
	t.Helper()
	var v map[string]json.RawMessage
	if e := json.Unmarshal(data, &v); e != nil {
		t.Fatal(e)
	}
	if len(path) == 1 {
		delete(v, path[0])
	} else {
		v[path[0]] = without(t, v[path[0]], path[1:]...)
	}
	return marshal(t, v)
}

func TestProductPresenceContracts(t *testing.T) {
	address := netip.MustParseAddrPort("192.0.2.1:24711")
	status := controlplane.Status{Ready: true, Workers: []controlplane.WorkerStatus{{Name: "w1", Address: address}}, Calls: []controlplane.CallStatus{{ID: "one", Owner: "w1", Epoch: 7}}, Pool: controlplane.PoolSample{Cap: 20}}
	data := marshal(t, status)
	value, err := Control(data)
	if err != nil || value.Pool.Demand != nil || value.Takeovers != nil {
		t.Fatalf("product control JSON contract: %v", err)
	}
	for _, field := range []string{"ready", "lost_count", "workers", "calls", "takeovers", "pool"} {
		if _, err := Control(without(t, data, field)); err == nil {
			t.Fatalf("missing %s accepted", field)
		}
	}
	for _, field := range []string{"dead", "recovering", "draining", "call_count"} {
		var raw map[string]json.RawMessage
		_ = json.Unmarshal(data, &raw)
		raw["workers"] = []byte("[" + string(without(t, marshal(t, status.Workers[0]), field)) + "]")
		if _, err := Control(marshal(t, raw)); err == nil {
			t.Fatalf("missing worker %s accepted", field)
		}
	}
	for _, field := range []string{"worker_call_cap", "reservations"} {
		if _, err := Control(without(t, data, "pool", field)); err == nil {
			t.Fatalf("missing pool %s accepted", field)
		}
	}
	local := mediaworker.WorkerStatus{Address: address, InventoryKnown: true, Inventory: []mediaworker.InstalledSession{}}
	for _, field := range []string{"sessions", "inventory_known", "truncated", "inventory", "address"} {
		if _, err := Worker(without(t, marshal(t, local), field)); err == nil {
			t.Fatalf("missing local %s accepted", field)
		}
	}
	if _, err := Worker(marshal(t, local)); err != nil {
		t.Fatal(err)
	}
	if _, err := Worker([]byte(`{"sessions":0,"inventory_known":true,"truncated":false,"inventory":null,"address":"192.0.2.1:24711"}`)); err == nil {
		t.Fatal("null inventory accepted as empty")
	}
	if Draining(status, "w1") {
		t.Fatal("control fence fabricated")
	}
	status.Workers[0].Draining = true
	if !Draining(status, "w1") || local.Draining {
		t.Fatal("worker-own-draining-required: control placement fence ignored")
	}
}

func TestMoveLostOmission(t *testing.T) {
	now := time.Date(2026, 10, 20, 1, 0, 0, 0, time.UTC)
	m := controlplane.MoveResult{ID: "one", From: "w1", To: "w2", Kind: "takeover", Start: now, End: now.Add(time.Second)}
	data := marshal(t, m)
	if strings.Contains(string(data), `"lost"`) {
		t.Fatal("product lost no longer omits false")
	}
	for _, suffix := range []string{"", `,"lost":false`, `,"lost":true`} {
		encoded := append(append([]byte{}, data[:len(data)-1]...), []byte(suffix+"}")...)
		got, err := Move(encoded)
		if err != nil || got.Lost != strings.Contains(suffix, "true") {
			t.Fatalf("lost omission contract: %+v %v", got, err)
		}
	}
	for _, field := range []string{"id", "from", "to", "kind", "start", "end"} {
		if _, err := Move(without(t, data, field)); err == nil {
			t.Fatalf("missing move %s accepted", field)
		}
	}
	if _, err := Move(append(data[:len(data)-1], []byte(`,"lost":null}`)...)); err == nil {
		t.Fatal("null lost accepted as false")
	}
	m.Lost, m.To, m.Error = true, "", "ownership expired"
	if got, err := Move(marshal(t, m)); err != nil || !got.Lost {
		t.Fatalf("known loss with no target discarded: %v", err)
	}
	drain, err := Drain(marshal(t, controlplane.DrainResult{Error: "drain failed"}))
	if err != nil || drain.Error == "" {
		t.Fatalf("drain failure discarded: %v", err)
	}
	failure, err := MoveFailure(marshal(t, controlplane.MoveFailure{Error: "move refused", Result: m}))
	if err != nil || failure.Error == "" || !failure.Result.Lost {
		t.Fatalf("move HTTP failure discarded: %v", err)
	}
}

func TestRelayPresenceAndProductJSON(t *testing.T) {
	r := relay.RelayStatus{Instance: "relay-one", Public: netip.MustParseAddrPort("192.0.2.1:3478"), Private: netip.MustParseAddrPort("192.0.2.1:24700")}
	data := marshal(t, r)
	got, err := Relay(data)
	if err != nil || got.Continuity != nil {
		t.Fatalf("nil continuity invented evidence: %v", err)
	}
	for _, field := range []string{"Workers", "WorkerPackets", "RoutesRestored", "SelfFences"} {
		if _, err := Relay(without(t, data, "stats", field)); err == nil {
			t.Fatalf("missing relay counter %s accepted", field)
		}
	}
	for _, broken := range []string{`"public":"same"`, `"stats":null`, `"stats":{"Workers":0.5}`, `"stats":{"Workers":-1}`} {
		if _, err := Relay([]byte(`{"instance":"one","private":"192.0.2.1:24700",` + broken + `}`)); err == nil {
			t.Fatalf("malformed relay accepted: %s", broken)
		}
	}
}

func TestMalformedProductObservations(t *testing.T) {
	for _, data := range []string{`null`, `{}`, `{"ready":true,"ready":false}`, `{"sessions":0} {}`, `{"sessions":0,"inventory_known":null,"truncated":false,"inventory":[]}`} {
		if _, err := Worker([]byte(data)); err == nil {
			t.Fatalf("invalid observation accepted: %s", data)
		}
	}
}

func TestNoncanonicalProductFields(t *testing.T) {
	control := controlplane.Status{Workers: []controlplane.WorkerStatus{{Name: "w1", Dead: true, Recovering: true, Draining: true}}}
	controlJSON := string(marshal(t, control))
	workerJSON := string(marshal(t, mediaworker.WorkerStatus{Inventory: []mediaworker.InstalledSession{}}))
	relayJSON := string(marshal(t, relay.RelayStatus{}))
	moveJSON := string(marshal(t, controlplane.MoveResult{}))
	controlDecode := func(b []byte) error { _, err := Control(b); return err }
	workerDecode := func(b []byte) error { _, err := Worker(b); return err }
	relayDecode := func(b []byte) error { _, err := Relay(b); return err }
	moveDecode := func(b []byte) error { _, err := Move(b); return err }
	cases := []struct {
		name   string
		data   string
		decode func([]byte) error
	}{
		{"workers alias", controlJSON[:len(controlJSON)-1] + `,"WORKERS":[{"name":"w1"}]}`, controlDecode},
		{"nested pool", strings.Replace(controlJSON, `"reservations":0`, `"reservations":0,"RESERVATIONS":9`, 1), controlDecode},
		{"lost alias null", moveJSON[:len(moveJSON)-1] + `,"LOST":null}`, moveDecode},
		{"lost alias false", moveJSON[:len(moveJSON)-1] + `,"Lost":false}`, moveDecode},
		{"worker known flag", strings.Replace(workerJSON, `"inventory_known":false`, `"inventory_known":false,"INVENTORY_KNOWN":true`, 1), workerDecode},
		{"relay safety counter", strings.Replace(relayJSON, `"SelfFences":0`, `"SelfFences":0,"selffences":7`, 1), relayDecode},
	}
	for _, field := range []string{"dead", "recovering", "draining"} {
		cases = append(cases, struct {
			name   string
			data   string
			decode func([]byte) error
		}{field, strings.Replace(controlJSON, `"`+field+`":true`, `"`+field+`":true,"`+strings.ToUpper(field)+`":false`, 1), controlDecode})
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := c.decode([]byte(c.data)); err == nil {
				t.Fatal("noncanonical product field accepted")
			}
		})
	}
	// Extension fields and dictionary keys do not name struct fields.
	var extension struct {
		Values map[string]int `json:"values"`
	}
	if err := Decode([]byte(`{"values":{"dead":1,"DEAD":2},"new_field":{"READY":true}}`), &extension, Contract{"values"}); err != nil || len(extension.Values) != 2 {
		t.Fatalf("extension or dictionary keys rejected: %v", err)
	}
}
