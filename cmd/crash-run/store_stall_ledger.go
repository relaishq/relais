package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"time"

	"github.com/relaishq/relais/examples/delayed-loopback/ledger"
	"github.com/relaishq/relais/internal/clusterprocess"
	"github.com/relaishq/relais/internal/workerdriver"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/mediaworker"
)

type ledgerFrame struct {
	At                  time.Time
	Status              *mediaworker.AgentSessionStatus
	Metrics             map[string]float64
	Deliveries, Commits int
}
type ledgerStallEvidence struct {
	Before *ledgerFrame
	During []ledgerFrame
	After  *ledgerFrame
	Errors []string
	URL    string
}

func validateStallAgent(variant string, worker workerdriver.Options) error {
	if variant == "" {
		return nil
	}
	if variant == "ledger" {
		if worker.Agent != ledger.StallName {
			return fmt.Errorf("-store-stall=ledger requires -agent=%s", ledger.StallName)
		}
	} else if workerdriver.IsCustom(worker.Agent) && worker.Agent != "delayed-loopback/v1" {
		return fmt.Errorf("-store-stall=%s needs delayed-loopback/v1 semantics; use -store-stall=ledger with -agent=%s for SideEffecting proof", variant, ledger.StallName)
	}
	return nil
}

func (s *storeStallOptions) startLedger(ctx context.Context, manager *clusterprocess.Manager, bin, dir string, env []string) (*clusterprocess.Child, []string, error) {
	addr, err := clusterprocess.FreeTCP()
	if err != nil {
		return nil, nil, err
	}
	child, err := manager.Start(dir, "ledger", withoutKey(env), selectedWorker.Path(bin), "ledger", "-listen", addr, "-file", filepath.Join(dir, "ledger.jsonl"))
	if err != nil {
		return nil, nil, err
	}
	s.Ledger = &ledgerStallEvidence{URL: "http://" + addr}
	startup, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err = clusterprocess.WaitTCP(startup, addr, child); err != nil {
		child.Stop()
		return nil, nil, err
	}
	return child, append(env, "DELAYED_LOOPBACK_LEDGER_URL="+s.Ledger.URL), nil
}
func (s *storeStallOptions) observeLedger(ctx context.Context) *ledgerFrame {
	f := &ledgerFrame{At: time.Now(), Status: s.observeAgent(ctx, s.WorkerURL)}
	client := &http.Client{Timeout: 500 * time.Millisecond}
	var err error
	f.Metrics, _, err = scrapeMetrics(ctx, client, s.WorkerURL+"/metrics")
	if err != nil {
		s.Ledger.Errors = append(s.Ledger.Errors, err.Error())
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.Ledger.URL+"/status", nil)
	if err != nil {
		s.Ledger.Errors = append(s.Ledger.Errors, err.Error())
		return nil
	}
	resp, err := client.Do(req)
	if err != nil {
		s.Ledger.Errors = append(s.Ledger.Errors, err.Error())
		return nil
	}
	defer func() { _ = resp.Body.Close() }()
	var counts struct{ Deliveries, Commits int }
	if resp.StatusCode != http.StatusOK {
		err = fmt.Errorf("ledger diagnostic HTTP %d", resp.StatusCode)
	} else {
		err = json.NewDecoder(resp.Body).Decode(&counts)
	}
	if err != nil {
		s.Ledger.Errors = append(s.Ledger.Errors, err.Error())
		return nil
	}
	f.Deliveries, f.Commits = counts.Deliveries, counts.Commits
	return f
}

func judgeLedgerStall(e *storeStallEvidence) []string {
	var failures []string
	check := func(ok bool, why string) {
		if !ok {
			failures = append(failures, "ledger: "+why)
		}
	}
	a := e.Ledger
	if a == nil || a.Before == nil || a.After == nil || len(a.During) == 0 {
		return []string{"ledger: missing process evidence"}
	}
	check(len(a.Errors) == 0 && e.Agent != nil && len(e.Agent.Errors) == 0, "diagnostics failed")
	b, z := a.Before, a.After
	check(b.Status != nil && b.Status.Name == ledger.StallName && b.Status.CheckpointKnown && b.Status.SideEffectLaunches == 0 && len(b.Status.Tasks) == 0, "pre-stall checkpoint/intents invalid")
	metric := func(f *ledgerFrame, name string) float64 {
		v, ok := f.Metrics["relais_worker_agent_"+name+"_total"]
		check(ok, "missing counter "+name)
		return v
	}
	attempts, successes, failed, debit := metric(b, "forced_checkpoint_attempts"), metric(b, "forced_checkpoint_successes"), metric(b, "forced_checkpoint_failures"), metric(b, "snapshot_debit_bytes")
	check(b.Deliveries == 0 && b.Commits == 0, "ledger already used before cut")
	timedOut, prepared, failedWrite := false, false, false
	for _, f := range a.During {
		check(f.Status != nil, "missing in-stall session")
		if f.Status == nil {
			continue
		}
		check(f.Status.SideEffectLaunches == 0 && f.Deliveries == 0 && f.Commits == 0, "SideEffecting task launched during failed writes")
		check(metric(&f, "forced_checkpoint_successes") == successes, "forced write succeeded during SIGSTOP")
		check(metric(&f, "snapshot_debit_bytes") == debit, "failed forced write debited snapshot tokens")
		failedWrite = failedWrite || (metric(&f, "forced_checkpoint_attempts") > attempts && metric(&f, "forced_checkpoint_failures") > failed)
		for _, task := range f.Status.Tasks {
			prepared = prepared || (task.Step == "commit" && task.Phase == "may_have_started")
			check(task.Outcome != agent.Capacity, "failed forced write became Capacity")
		}
		var app struct {
			Outcomes map[agent.Turn]agent.Outcome `json:"outcomes"`
		}
		err := json.Unmarshal(f.Status.App, &app)
		check(err == nil, "invalid in-stall app state")
		timedOut = timedOut || app.Outcomes[2] == agent.Timeout
		for _, outcome := range app.Outcomes {
			check(outcome != agent.Capacity, "failed forced write delivered Capacity")
		}
	}
	check(prepared, "no prepared SideEffecting intent during stall")
	check(failedWrite, "no failed forced write observed")
	check(timedOut, "deadline Timeout was not delivered locally before launch")
	check(metric(z, "forced_checkpoint_successes") > successes, "no later successful forced intent checkpoint")
	check(metric(z, "snapshot_debit_bytes") > debit, "successful checkpoint debit missing")
	check(z.Status != nil, "missing recovered session")
	if z.Status != nil {
		launch := z.Status.LastTaskLaunch
		check(z.Status.SideEffectLaunches == 1 && launch != nil, "expected exactly one SideEffecting launch")
		if launch != nil {
			check(launch.Step == "commit" && launch.Checkpointed, "launch lacks acknowledged matching intent")
			check(launch.CheckpointAt.After(e.ResumedAt) && !launch.At.Before(launch.CheckpointAt), "launch did not follow a later successful checkpoint")
			check(launch.At.After(e.ResumedAt) && launch.StoreAt.Before(launch.Deadline), "launch outside recovered deadline")
		}
		var app struct {
			Outcomes map[agent.Turn]agent.Outcome `json:"outcomes"`
		}
		err := json.Unmarshal(z.Status.App, &app)
		check(err == nil && app.Outcomes[1] == agent.OK && app.Outcomes[2] == agent.Timeout, "missing commit OK / expired Timeout terminals")
	}
	check(z.Deliveries == 1 && z.Commits == 1, "expired task launched or successful commit missing/duplicated")
	return failures
}
