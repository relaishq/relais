package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/relaishq/relais/examples/delayed-loopback/ledger"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/stretchr/testify/require"
)

func ledgerFixture() *storeStallEvidence {
	resumed := time.Unix(100, 0)
	metrics := func(attempts, successes, failures, debit float64) map[string]float64 {
		return map[string]float64{"relais_worker_agent_forced_checkpoint_attempts_total": attempts, "relais_worker_agent_forced_checkpoint_successes_total": successes, "relais_worker_agent_forced_checkpoint_failures_total": failures, "relais_worker_agent_snapshot_debit_bytes_total": debit}
	}
	before := &ledgerFrame{Metrics: metrics(0, 0, 0, 100), Status: &mediaworker.AgentSessionStatus{Name: ledger.StallName, CheckpointKnown: true}}
	during := ledgerFrame{Metrics: metrics(1, 0, 1, 100), Status: &mediaworker.AgentSessionStatus{App: json.RawMessage(`{"outcomes":{"2":"timeout"}}`), Tasks: []mediaworker.AgentTaskStatus{{Step: "commit", Phase: "may_have_started"}}}}
	after := &ledgerFrame{Metrics: metrics(2, 1, 1, 200), Deliveries: 1, Commits: 1, Status: &mediaworker.AgentSessionStatus{SideEffectLaunches: 1, App: json.RawMessage(`{"outcomes":{"1":"ok","2":"timeout"}}`), LastTaskLaunch: &mediaworker.AgentTaskLaunch{Step: "commit", Checkpointed: true, At: resumed.Add(time.Second), StoreAt: resumed.Add(time.Second), Deadline: resumed.Add(5 * time.Second), CheckpointAt: resumed.Add(500 * time.Millisecond)}}}
	return &storeStallEvidence{ResumedAt: resumed, Agent: &storeStallAgentEvidence{}, Ledger: &ledgerStallEvidence{Before: before, During: []ledgerFrame{during}, After: after}}
}
func TestLedgerStallRequiresRealForcedWriteAndLaunchEvidence(t *testing.T) {
	require.Empty(t, judgeLedgerStall(ledgerFixture()))
	for _, mutation := range []string{"missing", "before", "during", "after", "error", "agent-error", "checkpoint", "already-launched", "preexisting-intents", "preexisting-commit", "preexisting-delivery", "missing-counter", "no-attempt", "no-failure", "forced-success-during", "debit-during", "launch-during", "delivery-during", "commit-during", "prepared", "capacity-task", "capacity-terminal", "timeout", "state-invalid", "during-status", "no-success-after", "no-debit-after", "after-status", "launch-count", "launch-witness", "intent-ack", "step", "checkpoint-before-recovery", "launch-before-checkpoint", "launch-before-recovery", "deadline", "terminal-ok", "terminal-timeout", "delivered-twice", "no-commit"} {
		t.Run(mutation, func(t *testing.T) {
			e := ledgerFixture()
			a := e.Ledger
			b, d, z := a.Before, &a.During[0], a.After
			switch mutation {
			case "missing":
				e.Ledger = nil
			case "before":
				a.Before = nil
			case "during":
				a.During = nil
			case "after":
				a.After = nil
			case "error":
				a.Errors = []string{"failed"}
			case "agent-error":
				e.Agent.Errors = []string{"failed"}
			case "checkpoint":
				b.Status.CheckpointKnown = false
			case "already-launched":
				b.Status.SideEffectLaunches = 1
			case "preexisting-intents":
				b.Status.Tasks = []mediaworker.AgentTaskStatus{{}}
			case "preexisting-commit":
				b.Commits = 1
			case "preexisting-delivery":
				b.Deliveries = 1
			case "missing-counter":
				delete(d.Metrics, "relais_worker_agent_snapshot_debit_bytes_total")
			case "no-attempt":
				d.Metrics["relais_worker_agent_forced_checkpoint_attempts_total"] = 0
			case "no-failure":
				d.Metrics["relais_worker_agent_forced_checkpoint_failures_total"] = 0
			case "forced-success-during":
				d.Metrics["relais_worker_agent_forced_checkpoint_successes_total"] = 1
			case "debit-during":
				d.Metrics["relais_worker_agent_snapshot_debit_bytes_total"] = 101
			case "launch-during":
				d.Status.SideEffectLaunches = 1
			case "delivery-during":
				d.Deliveries = 1
			case "commit-during":
				d.Commits = 1
			case "prepared":
				d.Status.Tasks = nil
			case "capacity-task":
				d.Status.Tasks[0].Outcome = agent.Capacity
			case "capacity-terminal":
				d.Status.App = json.RawMessage(`{"outcomes":{"1":"capacity","2":"timeout"}}`)
			case "timeout":
				d.Status.App = json.RawMessage(`{}`)
			case "state-invalid":
				d.Status.App = json.RawMessage(`invalid`)
			case "during-status":
				d.Status = nil
			case "no-success-after":
				z.Metrics["relais_worker_agent_forced_checkpoint_successes_total"] = 0
			case "no-debit-after":
				z.Metrics["relais_worker_agent_snapshot_debit_bytes_total"] = 100
			case "after-status":
				z.Status = nil
			case "launch-count":
				z.Status.SideEffectLaunches = 2
			case "launch-witness":
				z.Status.LastTaskLaunch = nil
			case "intent-ack":
				z.Status.LastTaskLaunch.Checkpointed = false
			case "step":
				z.Status.LastTaskLaunch.Step = "expire"
			case "checkpoint-before-recovery":
				z.Status.LastTaskLaunch.CheckpointAt = e.ResumedAt
			case "launch-before-checkpoint":
				z.Status.LastTaskLaunch.At = e.ResumedAt.Add(time.Millisecond)
			case "launch-before-recovery":
				z.Status.LastTaskLaunch.At = e.ResumedAt.Add(-time.Second)
			case "deadline":
				z.Status.LastTaskLaunch.StoreAt = z.Status.LastTaskLaunch.Deadline
			case "terminal-ok":
				z.Status.App = json.RawMessage(`{"outcomes":{"2":"timeout"}}`)
			case "terminal-timeout":
				z.Status.App = json.RawMessage(`{"outcomes":{"1":"ok"}}`)
			case "delivered-twice":
				z.Deliveries = 2
			case "no-commit":
				z.Commits = 0
			}
			require.NotEmpty(t, judgeLedgerStall(e))
		})
	}
}
