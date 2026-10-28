// Package observe decodes operator observations into the product's own types.
package observe

import (
	"encoding/json"
	"reflect"

	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/relay"
)

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
		var history struct {
			Takeovers []json.RawMessage `json:"takeovers"`
		}
		if err = json.Unmarshal(data, &history); err == nil {
			for _, raw := range history.Takeovers {
				if err = requireEvidenceContract(raw, reflect.TypeFor[controlplane.MoveResult](), moveContract); err != nil {
					break
				}
			}
		}
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
		err = Require(data, Contract{"agent_session.checkpoint_known", "agent_session.checkpoint_age_ms", "agent_session.snapshot_age_ms"})
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
		err = Require(data, Contract{"continuity.epoch"})
	}
	return v, err
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

// HistoricalMove shares the typed exception ledger with complete evidence.
// Its outer receipt still needs independently observed signal start/end bounds.
func HistoricalMove(data []byte) (controlplane.MoveResult, error) {
	v, err := decode[controlplane.MoveResult](data, nil)
	if err == nil {
		err = requireEvidenceContract(data, reflect.TypeFor[controlplane.MoveResult](), moveContract)
	}
	return v, err
}

// Partial observations retain P2's presence contract, with optionality derived
// from the same typed ledger used by complete raw evidence.
func requireEvidenceContract(data []byte, typ reflect.Type, contract Contract) error {
	var object map[string]any
	if err := json.Unmarshal(data, &object); err != nil {
		return err
	}
	fields := append(Contract{}, contract...)
	for _, member := range evidenceMembers(typ) {
		if EvidenceOptional(typ, member.field, object) == "" {
			continue
		}
		for i, name := range fields {
			if name == member.name {
				fields[i] = "~" + name
			}
		}
	}
	return Require(data, fields)
}
