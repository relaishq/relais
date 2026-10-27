// Package observe decodes operator observations into the product's own types.
package observe

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"reflect"

	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/relay"
)

// Contract describes presence only. Product structs own values and JSON types.
// A trailing [] checks each element; ? permits null (product nullable slices).
// A leading ~ permits omission but still rejects an explicit null.
type Contract []string

var controlContract = Contract{
	"ready", "lost_count", "workers", "calls?", "takeovers?", "pool",
	"workers[].name", "workers[].address", "workers[].dead", "workers[].recovering", "workers[].draining", "workers[].call_count",
	"calls[].id", "calls[].owner", "calls[].lease_epoch",
	"pool.worker_call_cap", "pool.reservations",
}
var moveContract = Contract{"id", "from", "to", "kind", "start", "end", "~lost", "~error"}
var workerContract = Contract{"sessions", "inventory_known", "truncated", "inventory", "address", "inventory[].id", "inventory[].owner", "inventory[].epoch"}
var relayContract = Contract{"instance", "public", "private", "stats", "stats.Workers", "stats.WorkerPackets", "stats.RoutesRestored", "stats.SelfFences"}

func Control(data []byte) (controlplane.Status, error) {
	v, err := decode[controlplane.Status](data, controlContract)
	// Validate history presence against the original bytes, rather than marshaling
	// a zero-filled struct and turning missing evidence into present fields.
	if err == nil {
		err = paths(data, prefix("takeovers[]", moveContract))
	}
	return v, err
}

func Move(data []byte) (controlplane.MoveResult, error) {
	return decode[controlplane.MoveResult](data, moveContract)
}

func Drain(data []byte) (controlplane.DrainResult, error) {
	return decode[controlplane.DrainResult](data, append(Contract{"moves?", "~error"}, prefix("moves[]", moveContract)...))
}

func MoveFailure(data []byte) (controlplane.MoveFailure, error) {
	return decode[controlplane.MoveFailure](data, append(Contract{"error", "codes?", "result"}, prefix("result", moveContract)...))
}

func Worker(data []byte) (mediaworker.WorkerStatus, error) {
	v, err := decode[mediaworker.WorkerStatus](data, workerContract)
	if err == nil && v.AgentSession != nil {
		err = paths(data, Contract{"agent_session.checkpoint_known", "agent_session.checkpoint_age_ms", "agent_session.snapshot_age_ms"})
	}
	return v, err
}

// Draining uses the control placement fence. A live worker's own flag changes
// only on its SIGTERM path and cannot prove that control stopped placement.
func Draining(control controlplane.Status, name string) bool {
	for _, w := range control.Workers {
		if w.Name == name {
			return w.Draining
		}
	}
	return false
}

func Relay(data []byte) (relay.RelayStatus, error) {
	v, err := decode[relay.RelayStatus](data, relayContract)
	if err == nil && v.Continuity != nil {
		err = paths(data, Contract{"continuity.epoch"})
	}
	return v, err
}

// Decode bounds host JSON and rejects duplicates, including unknown nested
// fields, and noncanonical aliases of typed fields. Unknown members are
// otherwise allowed for forward product evolution.
func Decode(data []byte, value any, contract Contract) error {
	if len(data) > 256000 {
		return fmt.Errorf("observation exceeds 256000 bytes")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	if err := unique(d); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return fmt.Errorf("expected one JSON value")
	}
	if err := paths(data, contract); err != nil {
		return err
	}
	if err := canonicalFields(data, reflect.TypeOf(value)); err != nil {
		return err
	}
	return json.Unmarshal(data, value)
}

func decode[T any](data []byte, contract Contract) (v T, err error) {
	err = Decode(data, &v, contract)
	return
}

func prefix(parent string, fields Contract) Contract {
	out := make(Contract, len(fields))
	for i, field := range fields {
		out[i] = parent + "." + field
	}
	return out
}
