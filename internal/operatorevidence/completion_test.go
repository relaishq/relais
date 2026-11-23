package operatorevidence

import (
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/operatorcaller"
	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/relaishq/relais/internal/releasecompat"
	"github.com/relaishq/relais/pkg/operator"
)

func TestPoolRetainsAbortedCompletion(t *testing.T) {
	at := time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)
	plan := operatorcaller.NewPlan(operatorcaller.Survivor, 60, strings.Repeat("a", 40))
	host := operatorcaller.Host{SchemaVersion: 1, AvailableCPUs: 2, LogicalCPUs: 2, CPUModel: "synthetic", CallerGOMAXPROCS: 2, CapturedAt: at}
	burst := operatorcaller.Burst{StartedAt: at, FinishedAt: at}
	for range operatorcaller.ClockBurstCount {
		burst.Probes = append(burst.Probes, operatorcaller.Probe{RTTNS: 1000000, CallerSend: at, CallerReceive: at, ServerReceive: at, ServerSend: at})
	}
	reg, err := operatorcaller.Register(plan, "run-fixture", []string{"one"}, burst, host, at, nil)
	if err != nil {
		t.Fatal(err)
	}
	phase := Phase{Gate: operator.Rollout, Plan: plan, Registration: reg, FirstFaultAt: at.Add(time.Second), DeployedSHA: plan.PublicSHA, Clocks: clocks.Evidence{At: at}, Inventory: releasecompat.Pool{Complete: true}, Raw: []byte(`{"callerJsonl":"raw","events":[],"receipts":[],"callerCompletion":{"runId":"run-fixture","job":"caller-1","done":false,"stopped":true,"exitCode":1,"sessions":["one"],"stopReason":"generator-saturation-hard-ceiling"},"callerStop":{"runId":"run-fixture","job":"caller-1","reason":"generator-saturation-hard-ceiling"}}`)}
	pool, _, err := ReadPool("run-fixture", []Phase{phase}, true)
	if err != nil || !pool.Missing || len(pool.Phases) != 1 {
		t.Fatalf("aborted phase was lost or qualified: %+v err %v", pool, err)
	}
	got := pool.Phases[0]
	if got.CallerJSONL != "raw" || got.CallerCompletion == nil || !got.CallerCompletion.Stopped || got.CallerCompletion.ExitCode == nil || *got.CallerCompletion.ExitCode != 1 || got.CallerCompletion.StopReason != "generator-saturation-hard-ceiling" || got.CallerStop == nil || got.CallerStop.Reason != "generator-saturation-hard-ceiling" {
		t.Fatalf("aborted receipts were lost from judge artifacts: %+v", got)
	}
}
