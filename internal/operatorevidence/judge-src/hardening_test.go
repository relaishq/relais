package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/operatorcaller"
	"github.com/relaishq/relais/internal/operatorevidence"
	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/relaishq/relais/internal/operatorgates/observe"
	"github.com/relaishq/relais/internal/releasecompat"
)

func evidencePhases(t *testing.T, input previewPoolInput) []operatorevidence.Phase {
	t.Helper()
	var phases []operatorevidence.Phase
	for _, raw := range input.Phases {
		probes := make([]operatorcaller.Probe, len(raw.Probes))
		for i, p := range raw.Probes {
			probes[i] = operatorcaller.Probe{RTTNS: p.RTT, CallerSend: p.Send, CallerReceive: p.Receive, ServerReceive: p.ServerReceive, ServerSend: p.ServerSend}
		}
		burst := operatorcaller.Burst{StartedAt: probes[0].CallerSend, FinishedAt: probes[len(probes)-1].CallerReceive, Probes: probes}
		at := burst.FinishedAt.Add(time.Second)
		plan := operatorcaller.NewPlan(operatorcaller.Survivor, 120, strings.Repeat("a", 40))
		registration, err := operatorcaller.Register(plan, input.RunID, raw.Sessions, burst, operatorcaller.Host{SchemaVersion: 1, AvailableCPUs: 2, LogicalCPUs: 2, CPUModel: "synthetic", CallerGOMAXPROCS: 2, CapturedAt: at}, at, nil)
		if err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(operatorevidence.RawMedia{CallerJSONL: raw.CallerJSONL, Events: raw.Events, Receipts: nil})
		if err != nil {
			t.Fatal(err)
		}
		// Use original receipt JSON and product-owned result types at the real join.
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(data, &fields)
		fields["receipts"], err = json.Marshal(raw.Receipts)
		if err != nil {
			t.Fatal(err)
		}
		data, _ = json.Marshal(fields)
		phases = append(phases, operatorevidence.Phase{Plan: plan, Registration: registration, Raw: data, FirstFaultAt: at.Add(time.Second), DeployedSHA: plan.PublicSHA, Clocks: clocks.Evidence{At: at}, Inventory: releasecompat.Pool{Complete: true}})
	}
	return phases
}
func joinedPool(t *testing.T, phases []operatorevidence.Phase) (operatorevidence.Pool, previewPoolInput) {
	t.Helper()
	pool, _, err := operatorevidence.ReadPool("run-fixture", phases, false)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(pool)
	var input previewPoolInput
	if err := observe.DecodeLimit(data, &input, nil, 64<<20); err != nil {
		t.Fatal(err)
	}
	return pool, input
}
func TestReadPoolRetainsIndependentFailure(t *testing.T) {
	t.Parallel()
	_, complete := joinedPool(t, evidencePhases(t, rawPoolSample()))
	if got := fixturePoolVerdict(&complete); got.Status != "passed" {
		t.Fatal("complete evidence control", got)
	}
	for _, missing := range []string{"events", "callerJsonl", "receipts"} {
		t.Run(missing, func(t *testing.T) {
			t.Parallel()
			sample := rawPoolSample()
			if missing == "receipts" {
				// A caller loss must survive missing server receipts.
				sample.Phases[0].CallerJSONL = strings.Replace(sample.Phases[0].CallerJSONL, `"Kind":"move"`, `"Kind":"move","lost":true`, 1)
			} else {
				sample.Phases[0].Receipts[0].Result.Lost = true
			}
			phases := evidencePhases(t, sample)
			var fields map[string]json.RawMessage
			_ = json.Unmarshal(phases[0].Raw, &fields)
			delete(fields, missing)
			phases[0].Raw, _ = json.Marshal(fields)
			pool, input := joinedPool(t, phases)
			if !pool.Missing || len(pool.Phases) != 2 || fixturePoolVerdict(&input).Status != "failed" {
				t.Fatal("MUTATION: missing-preserves-failure", pool.Missing, fixturePoolVerdict(&input))
			}
		})
	}
	phases := evidencePhases(t, rawPoolSample())
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(phases[0].Raw, &fields)
	delete(fields, "events")
	phases[0].Raw, _ = json.Marshal(fields)
	_, input := joinedPool(t, phases)
	if got := fixturePoolVerdict(&input); got.Status != "inconclusive" {
		t.Fatal(got)
	}
}
func TestRawEvidenceRejectsAmbiguousJSON(t *testing.T) {
	t.Parallel()
	for _, member := range []string{`"lost":true,"lost":false`, `"lost":true,"Lost":false`} {
		sample := rawPoolSample()
		sample.Phases[0].Receipts[0].Result.Lost = true
		phases := evidencePhases(t, sample)
		phases[0].Raw = []byte(strings.Replace(string(phases[0].Raw), `"lost":true`, member, 1))
		if _, _, err := operatorevidence.ReadPool("run-fixture", phases, false); err == nil {
			t.Fatal("MUTATION: raw-json-ambiguity", member)
		}
	}
	// Otherwise passing evidence cannot use duplicate or alias zeroes in JSONL.
	for _, member := range []string{`"AuthTag":1,"AuthTag":0`, `"AuthTag":1,"authtag":0`} {
		input := rawPoolSample()
		input.Phases[0].CallerJSONL = strings.Replace(input.Phases[0].CallerJSONL, `"AuthTag":0`, member, 1)
		if got := fixturePoolVerdict(&input); got.Status == "passed" {
			t.Fatal("MUTATION: jsonl-ambiguity", member, got)
		}
		input.Phases[1].Receipts[0].Result.Lost = true
		if got := fixturePoolVerdict(&input); got.Status != "failed" {
			t.Fatal("independent failure erased", got)
		}
	}
	known := rawPoolSample()
	known.Phases[0].CallerJSONL = strings.Replace(known.Phases[0].CallerJSONL, `"Kind":"move"`, `"Kind":"move","lost":true`, 1)
	known.Phases[0].CallerJSONL = strings.Replace(known.Phases[0].CallerJSONL, `"cpu_percent_per_gomaxprocs":0`, `"cpu_percent_per_gomaxprocs":1,"cpu_percent_per_gomaxprocs":0`, 1)
	if got := fixturePoolVerdict(&known); got.Status != "failed" {
		t.Fatal("MUTATION: jsonl-valid-failure-after-invalid", got)
	}

	// A strict pool entry rejects duplicates before permissive struct decoding.
	raw, _ := json.Marshal(rawPoolSample())
	raw = []byte(strings.Replace(string(raw), `"missing":false`, `"missing":true,"missing":false`, 1))
	if _, err := decodePool(raw); err == nil {
		t.Fatal("MUTATION: pool-json-ambiguity")
	}
}
