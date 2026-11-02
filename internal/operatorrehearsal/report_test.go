package operatorrehearsal

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/relaishq/relais/internal/operatorevidence"
	"github.com/relaishq/relais/internal/operatorgates"
	"github.com/relaishq/relais/pkg/operator"
)

func passingReport() Report {
	r := Report{ScenarioSHA256: strings.Repeat("a", 64), Cleanup: true, CPU: CPUReport{Calibration: Calibration{Passed: true}}, Pool: &operatorevidence.Summary{CompleteMedia: true, Status: operator.Passed, QualifiedMoves: 20, QualifiedTakeovers: 10, ExpectedCalls: 4, CompletedCalls: 4, RelayCalls: 1, MetricCount: 1, EventCount: 30, IncludedPhases: 4}}
	for _, g := range CoveredGates {
		r.Gates = append(r.Gates, GateReport{Gate: g, ProcedureCompleted: true, ProcedureResult: operatorgates.ProcedureResult{Status: operator.Passed, Process: operatorgates.ProcessEvidence{CompletedSteps: 1}, Media: &operatorgates.MediaSummary{Status: operator.Passed}}, CallerStopped: true})
	}
	for i, g := range CoveredGates {
		n := 10
		if i == 3 {
			n = 0
		}
		r.Pool.PhaseDiagnostics = append(r.Pool.PhaseDiagnostics, operatorevidence.PhaseDiagnostic{Phase: i, Gate: string(g), EventCount: n, Included: true})
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
		"relay call missing":     func(r *Report) { r.Pool.RelayCalls = 0 },
		"relay counted twice":    func(r *Report) { r.Pool.RelayCalls = 2 },
		"phase count":            func(r *Report) { r.Pool.IncludedPhases = 3 },
		"missing trial phase":    func(r *Report) { r.Pool.PhaseDiagnostics[2].EventCount = 0 },
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

func TestReportPreservesRestoreFailedReason(t *testing.T) {
	r := passingReport()
	r.Gates[0].Status = operator.Inconclusive
	r.Gates[0].Reason = "restore-failed"
	var out bytes.Buffer
	if err := r.Write(&out); err != nil {
		t.Fatal(err)
	}
	var saved Report
	if err := json.Unmarshal(out.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Gates[0].Reason != "restore-failed" {
		t.Fatal("restoration reason lost during sanitization", saved.Gates[0].Reason)
	}
}

func TestReportPreservesPreconditionCategories(t *testing.T) {
	for _, category := range []string{"inventory", "relay-manifest-identity", "w1-not-running", "ssl-cert-verification-error", "store-incomplete", "observation", "compatibility-refused", "precondition-failed:inventory", "precondition-failed:status", "precondition-failed:live-calls", "precondition-failed:w2-local-inventory"} {
		r := passingReport()
		r.Gates[0].Reason, r.Gates[0].Category = "precondition-failed", category
		var out bytes.Buffer
		if err := r.Write(&out); err != nil {
			t.Fatal(err)
		}
		var saved Report
		if err := json.Unmarshal(out.Bytes(), &saved); err != nil || saved.Gates[0].Category != category {
			t.Fatalf("precondition category lost: want %q got %q: %v", category, saved.Gates[0].Category, err)
		}
	}
	for _, category := range []string{"precondition-failed:customer-production-db", "inventory:secret-value", "w1-private-account", "CustomSecretError"} {
		if publicCode(category) != "unclassified" {
			t.Fatal("free-form precondition code escaped", category)
		}
	}
}

func TestReportPreservesSafeUnknownContext(t *testing.T) {
	r := passingReport()
	r.Uncertainties = []operator.Uncertainty{{Kind: "mailbox.read", RequestID: "7", Target: "caller", CallerID: "1", Action: "caller-probes", DeadlineUnixMS: "1800000000000", RemainingMS: 0, Rule: "timeout"}}
	var out bytes.Buffer
	if err := r.Write(&out); err != nil {
		t.Fatal(err)
	}
	var saved Report
	if err := json.Unmarshal(out.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved.Uncertainties) != 1 || saved.Uncertainties[0] != r.Uncertainties[0] {
		t.Fatal("unknown context lost from report", out.String())
	}
	secret := "BEARER-private-value"
	r.Uncertainties = []operator.Uncertainty{{Kind: secret, RequestID: secret, Target: secret, CallerID: secret, Action: secret, DeadlineUnixMS: secret, Rule: secret}}
	out.Reset()
	if err := r.Write(&out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), secret) {
		t.Fatal("unknown diagnostics disclosed free text")
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

func TestRehearsalRejectsInconclusive(t *testing.T) {
	for _, target := range []string{"gate", "pool"} {
		r := passingReport()
		if target == "gate" {
			r.Gates[0].Status = operator.Inconclusive
		} else {
			r.Pool.Status = operator.Inconclusive
		}
		r.Evaluate()
		if r.Verdict != operator.Failure {
			t.Fatal("MUTATION: rehearsal-pass-accepts-inconclusive", target, r)
		}
	}
}

func TestRehearsalExemptionOnlyRelay(t *testing.T) {
	r := passingReport()
	// A non-relay gate cannot borrow the relay's two criterion exemptions even
	// when it supplies every other field of a completed relay procedure.
	r.Gates[0].Status = operator.Inconclusive
	r.Gates[0].Process = r.Gates[3].Process
	r.Evaluate()
	if r.Verdict != operator.Failure || len(r.Exemptions) != 2 {
		t.Fatal("MUTATION: rehearsal-exemption-widened", r)
	}
}

func TestReportRefusesMissingScenarioIdentity(t *testing.T) {
	for _, identity := range []string{"", "not-a-digest"} {
		r := passingReport()
		r.ScenarioSHA256 = identity
		r.Evaluate()
		if r.Verdict != operator.Failure {
			t.Fatal("MUTATION: scenario-report-identity", r)
		}
		var out bytes.Buffer
		if err := r.Write(&out); err == nil || out.Len() != 0 {
			t.Fatal("MUTATION: scenario-report-identity", err)
		}
		name := filepath.Join(t.TempDir(), "report.json")
		if err := r.Save(name); err == nil {
			t.Fatal("invalid identity report was saved")
		}
		if _, err := os.Stat(name); !os.IsNotExist(err) {
			t.Fatal("identity refusal left a report artifact", err)
		}
	}
}
