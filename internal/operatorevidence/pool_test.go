package operatorevidence

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/operatorcaller"
	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/relaishq/relais/internal/releasecompat"
	"github.com/relaishq/relais/pkg/operator"
)

func TestSummaryPresenceAndFailureDominance(t *testing.T) {
	s := Summary{Status: operator.Passed, ExpectedCalls: 1, CompletedCalls: 1, EventCount: 1, MetricCount: 1}
	raw, _ := json.Marshal(s)
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	delete(fields, "decryptionFailures")
	missing, _ := json.Marshal(fields)
	if _, err := DecodeSummary(missing); err == nil {
		t.Fatal("MUTATION: media-summary-presence")
	}
	s.DecryptionFailures = 1
	s.ExpectedCalls = 0
	s.InconclusiveMetrics = 1
	raw, _ = json.Marshal(s)
	out, err := DecodeSummary(raw)
	if err != nil || out.Status != operator.Failure {
		t.Fatal("MUTATION: known-media-failure", out, err)
	}
	s.DecryptionFailures = 0
	raw, _ = json.Marshal(s)
	out, err = DecodeSummary(raw)
	if err != nil || out.Status != operator.Inconclusive {
		t.Fatal(out, err)
	}
}
func TestPoolRegistrationAndBoundary(t *testing.T) {
	at := time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)
	p := operatorcaller.NewPlan(operatorcaller.Survivor, 60, strings.Repeat("a", 40))
	host := operatorcaller.Host{SchemaVersion: 1, AvailableCPUs: 2, LogicalCPUs: 2, CPUModel: "synthetic", CallerGOMAXPROCS: 2, CapturedAt: at}
	var probes []operatorcaller.Probe
	for range 10 {
		probes = append(probes, operatorcaller.Probe{RTTNS: 1000000, CallerSend: at, CallerReceive: at, ServerReceive: at, ServerSend: at})
	}
	burst := operatorcaller.Burst{StartedAt: at, FinishedAt: at, Probes: probes}
	r, err := operatorcaller.Register(p, "run-fixture", []string{"one"}, burst, host, at, nil)
	if err != nil {
		t.Fatal(err)
	}
	phase := Phase{Gate: operator.RelayDeploy, Plan: p, Registration: r, Raw: []byte(`{"callerJsonl":"raw","events":[],"receipts":[]}`), FirstFaultAt: at.Add(time.Second), DeployedSHA: p.PublicSHA, Clocks: clocks.Evidence{At: at}, Inventory: releasecompat.Pool{Complete: true}}
	pool, sha, err := ReadPool("run-fixture", []Phase{phase}, false)
	if err != nil || sha != p.PublicSHA || pool.Missing || len(pool.Phases) != 1 || pool.Phases[0].Gate != operator.RelayDeploy {
		t.Fatal(pool, err)
	}
	phase.FirstFaultAt = at
	pool, _, err = ReadPool("run-fixture", []Phase{phase}, false)
	if err != nil || !pool.Missing || len(pool.Phases) != 0 {
		t.Fatal("MUTATION: pool-fault-time", pool, err)
	}
	phase.FirstFaultAt = at.Add(time.Second)
	phase.Plan.Phase = operatorcaller.AdmissionBoundary
	phase.Plan.Calls = 2
	phase.Registration, err = operatorcaller.Register(phase.Plan, "run-fixture", []string{"two", "three"}, burst, host, at, nil)
	if err != nil {
		t.Fatal(err)
	}
	pool, _, _ = ReadPool("run-fixture", []Phase{phase}, false)
	if !pool.Missing || len(pool.Phases) != 0 {
		t.Fatal("MUTATION: boundary-pooled")
	}
}

func TestPoolRequiresClocksInventoryAndOnePin(t *testing.T) {
	at := time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)
	plan := operatorcaller.NewPlan(operatorcaller.Survivor, 60, strings.Repeat("a", 40))
	host := operatorcaller.Host{SchemaVersion: 1, AvailableCPUs: 2, LogicalCPUs: 2, CPUModel: "synthetic", CallerGOMAXPROCS: 2, CapturedAt: at}
	burst := operatorcaller.Burst{StartedAt: at, FinishedAt: at}
	for range 10 {
		burst.Probes = append(burst.Probes, operatorcaller.Probe{RTTNS: 1000000, CallerSend: at, CallerReceive: at, ServerReceive: at, ServerSend: at})
	}
	registration, err := operatorcaller.Register(plan, "run-fixture", []string{"one"}, burst, host, at, nil)
	if err != nil {
		t.Fatal(err)
	}
	phase := Phase{Plan: plan, Registration: registration, Raw: []byte(`{"callerJsonl":"raw","events":[],"receipts":[]}`), FirstFaultAt: at.Add(time.Second), DeployedSHA: plan.PublicSHA, Clocks: clocks.Evidence{At: at}, Inventory: releasecompat.Pool{Complete: true}}
	for _, missing := range []string{"clock", "inventory"} {
		p := phase
		if missing == "clock" {
			p.Clocks = clocks.Evidence{}
		} else {
			p.Inventory = releasecompat.Pool{}
		}
		pool, _, err := ReadPool("run-fixture", []Phase{p}, false)
		if err != nil || !pool.Missing || len(pool.Phases) != 1 {
			t.Fatal("MUTATION: pool-proof-missing", pool, err)
		}
	}
	other := phase
	other.Plan.PublicSHA = strings.Repeat("b", 40)
	if _, _, err := ReadPool("run-fixture", []Phase{phase, other}, false); err == nil {
		t.Fatal("MUTATION: pool-mixed-pin")
	}
}

func TestPinnedJudgeSourceBinding(t *testing.T) {
	judge := PinnedJudge{Commit: strings.Repeat("a", 40), Binary: "unopened"}
	if _, err := judge.Judge(context.Background(), Pool{SourceCommit: strings.Repeat("b", 40)}); err == nil || err.Error() != "judge-source-differs" {
		t.Fatal("MUTATION: pinned-judge-source", err)
	}
}

func TestSummaryFailureSurvivesInvalidCounters(t *testing.T) {
	raw, _ := json.Marshal(Summary{Status: operator.Passed, ExpectedCalls: -1, DecryptionFailures: 1})
	got, err := DecodeSummary(raw)
	if err != nil || got.Status != operator.Failure {
		t.Fatal("MUTATION: summary-positive-failure", got, err)
	}
	for _, raw := range []string{
		`{"status":"passed","expectedCalls":9223372036854775808,"completedCalls":0,"eventCount":0,"metricCount":0,"failedMetrics":0,"inconclusiveMetrics":0,"maxMediaGapNs":0,"decryptionFailures":1,"iceRestarts":0}`,
		`{"status":"passed","decryptionFailures":1}`,
	} {
		if got, err := DecodeSummary([]byte(raw)); err != nil || got.Status != operator.Failure {
			t.Fatal("unrepresentable or absent sibling erased failure", got, err)
		}
	}
	if _, err := DecodeSummary([]byte(`{"status":"passed","decryptionFailures":1,"decryptionFailures":0}`)); err == nil {
		t.Fatal("ambiguous summary accepted")
	}
	raw, _ = json.Marshal(Summary{Status: operator.Passed, ExpectedCalls: -1})
	if _, err = DecodeSummary(raw); err == nil {
		t.Fatal("invalid clean summary accepted")
	}
}

func TestEveryRequiredSummaryMember(t *testing.T) {
	control := Summary{Status: operator.Passed, ExpectedCalls: 1, CompletedCalls: 1, EventCount: 1, MetricCount: 1}
	raw, _ := json.Marshal(control)
	if got, err := DecodeSummary(raw); err != nil || got.Status != operator.Passed {
		t.Fatal("passing summary", got, err)
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	for name, original := range fields {
		for _, mode := range []string{"delete", "null"} {
			if mode == "delete" {
				delete(fields, name)
			} else {
				fields[name] = json.RawMessage("null")
			}
			changed, _ := json.Marshal(fields)
			fields[name] = original
			got, err := DecodeSummary(changed)
			if err == nil && got.Status == operator.Passed {
				t.Fatal("MUTATION: recursive-summary-fields", name, mode, got)
			}
		}
	}
}

func TestRelaySummaryDoesNotClaimTrials(t *testing.T) {
	for _, tc := range []struct {
		name                                 string
		relay, calls, events, metrics, moves int
		want                                 operator.Verdict
	}{
		{"relay", 1, 1, 0, 0, 0, operator.Passed},
		{"other-eventless", 0, 1, 0, 0, 0, operator.Inconclusive},
		{"relay-with-events", 1, 1, 1, 1, 0, operator.Inconclusive},
		{"relay-with-moves", 1, 1, 0, 0, 1, operator.Inconclusive},
		{"mixed", 1, 2, 30, 5, 20, operator.Passed},
		{"mixed-without-trials", 1, 2, 0, 0, 0, operator.Inconclusive},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := json.Marshal(Summary{Status: operator.Passed, ExpectedCalls: tc.calls, CompletedCalls: tc.calls, RelayCalls: tc.relay, EventCount: tc.events, MetricCount: tc.metrics, QualifiedMoves: tc.moves})
			got, err := DecodeSummary(raw)
			if err != nil || got.Status != tc.want {
				t.Fatal(got, err)
			}
		})
	}
}
