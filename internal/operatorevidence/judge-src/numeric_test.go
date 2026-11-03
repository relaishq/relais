package main

import (
	"reflect"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/callharness"
)

func TestNumericTimingDoesNotChangePoolVerdictsOrReasons(t *testing.T) {
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
			without := fixturePoolVerdict(&input)
			editPoolCalls(t, &input, func(c *callharness.Report) {
				for i := range c.Moves {
					c.Moves[i].Distributed.Timing = &callharness.TimingEvidence{Version: 1,
						Start:      callharness.ClockMappingEvidence{Probes: []callharness.MappingProbe{}, Host: callharness.OffsetInterval{Lower: -2 * time.Millisecond, Upper: 2 * time.Millisecond}},
						End:        callharness.ClockMappingEvidence{Probes: []callharness.MappingProbe{}},
						Candidates: []callharness.ClassificationCandidate{{Metric: "first_content", Label: "keyframe", Trusted: true}},
					}
				}
			})
			with := fixturePoolVerdict(&input)
			if !reflect.DeepEqual(without, with) {
				t.Fatalf("diagnostics changed verdicts or reason inventories: without=%+v with=%+v", without, with)
			}
			want := map[string]string{"pass": "passed", "fail": "failed", "uncertain": "inconclusive"}[scenario]
			if with.Status != want {
				t.Fatal("fixture did not exercise requested verdict", with.Status, want)
			}
		})
	}
}
