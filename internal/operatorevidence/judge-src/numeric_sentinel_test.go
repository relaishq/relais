package main

import (
	"testing"

	"github.com/relaishq/relais/internal/operatorgates/observe"
	"github.com/relaishq/relais/pkg/callharness"
)

func TestDistributedCallerWorkerSentinelsQualify(t *testing.T) {
	input := rawPoolSample()
	for i := range input.Phases {
		// Match the recorded distributed-caller shape: nonnumeric worker names
		// yield From/To=-1, while operation identities retain the real owners.
		one := previewPoolInput{Phases: input.Phases[i : i+1]}
		replaceCall(t, &one, func(c *callharness.Report) {
			for j := range c.Moves {
				c.Moves[j].From, c.Moves[j].To = -1, -1
			}
			if err := observe.Numbers(c); err != nil {
				t.Fatal(err)
			}
		})
	}
	got := pooledPreviewMedia(&input)
	if got.Status != "passed" || got.IncludedPhases != 2 || got.QualifiedMoves != 20 || got.QualifiedTakeovers != 10 {
		t.Fatal(got)
	}
}
