package operatorgates

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/relaishq/relais/internal/operatorgates/inventory"
	"github.com/relaishq/relais/internal/releasecompat"
	"github.com/relaishq/relais/pkg/operator"
)

func TestResourceConfigSourceBinding(t *testing.T) {
	for _, mode := range []string{"release-caller", "operator-caller", "judge-hash", "caller-judge-source", "release-a", "release-b"} {
		t.Run(mode, func(t *testing.T) {
			raw, err := os.ReadFile("testdata/resource-session.json")
			if err != nil {
				t.Fatal(err)
			}
			var f struct {
				Scenario operator.Scenario
				Config   ResourceConfig
			}
			if err = json.Unmarshal(raw, &f); err != nil {
				t.Fatal(err)
			}
			f.Config.JudgeBinary = filepath.Join(t.TempDir(), "judge")
			if err = os.WriteFile(f.Config.JudgeBinary, []byte("judge-stub"), 0600); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "operator-caller":
				f.Scenario.Caller.SourceCommit = strings.Repeat("c", 40)
				f.Scenario.Judge.SourceCommit = f.Scenario.Caller.SourceCommit
			case "judge-hash":
				f.Scenario.Judge.SHA256 = strings.Repeat("d", 64)
			case "caller-judge-source":
				f.Scenario.Caller.SourceCommit = strings.Repeat("c", 40)
			case "release-a":
				f.Scenario.ReleaseA.SourceCommit = strings.Repeat("c", 40)
			case "release-b":
				f.Scenario.ReleaseB.SourceCommit = strings.Repeat("c", 40)
			}
			err = (ResourceRunner{Config: f.Config}).validate(&Session{scenario: f.Scenario})
			wantOK := mode == "release-caller" || mode == "operator-caller"
			if (err == nil) != wantOK || err != nil && err.Error() != "invalid-resource-config" {
				t.Fatal("MUTATION: resource-source-binding", mode, err)
			}
		})
	}
}

func TestResourceInventoryTampering(t *testing.T) {
	for _, field := range []string{"control", "commit", "archive", "manifest-commit"} {
		t.Run(field, func(t *testing.T) {
			raw, err := os.ReadFile("testdata/resource-session.json")
			if err != nil {
				t.Fatal(err)
			}
			var fixture struct {
				Config      ResourceConfig
				Inventories map[string]json.RawMessage
			}
			if err = json.Unmarshal(raw, &fixture); err != nil {
				t.Fatal(err)
			}
			var live struct {
				ObservedAt   int64                       `json:"observedAt"`
				Participants []releasecompat.Participant `json:"participants"`
			}
			if err = json.Unmarshal(fixture.Inventories["w1"], &live); err != nil {
				t.Fatal(err)
			}
			switch field {
			case "commit":
				live.Participants[0].Commit = strings.Repeat("c", 40)
			case "archive":
				live.Participants[0].ArchiveSHA256 = strings.Repeat("e", 64)
			case "manifest-commit":
				live.Participants[0].Manifest.Commit = strings.Repeat("c", 40)
			}
			raw, err = json.Marshal(live)
			if err != nil {
				t.Fatal(err)
			}
			_, err = inventory.Parse("w1", raw, fixture.Config.Pins)
			if (err == nil) != (field == "control") {
				t.Fatal("MUTATION: resource-inventory-tamper", field, err)
			}
		})
	}
}
