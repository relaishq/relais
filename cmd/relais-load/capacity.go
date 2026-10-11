package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/relais/internal/clusterprocess"
	"github.com/relais/internal/loadgen"
)

type capacityStep struct {
	Kind                           string            `json:"kind"`
	Verdict                        string            `json:"service_outcome"`
	Causes                         []string          `json:"causes,omitempty"`
	StoreCommandsPerCallSecond     float64           `json:"store_commands_per_call_second"`
	StoreBytesPerCallSecond        float64           `json:"store_bytes_per_call_second"`
	RelayPacketsPerSecond          float64           `json:"relay_packets_per_second"`
	PublicEgressBytesPerCallSecond float64           `json:"public_egress_bytes_per_call_second"`
	DrainSeconds                   float64           `json:"drain_seconds,omitempty"`
	Run                            loadgen.RunReport `json:"run"`
}
type capacityReport struct {
	GOOS                     string         `json:"goos"`
	GOARCH                   string         `json:"goarch"`
	GoVersion                string         `json:"go_version"`
	HostCPUs                 int            `json:"host_cpus"`
	Runner                   string         `json:"runner"`
	Topology                 string         `json:"topology"`
	Profile                  string         `json:"profile"`
	CallersPerProcess        int            `json:"callers_per_process"`
	Workers                  int            `json:"workers"`
	WorkerCapacityLowerBound int            `json:"worker_capacity_lower_bound"`
	RelayCapacityLowerBound  int            `json:"relay_capacity_lower_bound"`
	RelayPPSLowerBound       float64        `json:"relay_packets_per_second_lower_bound"`
	PlannedCallers           int            `json:"candidate_n_plus_one_callers"`
	PlannedPerWorker         float64        `json:"candidate_calls_per_worker"`
	SurvivingPerWorker       int            `json:"candidate_max_calls_per_survivor"`
	PlannedValidated         bool           `json:"planned_load_validated"`
	Outcome                  string         `json:"outcome"`
	StoreOpsBudget           float64        `json:"store_commands_budget_per_call_second"`
	StoreBytesBudget         float64        `json:"store_bytes_budget_per_call_second"`
	Steps                    []capacityStep `json:"steps"`
	Cost                     *costReport    `json:"cost,omitempty"`
	Note                     string         `json:"note"`
}

// judgeCapacity never converts generator validity into a service pass. It
// additionally requires caller continuity, complete threshold rows, resource
// evidence, and the stated store budget. A trusted failure remains visible.
func judgeCapacity(kind string, r loadgen.RunReport, opsBudget, bytesBudget float64) capacityStep {
	s := capacityStep{Kind: kind, Run: r, Verdict: "pass"}
	inconclusive := func(reason string) {
		if s.Verdict != "fail" {
			s.Verdict = "inconclusive"
		}
		s.Causes = append(s.Causes, reason)
	}
	fail := func(reason string) { s.Verdict = "fail"; s.Causes = append(s.Causes, reason) }
	if r.Outcome != "valid" {
		inconclusive("generator invalid: " + strings.Join(r.Causes, "; "))
	}
	if len(r.Yardstick) == 0 {
		inconclusive("no event threshold rows")
	}
	for _, row := range r.Yardstick {
		if row.Outcome == "fail" {
			fail(row.Scenario + " " + row.Metric)
		} else if row.Outcome != "pass" {
			inconclusive(row.Scenario + " " + row.Metric)
		}
	}
	gaps := map[string][]time.Duration{}
	excess := map[string][]time.Duration{}
	for _, p := range r.Processes {
		for _, c := range p.Calls {
			if c.Call == nil {
				inconclusive("missing caller report")
				continue
			}
			report := c.Call
			if report.DecryptionFailures.Total() != 0 || report.ICERestarts != 0 || report.Renegotiations != 0 {
				fail("caller decryption failure or reconnect")
			}
			for _, state := range report.ConnectionStates {
				if state.State == "failed" || state.State == "disconnected" {
					fail("caller connection " + state.State)
				}
			}
			for _, move := range report.Moves {
				if move.Error != "" {
					fail("move error: " + move.Error)
				}
				for _, track := range move.Tracks {
					if track.PacketsAfter == 0 {
						fail("no " + track.Kind + " packets after event")
					}
					if move.Kind == "takeover" {
						if track.Gap >= 2*time.Second {
							fail(track.Kind + " takeover gap >=2s")
						}
						continue
					}
					if track.BaselineGapSamples < 10 {
						inconclusive(track.Kind + " has fewer than ten baseline gap samples")
						continue
					}
					gaps[track.Kind] = append(gaps[track.Kind], track.Gap)
					excess[track.Kind] = append(excess[track.Kind], track.Gap-track.BaselineGapP95)
				}
			}
		}
	}
	for kind, values := range gaps {
		if percentile95(values) > 150*time.Millisecond || percentile95(excess[kind]) > 50*time.Millisecond {
			fail(kind + " move p95 exceeds baseline p95 +50ms or 150ms ceiling")
		}
	}
	if len(r.Telemetry.Processes) == 0 || len(r.Telemetry.Errors) > 0 {
		inconclusive("missing or incomplete service resource samples")
	}
	for _, p := range r.Telemetry.Processes {
		for name, rate := range p.RatesPerCallSecond {
			if strings.HasPrefix(name, "relais_store_commands_total") {
				s.StoreCommandsPerCallSecond += rate
			}
			if strings.HasPrefix(name, "relais_transport_bytes_total{") && strings.Contains(name, `transport="redis"`) {
				s.StoreBytesPerCallSecond += rate
			}
			if p.Name == "relay" && (name == "relais_relay_caller_packets_total" || name == "relais_relay_worker_packets_total") {
				s.RelayPacketsPerSecond += rate * float64(r.Callers)
			}
			if p.Name == "relay" && name == "relais_relay_public_udp_bytes_out_total" {
				s.PublicEgressBytesPerCallSecond += rate
			}
			if p.Name == "relay" && name == "relais_relay_worker_packets_total" {
				s.PublicEgressBytesPerCallSecond += 28 * rate
			} // IPv4+UDP, excluding L2
		}
	}
	if s.StoreCommandsPerCallSecond <= 0 || s.StoreBytesPerCallSecond <= 0 || s.RelayPacketsPerSecond <= 0 {
		inconclusive("missing store or relay traffic evidence")
	}
	if s.StoreCommandsPerCallSecond > opsBudget || s.StoreBytesPerCallSecond > bytesBudget {
		fail("store budget exceeded; checkpoint cadence requires follow-up before operating-load approval")
	}
	for _, e := range r.Events {
		if e.Event.Kind == "drain" {
			s.DrainSeconds = e.FinishedAt.Sub(e.At).Seconds()
		}
	}
	// Saturation is authoritative even if another trusted metric failed.
	if r.Outcome != "valid" {
		s.Verdict = "inconclusive"
	}
	return s
}
func percentile95(values []time.Duration) time.Duration {
	if len(values) == 0 {
		return 0
	}
	values = append([]time.Duration(nil), values...)
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	return values[(95*len(values)+99)/100-1]
}
func plannedLoad(workers, workerCapacity, relayCapacity int) int {
	return max(0, min((workers-1)*workerCapacity-1, relayCapacity))
}

func runCapacity(ctx context.Context, m *clusterprocess.Manager, c config, root string) error {
	host, _ := os.Hostname()
	result := capacityReport{GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, GoVersion: runtime.Version(), HostCPUs: runtime.NumCPU(), Runner: host, Topology: c.Topology, Profile: "lan", CallersPerProcess: c.CallersPerProcess, Workers: c.Workers, StoreOpsBudget: c.StoreOpsBudget, StoreBytesBudget: c.StoreBytesBudget, Outcome: "inconclusive", Note: "Bounded short-run lower bounds, not sustained capacity or exact limits. Stop on the first failed or inconclusive step; generator saturation never establishes service capacity. Loopback is Mac/local evidence; netns is emulated unshaped LAN only. Region/far-caller and 60-second consent are not proven by these samples."}
	var prices *priceInput
	if c.Prices != "" {
		value, err := readPrices(c.Prices)
		if err != nil {
			return err
		}
		prices = &value
	}
	write := func() error { return writeJSON(c.Output, result) }
	executeStep := func(kind string, count, workers int, place bool, events []loadgen.Event) (capacityStep, error) {
		dir := filepath.Join(root, fmt.Sprintf("%02d-%s-%d", len(result.Steps), kind, count))
		if err := os.Mkdir(dir, 0700); err != nil {
			return capacityStep{}, err
		}
		step := c
		step.Callers = count
		step.Processes = (count + c.CallersPerProcess - 1) / c.CallersPerProcess
		step.Workers = workers
		step.PlaceOnWorker = place
		step.Events = events
		runCtx, cancel := context.WithTimeout(ctx, 4*time.Minute+c.Duration)
		defer cancel()
		report, runErr := execute(runCtx, m, step, dir)
		printReport(report)
		judged := judgeCapacity(kind, report, c.StoreOpsBudget, c.StoreBytesBudget)
		if kind == "relay" {
			for worker, peak := range report.WorkerPeaks {
				if peak > result.WorkerCapacityLowerBound {
					judged.Verdict = "inconclusive"
					judged.Causes = append(judged.Causes, fmt.Sprintf("%s exceeds the proven worker load: relay limit cannot be isolated", worker))
				}
			}
		}
		if runErr != nil {
			judged.Verdict = "inconclusive"
			judged.Causes = append(judged.Causes, runErr.Error())
		}
		result.Steps = append(result.Steps, judged)
		fmt.Printf("capacity kind=%s callers=%d service=%s store-commands/call-s=%.2f store-bytes/call-s=%.0f relay-pps=%.1f drain-s=%.3f\n", kind, count, judged.Verdict, judged.StoreCommandsPerCallSecond, judged.StoreBytesPerCallSecond, judged.RelayPacketsPerSecond, judged.DrainSeconds)
		return judged, errors.Join(writeJSON(filepath.Join(dir, "report.json"), judged), write())
	}
	for _, kind := range []string{"worker", "relay"} {
		for n := c.CapacityStart; n <= c.CapacityMax; {
			workers := c.Workers
			place := false
			moveCount := 1
			if kind == "worker" {
				workers = 2
				place = true
				moveCount = 0
			}
			if kind == "relay" && result.WorkerCapacityLowerBound == 0 {
				break
			}
			if kind == "relay" && n > workers*result.WorkerCapacityLowerBound-1 {
				break
			} // a worker bottleneck cannot locate the relay limit
			step, err := executeStep(kind, n, workers, place, []loadgen.Event{{After: 6 * time.Second, Kind: "move", Count: moveCount, Worker: 1}})
			if err != nil {
				return err
			}
			if step.Verdict != "pass" {
				break
			}
			if kind == "worker" {
				result.WorkerCapacityLowerBound = n
			} else {
				result.RelayCapacityLowerBound = n
				result.RelayPPSLowerBound = step.RelayPacketsPerSecond
			}
			if n == c.CapacityMax {
				break
			}
			n = min(n*2, c.CapacityMax)
		}
	}
	result.PlannedCallers = plannedLoad(c.Workers, result.WorkerCapacityLowerBound, result.RelayCapacityLowerBound)
	if result.PlannedCallers >= 2 { // need a killed call and one surviving call
		result.PlannedPerWorker = float64(result.PlannedCallers) / float64(c.Workers)
		result.SurvivingPerWorker = (result.PlannedCallers + c.Workers - 2) / (c.Workers - 1)
		kill, err := executeStep("n-plus-one", result.PlannedCallers, c.Workers, false, []loadgen.Event{{After: 6 * time.Second, Kind: "kill", Worker: 0}, {After: 12 * time.Second, Kind: "move", Count: 1, Worker: -1}})
		if err != nil {
			return err
		}
		for worker, peak := range kill.Run.WorkerPeaks {
			if peak > result.WorkerCapacityLowerBound {
				kill.Verdict = "fail"
				kill.Causes = append(kill.Causes, fmt.Sprintf("%s observed %d calls exceeds capacity %d", worker, peak, result.WorkerCapacityLowerBound))
			}
		}
		result.Steps[len(result.Steps)-1] = kill
		drain, err := executeStep("drain", result.PlannedCallers, c.Workers, false, []loadgen.Event{{After: 6 * time.Second, Kind: "drain", Worker: 0}})
		if err != nil {
			return err
		}
		result.PlannedValidated = kill.Verdict == "pass" && drain.Verdict == "pass"
		if result.PlannedValidated {
			result.Outcome = "pass"
		} else if kill.Verdict == "fail" || drain.Verdict == "fail" {
			result.Outcome = "fail"
		}
		if prices != nil { // cost uses a valid media window at the measured relay load
			for i := len(result.Steps) - 1; i >= 0; i-- {
				step := result.Steps[i]
				if step.Kind == "relay" && step.Verdict == "pass" {
					cost := calculateCost(*prices, c.Workers, result.PlannedCallers, step.PublicEgressBytesPerCallSecond)
					result.Cost = &cost
					break
				}
			}
		}
	}
	if err := write(); err != nil {
		return err
	}
	fmt.Printf("capacity worker-lower-bound=%d relay-call-lower-bound=%d relay-pps-lower-bound=%.1f candidate-N+1=%d validated=%t report=%s\n", result.WorkerCapacityLowerBound, result.RelayCapacityLowerBound, result.RelayPPSLowerBound, result.PlannedCallers, result.PlannedValidated, c.Output)
	if !result.PlannedValidated {
		return errors.New("capacity operating load remains unverified; see capacity report")
	}
	return nil
}

type machinePrice struct {
	Hourly float64 `json:"hourly"`
}
type priceInput struct {
	Currency           string       `json:"currency"`
	Worker             machinePrice `json:"worker"`
	Relay              machinePrice `json:"relay"`
	Control            machinePrice `json:"control"`
	Store              machinePrice `json:"store"`
	PublicEgressPerGiB float64      `json:"public_egress_per_gib"`
}
type costReport struct {
	Inputs                         priceInput `json:"inputs"`
	PlannedCallers                 int        `json:"planned_callers"`
	MachinePerCallHour             float64    `json:"machine_per_call_hour"`
	TransferPerCallHour            float64    `json:"transfer_per_call_hour"`
	TotalPerCallHour               float64    `json:"total_per_call_hour"`
	PublicEgressBytesPerCallSecond float64    `json:"public_egress_bytes_per_call_second"`
	Note                           string     `json:"note"`
}

func readPrices(path string) (priceInput, error) {
	var prices priceInput
	raw, err := os.ReadFile(path)
	if err != nil {
		return prices, err
	}
	if len(raw) > 64<<10 {
		return prices, errors.New("price JSON exceeds 64 KiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&prices); err != nil {
		return prices, err
	}
	// Presence matters: an omitted price must not silently become free.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return prices, err
	}
	for _, role := range []string{"worker", "relay", "control", "store"} {
		var machine map[string]json.RawMessage
		if err := json.Unmarshal(fields[role], &machine); err != nil {
			return prices, fmt.Errorf("missing %s price", role)
		}
		if value, ok := machine["hourly"]; !ok || strings.TrimSpace(string(value)) == "null" {
			return prices, fmt.Errorf("missing %s hourly price", role)
		}
	}
	if value, ok := fields["public_egress_per_gib"]; !ok || strings.TrimSpace(string(value)) == "null" {
		return prices, errors.New("egress price required")
	}
	var tail any
	if decoder.Decode(&tail) != io.EOF {
		return prices, errors.New("price JSON must contain one object")
	}
	if strings.TrimSpace(prices.Currency) == "" {
		return prices, errors.New("price currency required")
	}
	for _, machine := range []machinePrice{prices.Worker, prices.Relay, prices.Control, prices.Store} {
		for _, value := range []float64{machine.Hourly} {
			if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
				return prices, errors.New("prices/resources must be finite and nonnegative")
			}
		}
	}
	if math.IsNaN(prices.PublicEgressPerGiB) || math.IsInf(prices.PublicEgressPerGiB, 0) || prices.PublicEgressPerGiB < 0 {
		return prices, errors.New("egress price must be finite and nonnegative")
	}
	return prices, nil
}
func calculateCost(p priceInput, workers, callers int, egress float64) costReport {
	cost := costReport{Inputs: p, PlannedCallers: callers, PublicEgressBytesPerCallSecond: egress, Note: "Allocated machine cost at measured candidate N+1 occupancy: W workers + one relay/control/store, plus measured public IPv4 UDP egress at input price. No provider prices, discounts, inter-zone charges, provider agent fees, disk, or tax assumed. Valid short-run estimates, not independent-host sizing proof."}
	cost.MachinePerCallHour = (float64(workers)*p.Worker.Hourly + p.Relay.Hourly + p.Control.Hourly + p.Store.Hourly) / float64(callers)
	cost.TransferPerCallHour = egress * 3600 / (1 << 30) * p.PublicEgressPerGiB
	cost.TotalPerCallHour = cost.MachinePerCallHour + cost.TransferPerCallHour
	return cost
}
