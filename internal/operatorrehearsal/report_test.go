package operatorrehearsal

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/relaishq/relais/internal/operatorevidence"
	"github.com/relaishq/relais/internal/operatorgates"
	"github.com/relaishq/relais/pkg/operator"
)

func passingReport() Report {
	r := Report{Cleanup: true, CPU: CPUReport{Calibration: Calibration{Passed: true}}, Pool: &operatorevidence.Summary{Status: operator.Passed, QualifiedMoves: 20, QualifiedTakeovers: 10, ExpectedCalls: 4, CompletedCalls: 4, MetricCount: 1, EventCount: 30, IncludedPhases: 4}}
	for _, g := range CoveredGates {
		r.Gates = append(r.Gates, GateReport{Gate: g, ProcedureResult: operatorgates.ProcedureResult{Status: operator.Passed, Media: &operatorgates.MediaSummary{Status: operator.Passed}}, CallerStopped: true})
	}
	zero, one := uint64(0), uint64(1)
	r.Gates[3].Status = operator.Inconclusive
	r.Gates[3].Process = operatorgates.ProcessEvidence{CompletedSteps: 5, Restarts: 1, DuplicatePackets: &zero, RelayEpochDelta: &one, RoutesRestored: &one, RelayLocalTiming: "inconclusive", RelayConnectionInvariants: "inconclusive"}
	return r
}
func TestRehearsalPassRule(t *testing.T) {
	r := passingReport()
	r.Evaluate()
	if r.Verdict != operator.Passed || len(r.Exemptions) != 2 {
		t.Fatal(r)
	}
	for name, mutate := range map[string]func(*Report){
		"unknown":                func(r *Report) { r.Latched = true },
		"precondition":           func(r *Report) { r.Gates[0].Reason = "precondition-failed" },
		"known failure":          func(r *Report) { r.Gates[0].Status = operator.Failure },
		"missing gate":           func(r *Report) { r.Gates = r.Gates[:3] },
		"duplicate gate":         func(r *Report) { r.Gates[2].Gate = operator.Rollout },
		"wrong order":            func(r *Report) { r.Gates[0], r.Gates[1] = r.Gates[1], r.Gates[0] },
		"driver":                 func(r *Report) { r.Errors = []string{"sequence-failed"} },
		"gate media failure":     func(r *Report) { r.Gates[0].Media.Status = operator.Failure },
		"stop":                   func(r *Report) { r.Gates[1].CallerStopped = false },
		"cleanup":                func(r *Report) { r.Cleanup = false },
		"calibration":            func(r *Report) { r.CPU.Calibration.Passed = false },
		"pool missing":           func(r *Report) { r.Pool = nil },
		"pool unknown":           func(r *Report) { r.Pool.Status = operator.Inconclusive },
		"phase dropped":          func(r *Report) { r.Pool.DroppedPhases = 1 },
		"phase count":            func(r *Report) { r.Pool.IncludedPhases = 3 },
		"submitted only":         func(r *Report) { r.Pool.QualifiedTakeovers = 9 },
		"duplicates":             func(r *Report) { r.Pool.DuplicatePackets = 1 },
		"decryption":             func(r *Report) { r.Pool.DecryptionFailures = 1 },
		"relay unknown":          func(r *Report) { r.Gates[3].Reason = "command-result-unknown" },
		"relay duplicate absent": func(r *Report) { r.Gates[3].Process.DuplicatePackets = nil },
		"relay media missing":    func(r *Report) { r.Gates[3].Media = nil },
		"relay extra unknown":    func(r *Report) { r.Gates[3].Category = "precondition-failed" },
		"relay missing evidence": func(r *Report) { r.Gates[3].Process.RoutesRestored = nil },
	} {
		t.Run(name, func(t *testing.T) {
			r := passingReport()
			mutate(&r)
			r.Evaluate()
			if r.Verdict != operator.Failure || len(r.Failures) == 0 {
				t.Fatal("unsafe rehearsal pass", r)
			}
		})
	}
}
func TestReportDropsFreeText(t *testing.T) {
	r := passingReport()
	secret := "BEARER-private-value"
	r.Errors = []string{"error " + secret}
	r.Gates[0].Reason = secret
	r.Gates[1].Category = secret
	r.Commit = secret
	r.HeadCommit = secret
	r.Event = secret
	var b bytes.Buffer
	if e := r.Write(&b); e != nil {
		t.Fatal(e)
	}
	if strings.Contains(b.String(), secret) {
		t.Fatal("free text reached public report")
	}
	var parsed Report
	if e := json.Unmarshal(b.Bytes(), &parsed); e != nil {
		t.Fatal(e)
	}
	if parsed.Verdict != operator.Failure || parsed.Gates[0].Reason != "unclassified" {
		t.Fatal(parsed)
	}
}

func TestReportSanitizesEveryTextSurface(t *testing.T) {
	r := passingReport()
	secret := "private-secret"
	r.TemplateSHA256 = secret
	r.BinarySHA256 = secret
	r.CallerSHA256 = secret
	r.JudgeSHA256 = []string{secret}
	r.ReleaseCommits = []string{secret}
	r.ArchiveSHA256 = []string{secret}
	r.CPU.Topology = "# private-secret\n0,0,0,Y\nprivate-secret"
	r.CPU.DriverAffinity = secret
	r.CPU.CallerAffinities = []string{secret}
	r.CPU.Calibration.Causes = []string{secret}
	r.Gates[0].Status = operator.Verdict(secret)
	r.Gates[1].Gate = operator.Gate(secret)
	r.Gates[2].Process.RelayWindows = secret
	r.Gates[3].Media.Status = operator.Verdict(secret)
	r.Pool.PhaseDiagnostics = []operatorevidence.PhaseDiagnostic{{Cause: secret}}
	var out bytes.Buffer
	if e := r.Write(&out); e != nil {
		t.Fatal(e)
	}
	if strings.Contains(out.String(), secret) {
		t.Fatal(out.String())
	}
}
