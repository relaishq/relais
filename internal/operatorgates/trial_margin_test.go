package operatorgates

import (
	"context"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/operator"
)

func TestPartATrialMargin(t *testing.T) {
	moves, takeovers, qualified := 0, 0, 0
	for _, row := range []struct {
		gate operator.Gate
		want TrialTarget
	}{{operator.Rollout, TrialTarget{7, 7}}, {operator.ControlRestart, TrialTarget{7, 6}}, {operator.UpgradeRollback, TrialTarget{6, 3}}} {
		t.Run(string(row.gate), func(t *testing.T) {
			if CampaignDuration(row.gate) != 1500*time.Second || TrialTargets(row.gate) != row.want || CampaignMinimum(row.gate) != 30*time.Minute {
				t.Fatal("MUTATION: part-a-trial-margin")
			}
			// Exhaust the replacement budget. Two exclusions in each gate still
			// leave ten regular takeovers across Part A, without a replacement.
			c := newTrialCluster(t)
			c.g.gate = row.gate
			c.g.c.KillReplacements = MaxKillReplacements
			c.wide = map[int]bool{1: true, 2: true}
			if err := c.g.trials(context.Background(), TrialTargets(row.gate), "margin"); err != nil {
				t.Fatal(err)
			}
			m, k := receiptKinds(c.g.p.Broker)
			if m != row.want.Moves || k != row.want.Takeovers || c.g.evidence.KillReplacements != 0 || c.g.evidence.ExcludedKills != 2 {
				t.Fatal("MUTATION: part-a-trial-margin", m, k, c.g.evidence)
			}
			moves += m
			takeovers += k
			qualified += k - c.g.evidence.ExcludedKills
		})
	}
	if moves != 20 || takeovers != 16 || qualified != 10 {
		t.Fatal("MUTATION: part-a-trial-margin", moves, takeovers, qualified)
	}
}

func TestCampaignLimitsPreserveOtherGates(t *testing.T) {
	for gate, media := range map[operator.Gate]time.Duration{operator.RelayDeploy: 600 * time.Second, operator.Failover: 1020 * time.Second, operator.Load: 1020 * time.Second} {
		if CampaignDuration(gate) != media || TrialTargets(gate) != (TrialTarget{}) || CampaignMinimum(gate) > GateMaximum {
			t.Fatal("MUTATION: campaign-scope", gate)
		}
	}
	// Part B has two gates. Explicit admission media keeps its existing limits.
	if !(AdmissionPlan{SurvivorSeconds: 240, BoundarySeconds: 240}).valid() || 2*GateMaximum > MutationWindow {
		t.Fatal("MUTATION: campaign-scope: part B no longer fits")
	}
	if GateMaximum != 31*time.Minute || SessionMaximum != 105*time.Minute || MutationWindow != 103*time.Minute || 3*GateMaximum > MutationWindow {
		t.Fatal("MUTATION: campaign-scope: Part A exceeds session")
	}
}

func TestResourceDeadlineArithmetic(t *testing.T) {
	for _, row := range []struct {
		name                        string
		gateEnd, callerEnd, stopEnd time.Duration
	}{{"gate-first", 31 * time.Minute, 33 * time.Minute, 31 * time.Minute}, {"caller-first", 31 * time.Minute, 30 * time.Minute, 30 * time.Minute}} {
		t.Run(row.name, func(t *testing.T) {
			_, clock := fixtureSession(t)
			caller := &CallerRuntime{End: row.callerEnd}
			runResourceCommand(context.Background(), clock, caller, row.gateEnd, ProcedurePorts{}, func(_ context.Context, p ProcedurePorts, end time.Duration) ProcedureResult {
				if end != row.stopEnd-4*time.Minute || p.CollectionEnd != row.stopEnd-2*time.Minute || caller.End != p.CollectionEnd {
					t.Fatal("MUTATION: resource-deadline-arithmetic", end, p.CollectionEnd, caller.End)
				}
				clock.Advance(p.CollectionEnd - clock.Elapsed())
				return ProcedureResult{Status: operator.Inconclusive}
			}, func(_ context.Context, end time.Duration) bool {
				if end != row.stopEnd {
					t.Fatal("MUTATION: resource-deadline-arithmetic", end)
				}
				return true
			})
		})
	}
}
