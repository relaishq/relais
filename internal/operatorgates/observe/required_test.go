package observe

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestRequiredContractRetainsValidSiblings(t *testing.T) {
	type bounds struct{ Lower, Upper int }
	type evidence struct {
		At       time.Time
		Bounds   bounds
		Values   map[string]bounds
		Failures int
	}
	for _, raw := range []string{
		`{"At":{},"Bounds":{"Lower":0,"Upper":0},"Values":{"resume":{"Lower":0,"Upper":0}},"Failures":1}`,
		`{"At":"bad-time","Bounds":{"Lower":0,"Upper":0},"Values":{"resume":{"Lower":0,"Upper":0}},"Failures":1}`,
		`{"At":null,"Bounds":{},"Values":{"resume":{"Lower":0,"Upper":0}},"Failures":1}`,
		`{"At":"2026-10-25T00:00:00Z","Bounds":null,"Values":{"resume":{}},"Failures":1}`,
		`{"At":"2026-10-25T00:00:00Z","Bounds":3,"Values":{"resume":{"Lower":0,"Upper":0}},"Failures":1}`,
	} {
		var got evidence
		if err := DecodeEvidenceLimit([]byte(raw), &got, 256000); !IsIncompleteEvidence(err) || got.Failures != 1 {
			t.Fatal("MUTATION: required-valid-sibling", got, err)
		}
	}
	// Alias checking still reaches siblings of a malformed typed container.
	var got evidence
	raw := `{"At":{},"Bounds":3,"Values":{"resume":{"Lower":0,"Upper":0}},"Failures":1,"failures":0}`
	if err := DecodeEvidenceLimit([]byte(raw), &got, 256000); err == nil || got.Failures != 0 {
		t.Fatal("ambiguous sibling decoded", got, err)
	}
	raw = `{"At":{},"Bounds":{"Lower":0,"Upper":0},"Values":{},"Failures":1,"Failures":0}`
	if err := DecodeEvidenceLimit([]byte(raw), &got, 256000); err == nil || got.Failures != 0 {
		t.Fatal("duplicate sibling decoded", got, err)
	}
}

func TestRequiredContractPromotedAndMapMembers(t *testing.T) {
	type Endpoints struct{ Lower, Upper int }
	type evidence struct {
		Endpoints
		Durations map[string]Endpoints
	}
	for _, raw := range []string{`{}`, `null`, `{"Lower":0,"Upper":0,"Durations":{"resume":{}}}`, `{"Lower":0,"Upper":0,"Durations":{"resume":null}}`, `{"Lower":0,"Durations":{"resume":{"Lower":0,"Upper":0}}}`} {
		var got evidence
		if err := DecodeEvidenceLimit([]byte(raw), &got, 256000); !IsIncompleteEvidence(err) {
			t.Fatal("required member accepted", json.RawMessage(raw), err)
		}
	}
	var got evidence
	if err := DecodeEvidenceLimit([]byte(`{"Lower":0,"Upper":0,"Durations":{"resume":{"Lower":0,"Upper":0}}}`), &got, 256000); err != nil {
		t.Fatal(err)
	}
}

// Unlike time.Time, an object decoder has members and must not silently opt out
// of recursive presence validation merely by implementing an interface.
type opaqueEvidenceObject struct{ Lower, Upper int }

func (*opaqueEvidenceObject) UnmarshalJSON([]byte) error { return nil }

func TestTimeEvidenceScalarContract(t *testing.T) {
	type timestamp struct{ At time.Time }
	for _, raw := range []string{`{}`, `{"At":null}`, `{"At":{}}`, `{"At":[]}`, `{"At":0}`, `{"At":""}`, `{"At":"bad-time"}`} {
		var got timestamp
		if err := DecodeEvidenceLimit([]byte(raw), &got, 256000); !IsIncompleteEvidence(err) {
			t.Fatal("MUTATION: timestamp-presence-contract", raw, err)
		}
	}
	var got timestamp
	if err := DecodeEvidenceLimit([]byte(`{"At":"2026-10-25T00:00:00Z"}`), &got, 256000); err != nil || got.At.IsZero() {
		t.Fatal("valid timestamp", got, err)
	}
	for _, raw := range []string{`null`, `{}`, `[]`, `0`, `""`, `"bad-time"`} {
		var at time.Time
		if err := DecodeEvidenceLimit([]byte(raw), &at, 256000); !IsIncompleteEvidence(err) {
			t.Fatal("timestamp scalar accepted", raw, err)
		}
	}
}

type opaqueEvidenceScalar string

func (*opaqueEvidenceScalar) UnmarshalText([]byte) error { return nil }

func TestUnreviewedCustomDecoderCannotBypassContract(t *testing.T) {
	for _, raw := range []string{`{"Bounds":{"Lower":0,"Upper":0}}`, `{"Bounds":{"Lower":0}}`} {
		var value struct{ Bounds opaqueEvidenceObject }
		if err := DecodeEvidenceLimit([]byte(raw), &value, 256000); !IsIncompleteEvidence(err) {
			t.Fatal("MUTATION: unreviewed-custom-decoder", err)
		}
	}
	var scalar struct{ Value opaqueEvidenceScalar }
	if err := DecodeEvidenceLimit([]byte(`{"Value":"opaque"}`), &scalar, 256000); !IsIncompleteEvidence(err) {
		t.Fatal("MUTATION: unreviewed-custom-decoder", "text decoder accepted", err)
	}
}

// Named collections can own decoders, just as ordinary structs/scalars can.
type opaqueEvidenceSlice []int
type opaqueEvidenceArray [1]int
type opaqueEvidenceMap map[string]int

func (*opaqueEvidenceSlice) UnmarshalJSON([]byte) error { return nil }
func (*opaqueEvidenceArray) UnmarshalJSON([]byte) error { return nil }
func (*opaqueEvidenceMap) UnmarshalJSON([]byte) error   { return nil }

func TestCustomEvidenceKeysAndContainers(t *testing.T) {
	for _, tc := range []struct {
		name string
		typ  reflect.Type
		raw  string
	}{
		{"key", reflect.TypeFor[map[opaqueEvidenceScalar]int](), `{"opaque":1}`},
		{"slice", reflect.TypeFor[opaqueEvidenceSlice](), `[1]`},
		{"array", reflect.TypeFor[opaqueEvidenceArray](), `[1]`},
		{"map", reflect.TypeFor[opaqueEvidenceMap](), `{"opaque":1}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := EvidenceFields([]byte(tc.raw), tc.typ); err == nil {
				t.Fatal("MUTATION: custom-evidence-key-container", tc.name)
			}
			// Failure-only extraction must not bypass the same type policy.
			if err := EvidenceTypeFields([]byte(tc.raw), tc.typ, reflect.TypeFor[opaqueEvidenceObject]()); err == nil {
				t.Fatal("MUTATION: custom-evidence-failure-key-container", tc.name)
			}
		})
	}
}
