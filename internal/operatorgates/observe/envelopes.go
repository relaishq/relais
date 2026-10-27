package observe

import (
	"encoding/json"
	"fmt"

	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/relay"
)

// Inspection and Ownership are host observations, not product status copies.
type Inspection struct {
	Active string            `json:"active"`
	PIDs   map[string]int    `json:"pids"`
	Status relay.RelayStatus `json:"status"`
}
type Ownership struct {
	Owners    map[string]int `json:"owners"`
	Transfers int            `json:"transfers"`
}

func RelayInspection(data []byte) (Inspection, error) {
	var raw struct {
		Active string          `json:"active"`
		PIDs   map[string]*int `json:"pids"`
		Status json.RawMessage `json:"status"`
	}
	err := Decode(data, &raw, Contract{"active", "pids", "status"})
	// Unit names contain dots and are map keys, not path components. Validate
	// that host-owned dictionary directly after the envelope shape is decoded.
	if err != nil {
		return Inspection{}, err
	}
	for _, unit := range []string{"relais-relay.service", "relais-relay-standby.service"} {
		if pid := raw.PIDs[unit]; pid == nil || *pid < 0 {
			return Inspection{}, fmt.Errorf("missing relay PID observation")
		}
	}
	pids := make(map[string]int, len(raw.PIDs))
	for unit, pid := range raw.PIDs {
		if pid == nil || *pid < 0 {
			return Inspection{}, fmt.Errorf("invalid relay PID observation")
		}
		pids[unit] = *pid
	}
	if raw.Active != "relais-relay.service" && raw.Active != "relais-relay-standby.service" || pids[raw.Active] <= 0 {
		return Inspection{}, fmt.Errorf("unknown active relay")
	}
	status, err := Relay(raw.Status)
	return Inspection{raw.Active, pids, status}, err
}

func StoreOwnership(data []byte) (v Ownership, err error) {
	err = Decode(data, &v, Contract{"owners.w1", "owners.w2", "transfers"})
	if err == nil && (v.Owners["w1"] < 0 || v.Owners["w2"] < 0 || v.Transfers < 0) {
		err = fmt.Errorf("negative ownership count")
	}
	return
}

// LocalInventory decodes the bounded inventory projection produced by host
// telemetry into the original worker type. Telemetry does not forward address.
func LocalInventory(data []byte) (mediaworker.WorkerStatus, error) {
	return decode[mediaworker.WorkerStatus](data, Contract{"sessions", "inventory_known", "truncated", "inventory", "inventory[].id", "inventory[].owner", "inventory[].epoch"})
}
