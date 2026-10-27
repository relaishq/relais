package observe

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/relay"
)

func TestProductFieldLedger(t *testing.T) {
	types := map[string]any{
		"controlplane.Status": controlplane.Status{}, "controlplane.WorkerStatus": controlplane.WorkerStatus{}, "controlplane.CallStatus": controlplane.CallStatus{}, "controlplane.PoolSample": controlplane.PoolSample{}, "controlplane.MoveResult": controlplane.MoveResult{}, "controlplane.DrainResult": controlplane.DrainResult{}, "controlplane.MoveFailure": controlplane.MoveFailure{},
		"mediaworker.WorkerStatus": mediaworker.WorkerStatus{}, "mediaworker.InstalledSession": mediaworker.InstalledSession{}, "mediaworker.AgentSessionStatus": mediaworker.AgentSessionStatus{},
		"relay.RelayStatus": relay.RelayStatus{}, "relay.ContinuityStatus": relay.ContinuityStatus{}, "relay.Stats": relay.Stats{},
	}
	data, err := os.ReadFile("fields.json")
	if err != nil {
		t.Fatal(err)
	}
	var ledger struct {
		ProductTypes map[string]map[string]string `json:"productTypes"`
	}
	if err := json.Unmarshal(data, &ledger); err != nil {
		t.Fatal(err)
	}
	for name, fields := range ledger.ProductTypes {
		typ := reflect.TypeOf(types[name])
		if typ == nil {
			t.Fatalf("unowned product type %s", name)
		}
		for key, member := range fields {
			field, ok := typ.FieldByName(member)
			if !ok {
				t.Fatalf("missing product member %s.%s", name, member)
			}
			tag := strings.Split(field.Tag.Get("json"), ",")[0]
			if tag == "" {
				tag = member
			}
			if tag != key {
				t.Fatalf("product JSON changed: %s.%s has %s; expected %s", name, member, tag, key)
			}
		}
	}
}

func TestHostObservationEnvelopes(t *testing.T) {
	for _, bad := range []string{`{}`, `{"owners":{"w1":0},"transfers":0}`, `{"owners":{"w1":0,"w2":0}}`, `{"owners":{"w1":-1,"w2":0},"transfers":0}`} {
		if _, err := StoreOwnership([]byte(bad)); err == nil {
			t.Fatalf("missing ownership evidence: %s", bad)
		}
	}
	if _, err := StoreOwnership([]byte(`{"owners":{"w1":0,"w2":0},"transfers":0}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := RelayInspection([]byte(`{"active":"relais-relay.service","pids":{"relais-relay.service":1},"status":{}}`)); err == nil {
		t.Fatal("missing relay standby PID accepted")
	}
	if _, err := LocalInventory([]byte(`{"sessions":0,"inventory_known":true,"truncated":false,"inventory":[]}`)); err != nil {
		t.Fatal(err)
	}
}
