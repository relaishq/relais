package main

import (
	"github.com/relais/internal/capacity"
	"github.com/relais/internal/loadgen"
	"github.com/relais/pkg/callharness"
	"github.com/stretchr/testify/require"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func capacityFixture() loadgen.RunReport {
	return loadgen.RunReport{Callers: 2, Outcome: "valid", Yardstick: []loadgen.MetricRow{{Scenario: "move", Metric: "resume", Outcome: "pass"}}, Telemetry: capacity.Telemetry{Processes: []capacity.Process{{Name: "relay", RatesPerCallSecond: map[string]float64{"relais_relay_caller_packets_total": 80, "relais_relay_worker_packets_total": 80, `relais_transport_bytes_total{direction="out",transport="redis"}`: 1000, `relais_store_commands_total{store="session"}`: 40}}}}, Processes: []loadgen.ProcessReport{{Calls: []loadgen.Message{{Call: &callharness.Report{Moves: []callharness.MoveReport{{Kind: "move", Tracks: []callharness.MoveTrackReport{{Kind: "audio", PacketsAfter: 100, Gap: 40 * time.Millisecond, BaselineGapSamples: 50, BaselineGapP95: 20 * time.Millisecond}}}}}}}}}}
}
func TestCapacityRejectsSaturatedGeneratorEvenWithGreenCallerRows(t *testing.T) {
	r := capacityFixture()
	r.Outcome = "inconclusive"
	r.Causes = []string{"saturated interval"}
	step := judgeCapacity("worker", r, 256, 1<<20)
	require.Equal(t, "inconclusive", step.Verdict)
}
func TestCapacityRequiresAllServiceAndResourceEvidence(t *testing.T) {
	r := capacityFixture()
	require.Equal(t, "pass", judgeCapacity("worker", r, 256, 1<<20).Verdict)
	r.Telemetry.Errors = []string{"missing"}
	require.Equal(t, "inconclusive", judgeCapacity("worker", r, 256, 1<<20).Verdict)
	r = capacityFixture()
	r.Yardstick[0].Outcome = "fail"
	require.Equal(t, "fail", judgeCapacity("worker", r, 256, 1<<20).Verdict)
	r = capacityFixture()
	r.Processes[0].Calls[0].Call.Moves[0].Tracks[0].Gap = 200 * time.Millisecond
	require.Equal(t, "fail", judgeCapacity("worker", r, 256, 1<<20).Verdict)
	r = capacityFixture()
	r.Processes[0].Calls[0].Call.Moves[0].Tracks[0].BaselineGapSamples = 2
	require.Equal(t, "inconclusive", judgeCapacity("worker", r, 256, 1<<20).Verdict)
	require.Equal(t, "fail", judgeCapacity("worker", capacityFixture(), 20, 1<<20).Verdict)
}
func TestNPlusOneReservesMoveSlotAndRelayCapacity(t *testing.T) {
	require.Equal(t, 19, plannedLoad(3, 10, 100))
	require.Equal(t, 12, plannedLoad(3, 10, 12))
	require.Zero(t, plannedLoad(3, 0, 100))
	require.LessOrEqual(t, (plannedLoad(3, 10, 100)+1)/2, 10)
}
func TestCostUsesInputPricesAndMeasuredTransfer(t *testing.T) {
	p := priceInput{Currency: "test", Worker: machinePrice{Hourly: 2}, Relay: machinePrice{Hourly: 3}, Control: machinePrice{Hourly: 4}, Store: machinePrice{Hourly: 5}, PublicEgressPerGiB: 6}
	c := calculateCost(p, 3, 9, 1<<20)
	require.Equal(t, 2.0, c.MachinePerCallHour)
	require.Equal(t, 21.09375, c.TransferPerCallHour)
	require.Equal(t, 23.09375, c.TotalPerCallHour)
}

func TestPriceInputsRequireEveryExplicitPrice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prices.json")
	valid := `{"currency":"TEST","worker":{"hourly":0},"relay":{"hourly":2},"control":{"hourly":3},"store":{"hourly":4},"public_egress_per_gib":5}`
	require.NoError(t, os.WriteFile(path, []byte(valid), 0600))
	p, err := readPrices(path)
	require.NoError(t, err)
	require.Zero(t, p.Worker.Hourly)
	for _, invalid := range []string{strings.Replace(valid, `"worker":{"hourly":0}`, `"worker":{}`, 1), strings.Replace(valid, `"public_egress_per_gib":5`, `"extra":5`, 1), strings.Replace(valid, `"hourly":2`, `"hourly":-2`, 1), valid + ` {}`} {
		require.NoError(t, os.WriteFile(path, []byte(invalid), 0600))
		_, err := readPrices(path)
		require.Error(t, err)
	}
}
