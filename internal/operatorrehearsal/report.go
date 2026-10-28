package operatorrehearsal

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"

	"github.com/relaishq/relais/internal/operatorevidence"
	"github.com/relaishq/relais/internal/operatorgates"
	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/relaishq/relais/pkg/operator"
)

var CoveredGates = []operator.Gate{operator.Rollout, operator.ControlRestart, operator.UpgradeRollback, operator.RelayDeploy}

type GateReport struct {
	Gate operator.Gate `json:"gate"`
	operatorgates.ProcedureResult
	CallerStopped bool `json:"callerStopped"`
}
type CPUReport struct {
	Layout           CPULayout     `json:"layout"`
	Topology         string        `json:"topology"`
	DriverAffinity   string        `json:"driverAffinity"`
	CallerAffinities []string      `json:"callerAffinities"`
	Calibration      Calibration   `json:"calibration"`
	Phases           []Calibration `json:"phases"`
}
type Report struct {
	Uncertainties  []operator.Uncertainty    `json:"uncertainties"`
	SchemaVersion  int                       `json:"schemaVersion"`
	Commit         string                    `json:"commit"`
	HeadCommit     string                    `json:"headCommit,omitempty"`
	Event          string                    `json:"event"`
	ScenarioSHA256 string                    `json:"scenarioSha256"`
	TemplateSHA256 string                    `json:"templateSha256"`
	BinarySHA256   string                    `json:"binarySha256"`
	CallerSHA256   string                    `json:"callerSha256"`
	JudgeSHA256    []string                  `json:"judgeSha256"`
	ReleaseCommits []string                  `json:"releaseCommits"`
	ArchiveSHA256  []string                  `json:"archiveSha256"`
	Bounds         clocks.Bounds             `json:"bounds"`
	CPU            CPUReport                 `json:"cpu"`
	Gates          []GateReport              `json:"gates"`
	Pool           *operatorevidence.Summary `json:"pooledMedia"`
	Clocks         []clocks.Evidence         `json:"clocks"`
	Errors         []string                  `json:"errors"`
	Latched        bool                      `json:"latched"`
	Cleanup        bool                      `json:"cleanup"`
	Verdict        operator.Verdict          `json:"verdict"`
	Failures       []string                  `json:"failures"`
	Exemptions     []string                  `json:"exemptions"`
	Limitations    []string                  `json:"limitations"`
}

// Evaluate accepts only the two existing relay criteria. A missing duplicate
// counter, unfinished procedure, excluded phase or unknown never qualifies.
func (r *Report) Evaluate() {
	r.Failures = append([]string{}, r.Errors...)
	r.Exemptions = []string{}
	fail := func(s string) { r.Failures = append(r.Failures, s) }
	if !fullDigest.MatchString(r.ScenarioSHA256) {
		fail("invalid-scenario-identity")
	}
	if r.Latched {
		fail("session-latched")
	}
	if !r.Cleanup {
		fail("cleanup-unconfirmed")
	}
	if !r.CPU.Calibration.Passed {
		fail("calibration-failed")
	}
	if len(r.Gates) != len(CoveredGates) {
		fail("gate-count-differs")
	}
	for i, g := range r.Gates {
		if i >= len(CoveredGates) || g.Gate != CoveredGates[i] {
			fail("gate-order-differs")
		}
		if !g.CallerStopped {
			fail(string(g.Gate) + ":caller-stop-unconfirmed")
		}
		if g.Media == nil || g.Media.Status == operator.Failure {
			fail(string(g.Gate) + ":media-missing-or-failed")
		}
		if g.Reason != "" || g.Category != "" {
			fail(string(g.Gate) + ":procedure-error")
		}
		exempt := g.Gate == operator.RelayDeploy && g.Status == operator.Inconclusive && g.Reason == "" && g.Category == "" && g.Process.CompletedSteps > 0 && g.Process.Restarts == 1 && g.Process.DuplicatePackets != nil && *g.Process.DuplicatePackets == 0 && g.Process.RelayEpochDelta != nil && *g.Process.RelayEpochDelta > 0 && g.Process.RoutesRestored != nil && *g.Process.RoutesRestored > 0 && g.Process.RelayLocalTiming == "inconclusive" && g.Process.RelayConnectionInvariants == "inconclusive" && g.Media != nil && g.Media.Status != operator.Failure
		if exempt {
			r.Exemptions = append(r.Exemptions, "relayLocalTiming", "relayConnectionInvariants")
		} else if g.Status != operator.Passed {
			fail(string(g.Gate) + ":not-passed")
		}
	}
	p := r.Pool
	if p == nil || p.Status != operator.Passed || p.QualifiedMoves < 20 || p.QualifiedTakeovers < 10 || p.ExpectedCalls != 4 || p.CompletedCalls != p.ExpectedCalls || p.RelayCalls != 1 || p.EventCount < 1 || p.MetricCount < 1 || p.ExcludedEvents != 0 || p.FailedMetrics != 0 || p.InconclusiveMetrics != 0 || p.DecryptionFailures != 0 || p.ICERestarts != 0 || p.DroppedPhases != 0 || p.IncludedPhases != 4 || p.DuplicatePackets != 0 {
		fail("pooled-media-not-passed")
	}
	r.Verdict = operator.Passed
	if len(r.Failures) > 0 {
		r.Verdict = operator.Failure
	}
}

// Reports are typed projections: no raw mailbox, script, environment, product
// response or error string is serializable here. Restrict all remaining labels.
func publicCode(s string) string {
	switch s {
	case "release-stage-copy-failed", "release-stage-failed", "negative-control-setup-failed", "negative-control-refusal-failed", "negative-control-restore-failed", "negative-control-command-unconfirmed", "negative-control-verification-failed", "media-judge-failed", "source-not-clean", "tested-commit-differs", "linux-affinity-unavailable", "rehearsal-needs-four-allowed-cpus", "judge-compiler-version", "release-digest-differs", "release-manifest-differs", "local-file-unavailable", "read-rate-limited", "read-failed", "release-missing", "release-draft", "release-prerelease", "planned-digest-mismatch", "archive-digest-mismatch", "manifest-policy-drift", "reply-invalid", "asset-missing", "asset-size", "asset-digest-mismatch", "release-changed", "", "release-read-failed", "release-profile-differs", "setup-failed", "boot-failed", "negative-controls-failed", "calibration-failed", "sequence-failed", "cleanup-failed", "caller-stop-unconfirmed", "interrupted", "command-result-unknown", "gate-deadline", "precondition-failed", "precondition-unhealthy", "caller-not-near", "prior-gate-stopped", "not-implemented":
		return s
	case "caller-route-invalid", "caller-not-ready", "caller-deadline", "caller-stopped", "missing-raw-caller", "final-caller-sessions", "caller-affinity-unavailable", "caller-affinity-differs", "proved-not-started", "unknown":
		return s
	}
	return "unclassified"
}
func (r Report) Write(w io.Writer) error {
	if !fullDigest.MatchString(r.ScenarioSHA256) {
		return errors.New("invalid-scenario-identity")
	}
	r.sanitize()
	switch r.Event {
	case "pull_request", "push", "schedule", "workflow_dispatch", "local":
	default:
		r.Event = "unavailable"
	}
	r.Evaluate()
	r.Limitations = []string{"local TLS proxy and time reference; shared runner clock", "linux_amd64 only", "store failover and load admission not covered", "one execution; three fixed complete executions are required for initial calibration", "CPU affinity does not prove physical-core or VM scheduling isolation"}
	e := json.NewEncoder(w)
	e.SetIndent("", "  ")
	return e.Encode(r)
}
func (r Report) Save(name string) error {
	var data bytes.Buffer
	if err := r.Write(&data); err != nil {
		return err
	}
	return os.WriteFile(name, data.Bytes(), 0600)
}
