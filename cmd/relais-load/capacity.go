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
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/relaishq/relais/internal/clusterprocess"
	"github.com/relaishq/relais/internal/loadgen"
	"github.com/relaishq/relais/pkg/callharness"
)

type capacityStep struct {
	RelayBufferEnabled             *bool              `json:"relay_buffer_enabled"`
	StoreBytesByClient             map[string]float64 `json:"store_bytes_by_client_per_call_second"`
	Kind                           string             `json:"kind"`
	Verdict                        string             `json:"service_outcome"`
	Causes                         []string           `json:"causes,omitempty"`
	StoreCommandsPerCallSecond     float64            `json:"store_commands_per_call_second"`
	StoreBytesPerCallSecond        float64            `json:"store_bytes_per_call_second"`
	RelayPacketsPerSecond          float64            `json:"relay_packets_per_second"`
	PublicEgressBytesPerCallSecond float64            `json:"public_egress_bytes_per_call_second"`
	DrainSeconds                   float64            `json:"drain_seconds,omitempty"`
	// GeneratorOutcome is generator validity under the capacity rule; Run
	// keeps the strict loadgen judge and processes[].saturated as raw evidence.
	GeneratorOutcome string `json:"generator_outcome"`
	// ToleratedSlips lists isolated lateness or skipped-frame samples that did
	// not invalidate the generator (#133).
	ToleratedSlips []string          `json:"tolerated_lateness_slips,omitempty"`
	Run            loadgen.RunReport `json:"run"`
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
	CostStatus               string         `json:"cost_status"`
	RelayBufferEnabled       *bool          `json:"relay_buffer_enabled"`
	Cost                     *costReport    `json:"cost,omitempty"`
	Note                     string         `json:"note"`
}

// judgeCapacity never converts generator validity into a service pass. It
// additionally requires caller continuity, complete threshold rows, resource
// evidence, and the stated store budget. A trusted failure remains visible.
// An isolated generator tick slip does not invalidate the step (#133), but
// its stall bound keeps a near-threshold move or takeover gap from passing.
func judgeCapacity(kind string, r loadgen.RunReport, opsBudget, bytesBudget float64) capacityStep {
	generator, tolerated := capacityGenerator(r)
	s := judgeCapacityStep(kind, generator, opsBudget, bytesBudget, nil, true)
	s.Run, s.ToleratedSlips = r, tolerated
	return s
}

// excluded is used only by soak for individually inconclusive event windows.
// The soak bounds caller stalls per event in soakGapRows instead.
func judgeCapacityExcept(kind string, r loadgen.RunReport, opsBudget, bytesBudget float64, excluded map[int]bool) capacityStep {
	return judgeCapacityStep(kind, r, opsBudget, bytesBudget, excluded, false)
}

// capacityGenerator judges generator validity for capacity on a copy of r
// with the soak's rule (saturationCauses): one isolated sample with lateness
// p99 or skipped frames in (limit, 2x limit] does not invalidate a step by
// itself. Processes with dropped samples and any cause added after
// RunReport.Judge, such as an execution error, stay strict. It returns the
// copy and each tolerated sample; r is unchanged.
func capacityGenerator(r loadgen.RunReport) (loadgen.RunReport, []string) {
	if r.Outcome == "valid" {
		return r, nil
	}
	strict := r
	strict.Events = append([]loadgen.EventResult(nil), r.Events...)
	strict.Judge()
	if strict.Outcome != r.Outcome || !slices.Equal(strict.Causes, r.Causes) {
		return r, nil
	}
	view := r
	view.Processes = append([]loadgen.ProcessReport(nil), r.Processes...)
	view.Events = append([]loadgen.EventResult(nil), r.Events...)
	var tolerated []string
	for i := range view.Processes {
		p := &view.Processes[i]
		if !p.Saturated || p.SamplesDropped > 0 || p.PeakCPUPerGOMAXPROCS > r.Limits.CPUPercent {
			continue
		}
		var slips []string
		sustained := false
		// Lateness 1 keeps "no successful media sends" out of the peak causes.
		peak := loadgen.Stats{Lateness: loadgen.Lateness{Packets: 1}, IntervalLateness: loadgen.Lateness{P99: p.PeakIntervalP99}}
		for j, s := range p.Samples {
			if sampleSaturated(p.Samples, j, r.Limits, r.StartedAt, mediaEnd(r, *p)) {
				sustained = true
				break
			}
			// ReadProcess keeps the first sample with the largest skip share.
			if s.IntervalPacing.SkippedPercent() > peak.IntervalPacing.SkippedPercent() {
				peak.IntervalPacing = s.IntervalPacing
			}
			if overLimit(s, r.Limits) {
				var excess []string
				if s.IntervalLateness.P99 > r.Limits.P99 {
					excess = append(excess, fmt.Sprintf("interval send-lateness p99 %s exceeds %s", s.IntervalLateness.P99, r.Limits.P99))
				}
				if s.IntervalPacing.SkippedPercent() > r.Limits.SkippedPercent {
					excess = append(excess, fmt.Sprintf("skipped frames %.2f%% exceeds %.2f%%", s.IntervalPacing.SkippedPercent(), r.Limits.SkippedPercent))
				}
				slips = append(slips, fmt.Sprintf("process %d: isolated %s in the sample ending %s after start", p.PID, strings.Join(excess, " and "), s.At.Sub(r.StartedAt).Round(time.Millisecond)))
			}
		}
		if sustained || len(slips) == 0 {
			continue
		}
		// Drop only the peak lateness and skipped-frame causes that
		// ReadProcess derived from these samples; every other cause remains.
		tolerable := r.Limits.Causes(peak)
		p.Causes = slices.DeleteFunc(slices.Clone(p.Causes), func(c string) bool { return slices.Contains(tolerable, c) })
		p.Saturated = false
		tolerated = append(tolerated, slips...)
	}
	if len(tolerated) == 0 {
		return r, nil
	}
	view.Judge()
	return view, tolerated
}

// judgeCapacityStep is the shared step judge. With stallGuard, each move's
// caller stall bound is added to its gap checks: a pass that the bound could
// turn into a failure is inconclusive, a measured failure still fails, and no
// track leaves a percentile.
func judgeCapacityStep(kind string, r loadgen.RunReport, opsBudget, bytesBudget float64, excluded map[int]bool, stallGuard bool) capacityStep {
	s := capacityStep{Kind: kind, Run: r, Verdict: "pass", GeneratorOutcome: r.Outcome, StoreBytesByClient: map[string]float64{}}
	if hasDistributedCalls(r) {
		s.Verdict = "inconclusive"
		s.Causes = []string{"local capacity policy refuses distributed point estimates"}
		return s
	}
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
		if excluded[row.EventIndex] {
			continue
		}
		if row.Outcome == "fail" {
			fail(row.Scenario + " " + row.Metric)
		} else if row.Outcome != "pass" {
			inconclusive(row.Scenario + " " + row.Metric)
		}
	}
	gaps := map[string][]time.Duration{}
	excess := map[string][]time.Duration{}
	worstGaps, worstExcess := map[string][]time.Duration{}, map[string][]time.Duration{}
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
			for _, track := range report.Tracks {
				outsideGap := func(end, gap time.Duration, description string) {
					if !gapOutsideScheduledEvent(r.Events, c.Session, report, end, gap) {
						return
					}
					// A stall in this call's generator can explain the amount
					// above the 150ms ceiling even when interval p99 is valid.
					if p.Lateness.Max >= gap-150*time.Millisecond {
						// Only this gap is ambiguous. The severity helper
						// retains any independent failure in the step.
						inconclusive(fmt.Sprintf("%s %s: generator maximum send lateness %s can explain gap excess %s", track.Kind, description, p.Lateness.Max, gap-150*time.Millisecond))
						return
					}
					fail(track.Kind + " " + description + " outside scheduled events exceeds 150ms")
				}
				if gapOutsideScheduledEvent(r.Events, c.Session, report, track.MediaGapEndedAt, track.MediaGap) {
					outsideGap(track.MediaGapEndedAt, track.MediaGap, "whole-call media gap")
				}
				for i := 1; i < len(track.Arrivals); i++ {
					if gapOutsideScheduledEvent(r.Events, c.Session, report, track.Arrivals[i], track.Arrivals[i]-track.Arrivals[i-1]) {
						outsideGap(track.Arrivals[i], track.Arrivals[i]-track.Arrivals[i-1], "observed media gap")
						break
					}
				}
				if !stallGuard {
					continue
				}
				// A tolerated stall can shorten a real interruption to a passing
				// gap (#133). Judge every retained gap, not only the longest.
				for _, gap := range track.MediaGaps {
					if gap.EndedAt != track.MediaGapEndedAt && gapOutsideScheduledEvent(r.Events, c.Session, report, gap.EndedAt, gap.Duration) {
						outsideGap(gap.EndedAt, gap.Duration, "retained media gap")
					}
				}
				if reason := gapInventoryFullCause(track); reason != "" {
					inconclusive(reason)
				}
				seen := map[time.Duration]bool{}
				near := func(end, gap time.Duration) {
					if reason := stalledGapCause(r, p, c.Session, report, track.Kind, end, gap); reason != "" && !seen[end] {
						seen[end] = true
						inconclusive(reason)
					}
				}
				near(track.MediaGapEndedAt, track.MediaGap)
				for i := 1; i < len(track.Arrivals); i++ {
					near(track.Arrivals[i], track.Arrivals[i]-track.Arrivals[i-1])
				}
				for _, gap := range track.MediaGaps {
					near(gap.EndedAt, gap.Duration)
				}
			}
			for _, move := range report.Moves {
				skip := false
				for i, e := range r.Events {
					if excluded[i] && loadgen.MatchesEvent(e, report, move) {
						skip = true
					}
				}
				if skip {
					continue
				}
				if move.Error != "" {
					fail("move error: " + move.Error)
				}
				// Only this call's generator can shorten its gaps (#129).
				var bound time.Duration
				if stallGuard {
					bound = stallBound(p.Samples, r.Limits.P99, report.StartedAt.Add(move.Start-time.Second), report.StartedAt.Add(max(move.End, move.Measurement.MediaResumedAt)))
				}
				for _, track := range move.Tracks {
					if track.PacketsAfter == 0 {
						fail("no " + track.Kind + " packets after event")
					}
					if move.Kind == "takeover" {
						if track.Gap >= 2*time.Second {
							fail(track.Kind + " takeover gap >=2s")
						} else if track.Gap+bound >= 2*time.Second {
							inconclusive(fmt.Sprintf("%s takeover gap %s is within caller stall bound %s of 2s", track.Kind, track.Gap, bound))
						}
						continue
					}
					if track.BaselineGapSamples < 10 {
						inconclusive(track.Kind + " has fewer than ten baseline gap samples")
						continue
					}
					gaps[track.Kind] = append(gaps[track.Kind], track.Gap)
					excess[track.Kind] = append(excess[track.Kind], track.Gap-track.BaselineGapP95)
					worstGaps[track.Kind] = append(worstGaps[track.Kind], track.Gap+bound)
					worstExcess[track.Kind] = append(worstExcess[track.Kind], track.Gap-track.BaselineGapP95+bound)
				}
			}
		}
	}
	for kind, values := range gaps {
		if percentile95(values) > 150*time.Millisecond || percentile95(excess[kind]) > 50*time.Millisecond {
			fail(kind + " move p95 exceeds baseline p95 +50ms or 150ms ceiling")
		} else if percentile95(worstGaps[kind]) > 150*time.Millisecond || percentile95(worstExcess[kind]) > 50*time.Millisecond {
			inconclusive(kind + " move p95 is within the caller stall bound of baseline p95 +50ms or 150ms ceiling")
		}
	}
	if len(r.Telemetry.Processes) == 0 || len(r.Telemetry.Errors) > 0 {
		inconclusive("missing or incomplete service resource samples")
	}
	expectedTakeovers := 0
	for i, event := range r.Events {
		if event.Failed && !excluded[i] {
			fail(event.Event.Kind + " confirmed service failure")
		}
		if event.Error != "" && !excluded[i] {
			fail(event.Event.Kind + " event error: " + event.Error)
		}
		if event.Event.Kind == "kill" {
			expectedTakeovers += event.Affected
		}
	}
	takeoverEvidence := false
	for _, p := range r.Telemetry.Processes {
		if p.UnexpectedExit {
			fail(p.Name + " exited unexpectedly: " + strings.Join(p.Errors, "; "))
		}
		if p.Name == "control" {
			first, firstOK := p.First["relais_control_takeovers_total"]
			last, lastOK := p.Last["relais_control_takeovers_total"]
			if firstOK && lastOK && last >= first {
				takeoverEvidence = true
				if last-first != float64(expectedTakeovers) {
					fail(fmt.Sprintf("control takeovers %.0f do not match %d scheduled-kill calls", last-first, expectedTakeovers))
				}
			}
		}
		if p.Name == "relay" {
			if value, ok := p.Last["relais_relay_buffer_enabled"]; ok {
				enabled := value == 1
				s.RelayBufferEnabled = &enabled
			}
		}
		for name, rate := range p.RatesPerCallSecond {
			if strings.HasPrefix(name, "relais_store_commands_total") {
				s.StoreCommandsPerCallSecond += rate
			}
			if strings.HasPrefix(name, "relais_transport_bytes_total{") && strings.Contains(name, `transport="redis"`) {
				s.StoreBytesPerCallSecond += rate
				client := "unlabelled"
				for _, label := range []string{"session", "owners", "frame"} {
					if strings.Contains(name, `client="`+label+`"`) {
						client = label
						break
					}
				}
				s.StoreBytesByClient[client] += rate
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
	if !takeoverEvidence {
		inconclusive("missing control takeover counter window")
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
	// Generator saturation remains authoritative except when independent
	// evidence confirms a crash or a failed drain.
	if r.Outcome != "valid" && !confirmedServiceFailure(r) {
		s.Verdict = "inconclusive"
	}
	return s
}

// A crash is independently confirmed even with an invalid generator. A
// partial drain requires complete, unsaturated callers to establish failure.
func confirmedServiceFailure(r loadgen.RunReport) bool {
	for _, p := range r.Telemetry.Processes {
		if p.UnexpectedExit {
			return true
		}
	}
	failed := false
	for _, event := range r.Events {
		failed = failed || event.Failed
	}
	if !failed || len(r.Processes) == 0 {
		return false
	}
	calls := 0
	for _, p := range r.Processes {
		if p.Saturated || !p.Complete || len(p.Causes) > 0 {
			return false
		}
		calls += len(p.Calls)
	}
	return calls == r.Callers
}

// A scheduled gap must fit the matched event's API/resume interval, with
// one move-ceiling allowance for the bounding packet on either side. Another
// call's event, an unrecorded takeover, or a distant gap cannot excuse it.
func gapOutsideScheduledEvent(events []loadgen.EventResult, session string, report *callharness.Report, end, gap time.Duration) bool {
	return gap > callharness.MediaGapCeiling && unscheduledGap(events, session, report, end, gap)
}

// unscheduledGap applies the same event windows to a gap of any length.
func unscheduledGap(events []loadgen.EventResult, session string, report *callharness.Report, end, gap time.Duration) bool {
	for _, event := range events {
		affected := false
		for _, id := range event.Sessions {
			affected = affected || id == session
		}
		if !affected || event.Error != "" {
			continue
		}
		for _, move := range report.Moves {
			if !loadgen.MatchesEvent(event, report, move) {
				continue
			}
			start := move.Start - 150*time.Millisecond
			finish := max(move.End, move.Measurement.MediaResumedAt)
			if move.Kind == "takeover" {
				// MediaResumedAt is the earliest track. Allow later tracks
				// the takeover row's 2s bound; their gaps are judged above.
				finish = max(finish, move.Start+2*time.Second)
			}
			finish += 150 * time.Millisecond
			if end-gap >= start && end <= finish {
				return false
			}
		}
	}
	return true
}

func recordCapacityStep(dir, output string, result *capacityReport, step capacityStep) (capacityStep, error) {
	if step.Kind == "relay" || step.Kind == "n-plus-one" || step.Kind == "drain" {
		// Judge generator validity as judgeCapacity does; Run stays raw.
		generator, _ := capacityGenerator(step.Run)
		for worker, peak := range step.Run.WorkerPeaks {
			if peak <= result.WorkerCapacityLowerBound {
				continue
			}
			step.Causes = append(step.Causes, fmt.Sprintf("%s observed %d calls exceeds proven worker lower bound %d", worker, peak, result.WorkerCapacityLowerBound))
			if (step.Kind == "relay" || generator.Outcome != "valid") && !confirmedServiceFailure(generator) {
				step.Verdict = "inconclusive"
			} else {
				step.Verdict = "fail"
			}
		}
	}
	result.RelayBufferEnabled = step.RelayBufferEnabled
	result.Steps = append(result.Steps, step)
	return step, errors.Join(writeJSON(filepath.Join(dir, "report.json"), step), writeJSON(output, result))
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
	profile := "loopback"
	if c.Topology == "netns" {
		profile = "lan"
	}
	result := capacityReport{GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, GoVersion: runtime.Version(), HostCPUs: runtime.NumCPU(), Runner: host, Topology: c.Topology, Profile: profile, CallersPerProcess: c.CallersPerProcess, Workers: c.Workers, StoreOpsBudget: c.StoreOpsBudget, StoreBytesBudget: c.StoreBytesBudget, Outcome: "inconclusive", CostStatus: "NOT-RUN(measurement incomplete)", Note: "Bounded short-run lower bounds, not sustained capacity or exact limits. Stop on the first failed or inconclusive step; generator saturation never establishes service capacity. Loopback is Mac/local evidence; netns is emulated unshaped LAN only. Region/far-caller and 60-second consent are not proven by these samples."}
	defer func() { fmt.Println("cost=" + result.CostStatus) }()
	var prices *priceInput
	if c.Prices != "" {
		value, err := readPrices(c.Prices)
		if err != nil {
			result.CostStatus = "NOT-RUN(invalid price inputs)"
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
		if runErr != nil {
			judged.Verdict = "inconclusive"
			judged.Causes = append(judged.Causes, runErr.Error())
		}
		judged, writeErr := recordCapacityStep(dir, c.Output, &result, judged)
		for _, slip := range judged.ToleratedSlips {
			fmt.Println("tolerated:", slip)
		}
		fmt.Printf("capacity kind=%s callers=%d service=%s generator=%s store-commands/call-s=%.2f store-bytes/call-s=%.0f relay-pps=%.1f drain-s=%.3f\n", kind, count, judged.Verdict, judged.GeneratorOutcome, judged.StoreCommandsPerCallSecond, judged.StoreBytesPerCallSecond, judged.RelayPacketsPerSecond, judged.DrainSeconds)
		return judged, writeErr
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
	}
	setCapacityCost(&result, prices)
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
	Validated                      bool       `json:"validated"`
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
func setCapacityCost(result *capacityReport, prices *priceInput) {
	result.Cost = nil
	var relay *capacityStep
	for i := range result.Steps {
		step := &result.Steps[i]
		if step.Kind == "relay" && step.Verdict == "pass" {
			relay = step
		}
	}
	switch {
	case prices == nil:
		result.CostStatus = "NOT-RUN(no input prices)"
	case relay == nil:
		result.CostStatus = "NOT-RUN(no passing relay step)"
	case !result.PlannedValidated || result.PlannedCallers < 2:
		result.CostStatus = "NOT-RUN(planned load not validated)"
	default:
		cost := calculateCost(*prices, result.Workers, result.PlannedCallers, relay.PublicEgressBytesPerCallSecond)
		cost.Validated = true
		result.Cost = &cost
		result.CostStatus = fmt.Sprintf("VALIDATED(%s %.6f/call-hour upper-bound estimate at T=%d)", prices.Currency, cost.TotalPerCallHour, result.PlannedCallers)
	}
}

func calculateCost(p priceInput, workers, callers int, egress float64) costReport {
	cost := costReport{Inputs: p, PlannedCallers: callers, PublicEgressBytesPerCallSecond: egress, Note: "Allocated machine cost at measured candidate N+1 occupancy: W workers + one relay/control/store, plus measured public IPv4 UDP egress at input price. No provider prices, discounts, inter-zone charges, provider agent fees, disk, or tax assumed. Upper-bound estimate at a demonstrated load floor, not an operating recommendation or independent-host sizing proof."}
	cost.MachinePerCallHour = (float64(workers)*p.Worker.Hourly + p.Relay.Hourly + p.Control.Hourly + p.Store.Hourly) / float64(callers)
	cost.TransferPerCallHour = egress * 3600 / (1 << 30) * p.PublicEgressPerGiB
	cost.TotalPerCallHour = cost.MachinePerCallHour + cost.TransferPerCallHour
	return cost
}

func hasDistributedCalls(r loadgen.RunReport) bool {
	if r.Distributed != nil {
		return true
	}
	for _, p := range r.Processes {
		for _, c := range p.Calls {
			if c.Call.HasDistributedEvidence() {
				return true
			}
		}
	}
	return false
}
