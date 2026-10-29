package main

import (
	"bytes"
	"encoding/json"

	"github.com/relaishq/relais/internal/loadgen"
	"github.com/relaishq/relais/internal/operatorgates/observe"
)

// The product emits null Moves/armed for a call with no operations. Accept
// those explicit empty collections only for relay-deploy. Omitted fields still
// fail the recursive evidence contract, as do null trial collections elsewhere.
func decodeCallerMessage(raw json.RawMessage, m *loadgen.Message, relay bool) (json.RawMessage, error) {
	if relay {
		// Check types, duplicates and canonical field names before normalizing JSON.
		if err := observe.DecodeLimit(raw, m, nil, 64<<20); err != nil {
			return raw, err
		}
		if m.Type == "call" && m.Call != nil {
			var message, call, distributed map[string]json.RawMessage
			if err := json.Unmarshal(raw, &message); err != nil {
				return raw, err
			}
			if err := json.Unmarshal(message["call"], &call); err != nil {
				return raw, err
			}
			if bytes.Equal(call["Moves"], []byte("null")) {
				call["Moves"] = json.RawMessage("[]")
			}
			if m.Call.Distributed != nil {
				if err := json.Unmarshal(call["distributed"], &distributed); err != nil {
					return raw, err
				}
				if bytes.Equal(distributed["armed"], []byte("null")) {
					distributed["armed"] = json.RawMessage("[]")
				}
				call["distributed"], _ = json.Marshal(distributed)
			}
			message["call"], _ = json.Marshal(call)
			raw, _ = json.Marshal(message)
		}
	}
	return raw, observe.DecodeEvidenceLimit(raw, m, 64<<20)
}
