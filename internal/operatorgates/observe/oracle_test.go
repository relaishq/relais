package observe

import (
	"encoding/json"
	"testing"

	"github.com/relaishq/relais/internal/operatorgates/oracletest"
	"github.com/relaishq/relais/pkg/controlplane"
)

func TestTypedLostOracle(t *testing.T) {
	seen := map[string]bool{}
	check := func(raw json.RawMessage, got controlplane.MoveResult) {
		var original struct {
			Lost *bool `json:"lost"`
		}
		if err := json.Unmarshal(raw, &original); err != nil {
			t.Fatal(err)
		}
		kind := "omitted"
		if original.Lost != nil {
			kind = "false"
			if *original.Lost {
				kind = "true"
			}
		}
		seen[kind] = true
		if got.Lost != (original.Lost != nil && *original.Lost) {
			t.Fatalf("typed lost oracle: %s became %v", kind, got.Lost)
		}
	}
	for _, c := range oracletest.Typed(t) {
		var envelope struct {
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(c.Result.Value, &envelope); err != nil {
			t.Fatal(err)
		}
		switch c.Decoder {
		case "control":
			got, err := Control(envelope.Data)
			if err != nil {
				t.Fatal(err)
			}
			var original struct {
				LostCount uint64            `json:"lost_count"`
				Takeovers []json.RawMessage `json:"takeovers"`
			}
			_ = json.Unmarshal(envelope.Data, &original)
			if got.LostCount != original.LostCount {
				t.Fatal("oracle lost counter discarded")
			}
			for i, m := range got.Takeovers {
				check(original.Takeovers[i], m)
			}
		case "drain":
			got, err := Drain(envelope.Data)
			if err != nil {
				t.Fatal(err)
			}
			var original struct {
				Moves []json.RawMessage `json:"moves"`
			}
			_ = json.Unmarshal(envelope.Data, &original)
			for i, m := range got.Moves {
				check(original.Moves[i], m)
			}
		case "move":
			got, err := Move(envelope.Data)
			if err != nil {
				t.Fatal(err)
			}
			check(envelope.Data, got)
		}
	}
	for _, kind := range []string{"omitted", "false", "true"} {
		if !seen[kind] {
			t.Fatalf("oracle did not exercise %s lost", kind)
		}
	}
}
