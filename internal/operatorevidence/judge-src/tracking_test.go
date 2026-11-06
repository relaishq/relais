package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/relaishq/relais/pkg/callharness"
)

func TestTrackingEvidenceKeepsStoredVerdictBytes(t *testing.T) {
	for _, scenario := range []string{"pass", "fail", "uncertain"} {
		t.Run(scenario, func(t *testing.T) {
			input := rawPoolSample()
			editPoolCalls(t, &input, func(c *callharness.Report) {
				switch scenario {
				case "fail":
					c.DecryptionFailures.AuthTag = 1
				case "uncertain":
					c.Moves[0].Measurement.FirstContentVerdict = callharness.MetricVerdict{Reasons: []string{"first_content classification changes within event-time interval; width=4ms"}}
				}
			})
			phases := evidencePhases(t, input)
			beforePool, before := joinedPool(t, phases)
			for i := range phases {
				phases[i].Clocks.Version = clocks.EvidenceVersion
				phases[i].Clocks.Clocks = map[string]clocks.Sample{"relay": {TrackingLog: &clocks.TrackingLog{
					Rows:   []clocks.TrackingRow{{OffsetSeconds: 1}},
					Breaks: []clocks.TrackingBreak{{Kind: "self-consistency"}, {Kind: "leap-status"}},
				}}}
			}
			afterPool, after := joinedPool(t, phases)
			beforeInput, _ := json.Marshal(beforePool)
			afterInput, _ := json.Marshal(afterPool)
			if !bytes.Equal(beforeInput, afterInput) {
				t.Fatal("tracking entered judge input")
			}
			want, _ := json.Marshal(fixturePoolVerdict(&before))
			got, _ := json.Marshal(fixturePoolVerdict(&after))
			if !bytes.Equal(want, got) {
				t.Fatal("tracking changed verdict bytes")
			}
			name := filepath.Join("testdata", "tracking-"+scenario+".json")
			stored, err := os.ReadFile(name)
			if err != nil || !bytes.Equal(bytes.TrimSpace(stored), got) {
				t.Fatalf("stored verdict bytes changed: %v", err)
			}
		})
	}
}
