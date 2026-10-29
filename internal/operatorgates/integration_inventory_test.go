package operatorgates

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestIntegrationCoverageInventory(t *testing.T) {
	raw, err := os.ReadFile("testdata/coverage.json")
	if err != nil {
		t.Fatal(err)
	}
	var coverage struct {
		IntegrationInvocations []struct {
			Fixture, Operation, Test, Evidence, Reason string
			Invocation                                 int
		}
	}
	if err := json.Unmarshal(raw, &coverage); err != nil {
		t.Fatal(err)
	}
	operations := map[string]string{"trials.trialWarmup": "TestComposedTrialOracle", "trials.workerTrials": "TestComposedTrialOracle", "processes.joinedDuplicatePackets": "TestDuplicatePacketsOracle", "measure.summarizeObservations": "TestTelemetryOracle", "processes.ProcessRuntime.verifyClocks": "TestRuntimeClockCacheOracle"}
	corpus, err := loadProcedureCorpus(t)
	if err != nil {
		t.Fatal(err)
	}
	expected := map[deltaBinding]string{}
	for name := range corpus {
		if strings.Contains(name, "/") {
			continue
		}
		f, err := loadProcedureFixture(corpus, name)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range f.Events {
			if e.Kind == "request" && operations[e.Operation] != "" {
				expected[deltaBinding{name, e.Operation, e.ID}] = operations[e.Operation]
			}
		}
	}
	excluded := 0
	for _, row := range coverage.IntegrationInvocations {
		key := deltaBinding{row.Fixture, row.Operation, row.Invocation}
		if expected[key] != row.Test || row.Test == "" {
			t.Fatal("integration invocation identity", key)
		}
		delete(expected, key)
		if row.Evidence == "non-replayable" {
			if row.Reason == "" || row.Fixture != "ae05714cbfb25ed6.json" && row.Fixture != "9df1f6468012b315.json" {
				t.Fatal("unrecorded composition exclusion", key)
			}
			excluded++
		} else if row.Evidence != "replayed" {
			t.Fatal("pending integration invocation", key)
		}
	}
	if len(expected) != 0 || len(coverage.IntegrationInvocations) != 108 || excluded != 4 {
		t.Fatal("integration coverage incomplete", len(expected), len(coverage.IntegrationInvocations), excluded)
	}
}
