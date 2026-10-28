package main

import (
	"testing"
)

func TestPoolReportsDroppedSaturatedPhases(t *testing.T) {
	input := rawPoolSample()
	control := pooledPreviewMedia(&input)
	if control.Status != "passed" || control.IncludedPhases != len(input.Phases) || control.DroppedPhases != 0 {
		t.Fatal(control)
	}
	// The unchanged sample builder injects a real send-lateness spike in the
	// event window; this must exclude trials and the whole phase from the pool.
	phase := rawPreviewInput(previewSample(), 40)
	input = previewPoolInput{RunID: phase.RunID, Phases: []previewMediaInput{phase}}
	got := pooledPreviewMedia(&input)
	if got.Status == "passed" || got.DroppedPhases != 1 || got.IncludedPhases != 0 || got.ExcludedEvents == 0 || got.SaturationOverlaps == 0 || len(got.PhaseDiagnostics) != 1 || got.PhaseDiagnostics[0].Cause != "generator saturation overlaps event window" {
		t.Fatal(got)
	}
}
func TestPoolMissingPhaseStillHasDiagnostics(t *testing.T) {
	input := rawPoolSample()
	input.Phases[0].CallerJSONL = ""
	got := pooledPreviewMedia(&input)
	if got.Status == "passed" || got.DroppedPhases != 1 || len(got.PhaseDiagnostics) != len(input.Phases) {
		t.Fatal(got)
	}
}

func TestPoolQualifiesEachPhaseAfterInvalidSibling(t *testing.T) {
	for _, loss := range []bool{false, true} {
		input := rawPoolSample()
		input.Phases[0].CallerJSONL = ""
		input.Phases[0].Receipts[0].Result.Lost = loss
		got := pooledPreviewMedia(&input)
		if got.IncludedPhases != len(input.Phases)-1 || got.DroppedPhases != 1 || !got.PhaseDiagnostics[1].Included {
			t.Fatalf("invalid sibling suppressed independent qualification: %+v", got)
		}
		if loss && got.Status != "failed" {
			t.Fatal("invalid phase erased known loss", got)
		}
	}
}
