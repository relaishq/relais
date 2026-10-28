package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"reflect"

	"github.com/relaishq/relais/internal/loadgen"
	"github.com/relaishq/relais/internal/operatorgates/observe"
)

// Contract locations are fixed here; never derive diagnostics from call IDs,
// error values, server responses, SDP or opaque map keys.
func collectionPath(err error) string {
	switch err.Error() {
	case "incomplete registration collections", "invalid expected sessions":
		return "$.sessions"
	case "missing caller collections", "invalid probe population", "invalid probe numbers", "caller registration mismatch":
		return "$.callerJsonl.call.distributed.probes"
	case "ambiguous track inventory", "incomplete track inventory", "missing video evidence", "arrival count exceeds packets":
		return "$.callerJsonl.call.Tracks"
	case "missing trials", "move differs from armed inventory", "armed event without move", "incomplete event mapping", "missing event duration bound", "missing classification population", "inconsistent first baselines", "first baseline population changed", "incomplete event collections":
		return "$.callerJsonl.call.Moves"
	case "invalid armed inventory", "incomplete armed inventory":
		return "$.callerJsonl.call.distributed.armed"
	case "invalid receipt scope", "duplicate receipt", "receipt and move inventories differ", "caller and server receipts differ", "receipt without move", "caller receipt identity mismatch":
		return "$.receipts"
	case "incomplete event membership", "invalid event sessions", "invalid event kind", "event identity mismatch", "event and move inventories differ":
		return "$.events"
	default:
		return "$.callerJsonl"
	}
}
func phaseIssue(err error, path string) observe.EvidenceIssue {
	issue := observe.DescribeEvidence(err)
	if issue.Path == "$" {
		issue.Path = path
	} else {
		issue.Path = path + issue.Path[1:]
	}
	return issue
}

func publicPhaseIssue(issue observe.EvidenceIssue) observe.EvidenceIssue {
	return issue.Public(reflect.TypeFor[previewMediaInput](), reflect.TypeFor[loadgen.Message]())
}

// The envelope and each opaque phase are separate validation boundaries.
// Duplicate outer members invalidate the envelope; duplicate members inside a
// phase quarantine that phase without hiding independently valid siblings.
type poolEnvelope struct {
	RunID   string            `json:"runId"`
	Phases  []json.RawMessage `json:"phases"`
	Missing bool              `json:"missing"`
}

func decodePoolEnvelope(data []byte) (wire poolEnvelope, err error) {
	if len(data) > 64<<20 {
		return wire, fmt.Errorf("pool input exceeds bound")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return wire, fmt.Errorf("expected pool object")
	}
	fields := map[string]json.RawMessage{}
	for decoder.More() {
		token, err = decoder.Token()
		if err != nil {
			return wire, err
		}
		key := token.(string)
		if _, duplicate := fields[key]; duplicate {
			return wire, fmt.Errorf("duplicate pool member")
		}
		var raw json.RawMessage
		if err = decoder.Decode(&raw); err != nil {
			return wire, err
		}
		fields[key] = raw
	}
	if _, err = decoder.Token(); err != nil {
		return wire, err
	}
	if _, err = decoder.Token(); err != io.EOF {
		return wire, fmt.Errorf("trailing pool input")
	}
	phases, exists := fields["phases"]
	if exists {
		fields["phases"] = json.RawMessage("[]")
	}
	envelope, err := json.Marshal(fields)
	if err != nil {
		return wire, err
	}
	if err = observe.DecodeLimit(envelope, &wire, observe.Contract{"runId", "phases", "missing"}, 64<<20); err != nil {
		return wire, err
	}
	if err = json.Unmarshal(phases, &wire.Phases); err != nil {
		return wire, err
	}
	if wire.Phases == nil {
		wire.Missing = true
	}
	return wire, nil
}
