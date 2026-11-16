package operatorrehearsal

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/operatorevidence"
	"github.com/relaishq/relais/internal/operatorgates"
	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/relaishq/relais/pkg/operator"
)

// Pin the real sequence refusal and Evaluate output consumed by Python. The
// media ports supply a clean three-phase population; no real caller is started.
func TestLeasePreparationFlakeFixture(t *testing.T) {
	r := passingReport()
	r.SchemaVersion, r.Event = 1, "pull_request"
	r.ClockSelection = &ClockSelectionReport{}
	r.ClockCoverage = &clocks.CoverageEvidence{DrainComplete: true}
	r.CPU.Phases = []Calibration{{Passed: true}, {Passed: true}, {Passed: true}}
	r.Pool = &operatorevidence.Summary{CompleteMedia: true, Status: operator.Inconclusive,
		QualifiedMoves: 13, QualifiedTakeovers: 10, ExpectedCalls: 3, CompletedCalls: 3,
		IncludedPhases: 3, RelayCalls: 1}
	clock := &operatorgates.FakeClock{Now: time.UnixMilli(1000000)}
	session, err := operatorgates.NewSession("lease-fixture", operator.Init{
		Scenario:      operator.Scenario{SchemaVersion: 1, Digest: strings.Repeat("a", 64), Gates: CoveredGates},
		ArmedAtUnixMS: "1000000", EndUnixMS: "7300000",
	}, clock)
	if err != nil {
		t.Fatal(err)
	}
	var diagnostics []operatorevidence.PhaseDiagnostic
	sequence := session.RunSequence(context.Background(), operatorgates.SequencePorts{
		Prepare: func(_ context.Context, gate operator.Gate, _, _ time.Duration) error {
			if gate == operator.ControlRestart {
				return &operatorgates.ProcedureError{Code: "clock-lease-expired"}
			}
			return nil
		},
		ReleaseCallers: func(context.Context, string, time.Duration) bool { return true },
		Command: func(_ context.Context, gate operator.Gate, _ time.Duration) operatorgates.ProcedureResult {
			diagnostics = append(diagnostics, operatorevidence.PhaseDiagnostic{
				Gate: string(gate), Phase: len(diagnostics), Included: true, EventCount: 10,
			})
			process := operatorgates.ProcessEvidence{CompletedSteps: 1}
			if gate == operator.RelayDeploy {
				process = r.Gates[3].Process
			}
			return operatorgates.ProcedureResult{Status: operator.Passed, Completed: true,
				Process: process,
				Media: &operatorgates.MediaSummary{Status: operator.Inconclusive,
					PhaseDiagnostics: append([]operatorevidence.PhaseDiagnostic{}, diagnostics...)}}
		},
	})
	if session.Latched() || len(diagnostics) != 3 {
		t.Fatal("preparation refusal stopped later gates", sequence)
	}
	r.Gates = gateRows(nil, sequence, nil)
	r.Pool.PhaseDiagnostics = diagnostics
	r.Evaluate()
	wantFailures := []string{"control-restart:procedure-incomplete", "control-restart:media-missing-or-failed",
		"control-restart:procedure-error", "control-restart:not-passed", "pooled-media-not-passed"}
	if !reflect.DeepEqual(r.Failures, wantFailures) {
		t.Fatalf("preparation refusal failure shape changed: %v", r.Failures)
	}
	g := r.Gates[1]
	if g.Reason != "clock-lease-expired" || g.Category != "" || g.Completed || g.ProcedureCompleted ||
		!g.CallerStopped || g.Media != nil || g.Status != operator.Inconclusive {
		t.Fatalf("preparation refusal gate shape changed: %+v", g)
	}
	var raw bytes.Buffer
	if err := r.Write(&raw); err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(raw.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	fixture := projectFlakeReport(document)
	name := filepath.Join("..", "..", "scripts", "ci", "fixtures", "rehearsal-flakes", "lease-preparation-refusal.json")
	// Explicit local regeneration uses the same Go projection as verification.
	if os.Getenv("RELAIS_UPDATE_FLAKE_FIXTURE") == "1" {
		data, err := json.MarshalIndent(fixture, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, append(data, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	var expected map[string]any
	if err := json.Unmarshal(data, &expected); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fixture, expected) {
		t.Fatal("Go report projection differs from the classifier fixture")
	}
}

func flakeFields(value any, keys ...string) map[string]any {
	input := value.(map[string]any)
	out := map[string]any{}
	for _, key := range keys {
		if v, ok := input[key]; ok {
			out[key] = v
		}
	}
	return out
}

func flakeMedia(value any) any {
	if value == nil {
		return nil
	}
	out := flakeFields(value, "status", "failedMetrics", "decryptionFailures", "iceRestarts",
		"lostCalls", "duplicatePackets", "completeMedia", "includedPhases", "droppedPhases",
		"expectedCalls", "completedCalls", "relayCalls", "qualifiedMoves", "qualifiedTakeovers",
		"excludedEvents", "saturationOverlaps")
	rows := []any{}
	for _, row := range value.(map[string]any)["phaseDiagnostics"].([]any) {
		rows = append(rows, flakeFields(row, "gate", "included", "cause", "issue"))
	}
	out["phaseDiagnostics"] = rows
	return out
}

func projectFlakeReport(input map[string]any) map[string]any {
	out := flakeFields(input, "schemaVersion", "verdict", "latched", "cleanup", "errors", "uncertainties", "failures", "timingUndecided")
	rows := []any{}
	for _, value := range input["gates"].([]any) {
		row := flakeFields(value, "gate", "status", "callerStopped", "procedureCompleted", "reason", "category")
		row["media"] = flakeMedia(value.(map[string]any)["media"])
		rows = append(rows, row)
	}
	out["gates"], out["pooledMedia"] = rows, flakeMedia(input["pooledMedia"])
	cpu := input["cpu"].(map[string]any)
	keys := []string{"passed", "causes", "checks", "decoderDropped", "decoderErrors", "duplicatePackets"}
	rows = []any{}
	for _, row := range cpu["phases"].([]any) {
		rows = append(rows, flakeFields(row, keys...))
	}
	out["cpu"] = map[string]any{"calibration": flakeFields(cpu["calibration"], keys...), "phases": rows}
	out["clockSelection"] = flakeFields(input["clockSelection"], "cause")
	out["clockCoverage"] = flakeFields(input["clockCoverage"], "falsified", "drainComplete")
	return out
}
