package operatorrehearsal

import (
	"context"
	"strconv"
	"time"

	"github.com/relaishq/relais/internal/operatorevidence/relayjudge"
	"github.com/relaishq/relais/internal/operatorgates"
	"github.com/relaishq/relais/internal/operatorgates/artifacts"
	"github.com/relaishq/relais/internal/operatorgates/observe"
	"github.com/relaishq/relais/pkg/operator"
)

func (r *rehearsal) releasePlan() (artifacts.Plan, error) {
	if !r.c.PartR {
		return readReleases(r.c.Assets)
	}
	pin, err := readRelayLossRelease(r.c.Assets)
	return artifacts.Plan{From: pin}, err
}
func (r *rehearsal) partRScenario() (operator.Scenario, error) {
	release := operator.Identity{SourceCommit: ReleaseRCommit, SHA256: ReleaseRSHA256}
	return r.relayJudge.BindScenario(operator.Scenario{SchemaVersion: 1, RelayTwo: true, Gates: []operator.Gate{operator.RelayHostLoss}, Template: operator.Identity{SourceCommit: r.c.Commit, SHA256: r.report.TemplateSHA256}, ReleaseA: release, ReleaseB: release, Caller: operator.Identity{SourceCommit: r.c.Commit, SHA256: r.report.CallerSHA256}})
}

// R has its own deployment and release pin. It enters the same public resource
// sequence as the operator session, including clock admission and host steps.
func (r *rehearsal) rehearsePartR(ctx context.Context) error {
	scenario, err := r.partRScenario()
	if err != nil {
		return err
	}
	r.report.ScenarioSHA256 = scenario.Digest
	if err = r.deployment.Boot(ctx); err != nil {
		r.recordError("boot-failed", err)
		return err
	}
	if err = r.stage(ctx); err != nil {
		r.recordError("setup-failed", err)
		return err
	}
	r.clock = operatorgates.NewRealClock()
	armed := r.clock.Wall()
	adapter := &Adapter{Topology: r.topology, Containers: r.deployment.ContainerIDs(), Runner: r.runner, Diagnostics: r.diagnostics}
	init := operator.Init{Scenario: scenario, ArmedAtUnixMS: strconv.FormatInt(armed.UnixMilli(), 10), EndUnixMS: strconv.FormatInt(armed.Add(operatorgates.SessionMaximum).UnixMilli(), 10), Capabilities: adapter.Capabilities()}
	r.session, err = operatorgates.NewSession(r.run, init, r.clock)
	if err != nil {
		r.recordError("setup-failed", err)
		return err
	}
	r.peer = NewLocalPeer(adapter, r.run, init)
	defer func() { _ = r.peer.Close() }()
	r.process = &operatorgates.ProcessConfig{Parameters: r.parameters(), ArmedAt: armed, Trials: true, Pool: true}
	r.runtime = &operatorgates.ProcessRuntime{Session: r.session, Peer: r.peer, Config: r.process, Architecture: "x86_64", Pins: r.pins}
	r.boundInit = init
	// The boot proof separately exercises negative controls. R keeps its own
	// five-host clock admission before any caller or fault starts.
	r.recordClockSelection(ctx)
	if err = r.bindAdmission(ctx, scenario); err != nil {
		r.recordError("clock-binding-failed", err)
		return err
	}
	if err = r.verifyIdleAt(ctx, r.clock.Elapsed()+2*time.Minute, "part-r"); err != nil {
		r.recordError("precondition-failed", err)
		return err
	}
	if err = r.runPinnedSequence(ctx); err != nil {
		r.recordError("operator-session-incomplete", err)
		return err
	}
	return nil
}

func (r *Report) evaluatePartR() {
	r.Failures = append([]string{}, r.Errors...)
	r.Exemptions = []string{}
	r.TimingUndecided = []TimingUndecided{}
	fail := func(s string) { r.Failures = append(r.Failures, s) }
	if !fullDigest.MatchString(r.ScenarioSHA256) {
		fail("invalid-scenario-identity")
	}
	if !r.Cleanup {
		fail("cleanup-unconfirmed")
	}
	if r.Latched {
		fail("session-latched")
	}
	if len(r.Uncertainties) > 0 {
		fail("command-result-unknown")
	}
	if len(r.Gates) != 1 || r.Gates[0].Gate != operator.RelayHostLoss {
		fail("gate-count-differs")
	} else {
		g := r.Gates[0]
		if !g.ProcedureCompleted || g.Process.CompletedSteps <= 0 {
			fail("relay-host-loss:procedure-incomplete")
		}
		if !g.CallerStopped {
			fail("relay-host-loss:caller-stop-unconfirmed")
		}
		if g.Reason != "" || g.Category != "" {
			fail("relay-host-loss:procedure-error")
		}
		if g.Status != operator.Passed || g.Media == nil || g.Media.Status != operator.Passed {
			fail("relay-host-loss:not-passed")
		}
		judged := g.Process.RelayLoss
		if judged == nil || judged.Outcome != "pass" || len(judged.Causes) != 0 || len(judged.Calls) != 4 {
			fail("relay-host-loss:judge-not-passed")
		} else {
			for i, c := range judged.Calls {
				if c.Ordinal != i || c.Outcome != "pass" || len(c.Causes) != 0 || c.Gap <= 0 || c.Gap > 5*time.Second {
					fail("relay-host-loss:call-not-passed")
				}
			}
		}
	}
	r.Verdict = operator.Passed
	if len(r.Failures) > 0 {
		r.Verdict = operator.Failure
	}
}

// Only the fixed judge vocabulary can reach reports or the run-page summary.
func relayLossCause(s string) bool {
	switch s {
	case "relay-loss-missing", "relay-loss-generator", "relay-loss-target-insufficient", "relay-loss-restore-failed", "relay-loss-call-lost", "relay-loss-decryption", "relay-loss-duplicate", "relay-loss-unaffected-restart", "relay-loss-episode-count", "relay-loss-generation-mismatch", "relay-loss-candidate-mismatch", "relay-loss-anchor-order", "relay-loss-media-order-violated", "relay-loss-unrecovered", "relay-loss-slow":
		return true
	}
	return false
}
func publicRelayLoss(in relayjudge.Result) relayjudge.Result {
	out := relayjudge.Result{Outcome: publicRelayLossOutcome(in.Outcome), Causes: publicRelayLossCauses(in.Causes), Calls: make([]relayjudge.CallResult, len(in.Calls))}
	if in.Generator != nil {
		g := *in.Generator
		g.Checks = []relayjudge.GeneratorCheck{}
		if observe.Numbers(g) == nil {
			for _, check := range in.Generator.Checks {
				if relayjudge.GeneratorRuleAllowed(check.Rule) && observe.Numbers(check) == nil {
					g.Checks = append(g.Checks, check)
				}
			}
			out.Generator = &g
		}
	}
	for i, c := range in.Calls {
		role := c.Role
		if role != "affected" && role != "unaffected" && role != "unknown" {
			role = "unknown"
		}
		out.Calls[i] = relayjudge.CallResult{Ordinal: c.Ordinal, Role: role, Outcome: publicRelayLossOutcome(c.Outcome), Causes: publicRelayLossCauses(c.Causes), Gap: c.Gap}
	}
	return out
}
func publicRelayLossOutcome(s string) string {
	if s == "pass" || s == "fail" || s == "inconclusive" {
		return s
	}
	return "inconclusive"
}
func publicRelayLossCauses(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = "unclassified"
		if relayLossCause(s) {
			out[i] = s
		}
	}
	return out
}
