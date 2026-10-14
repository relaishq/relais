package main

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/relais/internal/capacity"
	"github.com/relais/internal/clusterprocess"
	"github.com/relais/internal/loadgen"
	"github.com/relais/internal/processrss"
	"github.com/relais/internal/relaylease"
	"github.com/relais/pkg/controlplane"
	"github.com/relais/pkg/mediaworker"
	"github.com/relais/pkg/relay"
	"github.com/relais/pkg/sessionstore"
)

// Schedule workers are cyclic selection offsets into the occupied/live pool.
// Resolving against ownership avoids no-op kills. Both the exact seeded plan
// and the resolved action/session IDs are recorded; resolution is deterministic
// for the same ownership state, and never consumes more random numbers.
func soakSchedule(duration, interval time.Duration, seed int64, workers int) []loadgen.Event {
	if interval <= 0 || workers <= 0 {
		return nil
	}
	rng := rand.New(rand.NewSource(seed)) // Version-stable Go source, isolated from placement/random ports.
	kinds := rng.Perm(3)
	names := []string{"move", "drain", "kill"}
	var events []loadgen.Event
	for at, i := interval, 0; at+7*time.Second < duration; at, i = at+interval, i+1 {
		events = append(events, loadgen.Event{After: at, Kind: names[kinds[i%3]], Worker: rng.Intn(workers), Count: 0})
	}
	return events
}

type memoryVerdict struct {
	Diagnostic          bool    `json:"diagnostic,omitempty"`
	Name                string  `json:"name"`
	PID                 int     `json:"pid"`
	Metric              string  `json:"metric"`
	Outcome             string  `json:"outcome"`
	Samples             int     `json:"samples"`
	RawSamples          int     `json:"raw_samples"`
	LongLived           bool    `json:"long_lived"`
	MedianBytes         float64 `json:"median_bytes"`
	SlopeBytesPerSecond float64 `json:"slope_bytes_per_second"`
	ProjectedHourBytes  float64 `json:"projected_hour_bytes"`
	LimitBytes          float64 `json:"limit_bytes"`
	Reason              string  `json:"reason,omitempty"`
}

func judgeMemory(name string, pid int, metric string, samples []capacity.MemorySample, start time.Time, duration time.Duration, incarnation bool) memoryVerdict {
	v := memoryVerdict{Name: name, PID: pid, Metric: metric, Outcome: "insufficient-data", RawSamples: len(samples), Diagnostic: name == "caller" && metric != "heap-live"}
	v.LongLived = !incarnation || name == "caller" || (len(samples) >= 2 && samples[len(samples)-1].At.Sub(samples[0].At) >= 10*time.Minute)
	warmup := min(duration/10, 5*time.Minute)
	if len(samples) < 2 {
		v.Reason = "fewer than two samples"
		return v
	}
	if incarnation && samples[len(samples)-1].At.Sub(samples[0].At) < 10*time.Minute {
		v.Reason = "incarnation observed for less than ten minutes"
		return v
	}
	// Every incarnation has its own warm-up, using the run's warm-up duration.
	if samples[0].At.After(start.Add(time.Second)) {
		start = samples[0].At
	}
	var xs, ys []float64
	for _, s := range samples {
		if s.At.Before(start.Add(warmup)) || s.At.After(start.Add(duration)) {
			continue
		}
		value := s.RSSBytes
		if metric == "physical-footprint" {
			value = s.PhysicalFootprintBytes
		}
		if metric == "heap-in-use" {
			value = s.HeapInUseBytes
		}
		if metric == "heap-live" {
			value = s.HeapLiveBytes
		}
		if value == 0 {
			continue
		}
		xs = append(xs, s.At.Sub(start).Seconds())
		ys = append(ys, float64(value))
	}
	v.Samples = len(xs)
	if len(xs) < 3 || xs[len(xs)-1] <= xs[0] {
		v.Reason = "fewer than three post-warm-up samples"
		return v
	}
	if xs[len(xs)-1]-xs[0] < (5 * time.Minute).Seconds() {
		v.Reason = "less than five minutes of post-warm-up observations"
		return v
	}
	var mx, my float64
	for i := range xs {
		mx += xs[i]
		my += ys[i]
	}
	mx /= float64(len(xs))
	my /= float64(len(xs))
	var numerator, denominator float64
	for i := range xs {
		numerator += (xs[i] - mx) * (ys[i] - my)
		denominator += (xs[i] - mx) * (xs[i] - mx)
	}
	sort.Float64s(ys)
	v.MedianBytes = ys[len(ys)/2]
	if len(ys)%2 == 0 {
		v.MedianBytes = (ys[len(ys)/2] + ys[len(ys)/2-1]) / 2
	}
	v.SlopeBytesPerSecond = numerator / denominator
	v.ProjectedHourBytes = v.SlopeBytesPerSecond * 3600
	v.LimitBytes = max(.1*v.MedianBytes, 16<<20)
	v.Outcome = "pass"
	if v.ProjectedHourBytes > v.LimitBytes {
		v.Outcome = "fail"
	}
	return v
}

type excludedInterval struct {
	PID        int `json:"pid"`
	Start, End time.Time
	Causes     []string `json:"causes"`
}
type saturationBudget struct {
	LatenessPersistenceSamples int                `json:"lateness_persistence_samples"`
	ImmediateLatenessLimit     time.Duration      `json:"immediate_p99_lateness_limit_ns"`
	Intervals                  int                `json:"one_second_intervals"`
	Saturated                  int                `json:"saturated_one_second_intervals"`
	Percent                    float64            `json:"saturated_percent"`
	LimitPercent               float64            `json:"limit_percent"`
	Outcome                    string             `json:"outcome"`
	Excluded                   []excludedInterval `json:"excluded"`
}

// The denominator is wall-clock seconds, not process-seconds. If several
// generators stall together that second counts once, with every process named.
// A delayed saturated sample excludes every second it overlaps, conservatively.
func judgeSaturation(r loadgen.RunReport) saturationBudget {
	b := saturationBudget{LatenessPersistenceSamples: 2, ImmediateLatenessLimit: 2 * r.Limits.P99, LimitPercent: 1, Outcome: "pass", Intervals: int((r.Duration + time.Second - 1) / time.Second)}
	seen := map[int]bool{}
	end := r.StartedAt.Add(r.Duration)
	for _, p := range r.Processes {
		for i, s := range p.Samples {
			first, last := maxTime(s.At.Add(-s.Interval), r.StartedAt), minTime(s.At, end)
			if !last.After(first) {
				continue
			}
			late := soakLatenessSaturated(p.Samples, i, r.Limits.P99, r.StartedAt, end)
			if !late && s.IntervalPacing.SkippedPercent() <= r.Limits.SkippedPercent && s.CPUPerGOMAXPROCS <= r.Limits.CPUPercent {
				continue
			}
			// Name lateness only when it excludes this sample by itself.
			limits := r.Limits
			if !late {
				limits.P99 = max(limits.P99, s.IntervalLateness.P99)
			}
			causes := limits.Causes(s)
			if late && s.IntervalLateness.P99 <= b.ImmediateLatenessLimit {
				causes = append(causes, "send-lateness exceeds limit in consecutive samples")
			}
			b.Excluded = append(b.Excluded, excludedInterval{PID: p.PID, Start: first, End: last, Causes: causes})
			for i := int(first.Sub(r.StartedAt) / time.Second); i < b.Intervals && r.StartedAt.Add(time.Duration(i)*time.Second).Before(last); i++ {
				seen[i] = true
			}
		}
	}
	b.Saturated = len(seen)
	if b.Intervals > 0 {
		b.Percent = 100 * float64(b.Saturated) / float64(b.Intervals)
	}
	if b.Intervals == 0 || b.Percent > b.LimitPercent {
		b.Outcome = "inconclusive"
	}
	return b
}

// A modest, isolated tick slip is not evidence of sustained generator overload.
// Retain both samples of a consecutive excess, including the first second of
// an event. A slip beyond two audio ticks, skipped frames, and CPU excess still
// exclude immediately. This policy is soak-only; raw limits/samples and the
// calibration/capacity gates remain unchanged.
func soakLatenessSaturated(samples []loadgen.Stats, i int, limit time.Duration, start, end time.Time) bool {
	s := samples[i]
	if s.IntervalLateness.P99 <= limit {
		return false
	}
	if s.IntervalLateness.P99-limit > limit {
		return true
	}
	for _, j := range []int{i - 1, i + 1} {
		if j < 0 || j >= len(samples) || samples[j].IntervalLateness.P99 <= limit {
			continue
		}
		// Startup or hangup-only lateness cannot turn a media slip into overload.
		if !samples[j].At.After(start) || !samples[j].At.Add(-samples[j].Interval).Before(end) {
			continue
		}
		previous, next := s, samples[j]
		if j < i {
			previous, next = samples[j], s
		}
		// Samples must cover consecutive time, not just be adjacent entries in
		// an incomplete report. Permit only timestamp rounding/clock noise.
		gap := next.At.Add(-next.Interval).Sub(previous.At)
		if previous.Interval > 0 && next.Interval > 0 && next.At.After(previous.At) && gap >= -time.Millisecond && gap <= time.Millisecond {
			return true
		}
	}
	return false
}
func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// The event window includes each affected caller's baseline and the complete
// recovery/freshness observation horizon used by its saved yardstick.
func eventWindow(r loadgen.RunReport, e loadgen.EventResult) (time.Time, time.Time) {
	first, last := e.At.Add(-time.Second), e.FinishedAt.Add(7*time.Second)
	for _, p := range r.Processes {
		for _, c := range p.Calls {
			if c.Call == nil {
				continue
			}
			for _, move := range c.Call.Moves {
				if loadgen.MatchesEvent(e, c.Call, move) {
					first = minTime(first, c.Call.StartedAt.Add(move.Start).Add(-time.Second))
					last = maxTime(last, c.Call.StartedAt.Add(max(move.End, move.Measurement.MediaResumedAt)).Add(7*time.Second))
					m := move.Measurement
					observed := max(m.ObservedUntil, m.SettledAt, m.Audio.EvaluationEnd, m.Video.EvaluationEnd)
					last = maxTime(last, c.Call.StartedAt.Add(observed))
				}
			}
		}
	}
	return first, last
}
func eventSaturated(r loadgen.RunReport, e loadgen.EventResult, b saturationBudget) bool {
	first, last := eventWindow(r, e)
	for _, s := range b.Excluded {
		if s.Start.Before(last) && s.End.After(first) {
			return true
		}
	}
	return false
}

type soakEventVerdict struct {
	Index     int                 `json:"index"`
	Kind      string              `json:"kind"`
	Causes    []string            `json:"causes"`
	Outcome   string              `json:"outcome"`
	Saturated bool                `json:"saturated"`
	Rows      []loadgen.MetricRow `json:"rows"`
}
type soakReport struct {
	SentinelWorker         string                `json:"sentinel_worker"`
	WorkerTrends           []recycledWorkerTrend `json:"recycled_worker_trends"`
	GOOS                   string                `json:"goos"`
	GOARCH                 string                `json:"goarch"`
	GoVersion              string                `json:"go_version"`
	Runner                 string                `json:"runner"`
	Topology               string                `json:"topology"`
	Video                  bool                  `json:"video"`
	GOMAXPROCS             int                   `json:"gomaxprocs"`
	Seed                   int64                 `json:"seed"`
	Schedule               []loadgen.Event       `json:"schedule"`
	WorkerFloor            int                   `json:"worker_floor"`
	RelayFloor             int                   `json:"relay_floor"`
	Workers                int                   `json:"workers"`
	Processes              int                   `json:"generator_processes"`
	CallersPerProcess      int                   `json:"callers_per_process"`
	Interval               time.Duration         `json:"interval_ns"`
	FullScale              bool                  `json:"full_scale"`
	Acceptance             bool                  `json:"acceptance"`
	AcceptanceReasons      []string              `json:"acceptance_reasons"`
	ConclusiveEvents       int                   `json:"conclusive_events"`
	ConclusiveEventPercent float64               `json:"conclusive_event_percent"`
	Saturation             saturationBudget      `json:"saturation"`
	Memory                 []memoryVerdict       `json:"memory"`
	Events                 []soakEventVerdict    `json:"events"`
	Delivery               soakDelivery          `json:"delivery"`
	KernelUDP              kernelUDPDrops        `json:"kernel_udp"`
	Outcome                string                `json:"outcome"`
	Causes                 []string              `json:"causes"`
	Run                    loadgen.RunReport     `json:"run"`
}

func leakCauses(l loadgen.LeakReport) []string {
	var causes []string
	if !l.Checked {
		causes = append(causes, "leak checks incomplete")
	}
	for name, n := range l.Sessions {
		if strings.HasPrefix(name, "relay-") && (l.RelayWait < soakRelayWait || l.RelayObservedAt.IsZero()) {
			continue
		}
		if n != 0 {
			causes = append(causes, fmt.Sprintf("%s retains %d sessions", name, n))
		}
	}
	for _, key := range l.Keys {
		if key.Error != "" {
			continue
		}
		if !boundedSoakKey(key) {
			causes = append(causes, fmt.Sprintf("Redis key %s class=%s has unbounded retention (TTL=%dms)", key.Key, key.Class, key.TTLMS))
		}
	}
	if len(l.Processes) > 0 {
		causes = append(causes, "owned processes remain")
	}
	if len(l.Listeners) > 0 {
		causes = append(causes, "owned listeners remain")
	}
	return append(causes, l.Errors...)
}

func judgeSoak(c config, r loadgen.RunReport) soakReport {
	host, _ := os.Hostname()
	result := soakReport{GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, GoVersion: runtime.Version(), Runner: host, Topology: c.Topology, Video: c.Video, GOMAXPROCS: c.GOMAXPROCS, Seed: c.Seed, Schedule: c.Events, Workers: c.Workers, Processes: c.Processes, CallersPerProcess: c.CallersPerProcess, Interval: c.ChaosInterval, WorkerFloor: c.WorkerFloor, RelayFloor: c.RelayFloor, FullScale: c.Callers >= 50 && c.Duration >= time.Hour, Outcome: "pass", Run: r}
	if hasDistributedCalls(r) {
		result.Outcome = "inconclusive"
		result.Causes = []string{"local soak policy refuses distributed point estimates"}
		return result
	}
	inconclusive := func(reason string) {
		if result.Outcome != "fail" {
			result.Outcome = "inconclusive"
		}
		result.Causes = append(result.Causes, reason)
	}
	fail := func(reason string) { result.Outcome = "fail"; result.Causes = append(result.Causes, reason) }
	result.SentinelWorker = r.SentinelWorker
	result.WorkerTrends = recycledWorkerTrends(r.Telemetry.Processes, r.SentinelWorker)
	result.Saturation = judgeSaturation(r)
	// Rejudge a copy without process-wide saturation. Its intervals remain intact
	// in the original report, and the budget/event windows control validity here.
	usable := r
	usable.Processes = append([]loadgen.ProcessReport(nil), r.Processes...)
	for i := range usable.Processes {
		usable.Processes[i].Saturated = false
	}
	usable.Events = append([]loadgen.EventResult(nil), r.Events...)
	usable.Judge()
	// Preserve execution errors that were added after RunReport.Judge. Known
	// event evidence and process saturation are evaluated by their own gates.
	expected := r
	expected.Events = append([]loadgen.EventResult(nil), r.Events...)
	expected.Judge()
	known := map[string]bool{}
	for _, cause := range expected.Causes {
		known[cause] = true
	}
	for _, cause := range r.Causes {
		if !known[cause] {
			inconclusive(cause)
		}
	}
	for i, e := range usable.Events {
		v := soakEventVerdict{Index: i, Kind: e.Event.Kind, Outcome: "pass", Saturated: eventSaturated(r, e, result.Saturation)}
		rows := append([]loadgen.MetricRow(nil), usable.Yardstick...)
		rows = append(rows, soakGapRows(usable, e, i)...)
		for _, row := range rows {
			if row.EventIndex == i {
				if v.Saturated {
					row.Outcome = "inconclusive"
				} // Retain Trusted/Failed counts as raw evidence.
				v.Rows = append(v.Rows, row)
				if row.Outcome == "fail" {
					v.Outcome = "fail"
				} else if row.Outcome != "pass" && v.Outcome != "fail" {
					v.Outcome = "inconclusive"
				}
				if row.Outcome != "pass" && !v.Saturated {
					v.Causes = append(v.Causes, row.Metric+" "+row.Outcome)
				}
			}
		}
		if e.Failed {
			v.Outcome = "fail"
			v.Causes = append(v.Causes, "confirmed event failure")
		}
		if e.Error != "" {
			v.Causes = append(v.Causes, "action failed: "+e.Error)
			if !v.Saturated {
				v.Outcome = "fail"
			}
		}
		if v.Saturated {
			if v.Outcome != "fail" {
				v.Outcome = "inconclusive"
			}
			v.Causes = append(v.Causes, "event window overlaps generator saturation")
		}
		if len(v.Rows) == 0 {
			v.Causes = append(v.Causes, "no recovery rows")
			if v.Outcome != "fail" {
				v.Outcome = "inconclusive"
			}
		}
		ids := map[string]bool{}
		for _, id := range e.Sessions {
			ids[id] = true
		}
		if e.Recorded != e.Affected || len(ids) != e.Affected || len(ids) != len(e.Sessions) {
			v.Causes = append(v.Causes, fmt.Sprintf("incomplete or duplicate affected-call evidence: %d/%d recorded", e.Recorded, e.Affected))
			if v.Outcome != "fail" {
				v.Outcome = "inconclusive"
			}
		}
		result.Events = append(result.Events, v)
		if v.Outcome == "fail" {
			fail(fmt.Sprintf("event %d failed caller recovery rows", i))
		}
	}
	// Event coverage is judged separately. Capacity still checks whole-call
	// continuity, counters and resources, while inconclusive event evidence is
	// omitted from its aggregate rows/move samples. Raw evidence remains in Run.
	excluded := map[int]bool{}
	for i, e := range result.Events {
		excluded[i] = e.Outcome == "inconclusive"
	}
	// RunReport's event matching causes belong to individual event coverage.
	// Rebuild only generator completeness here; keep the complete event/row copy
	// for the independent continuity, takeover-counter and resource checks.
	generatorValidity := usable
	generatorValidity.Events = nil
	generatorValidity.Judge()
	usable.Outcome, usable.Causes = generatorValidity.Outcome, generatorValidity.Causes
	step := judgeCapacityExcept("soak", usable, c.StoreOpsBudget, c.StoreBytesBudget, excluded)
	if step.Verdict == "fail" {
		for _, cause := range step.Causes {
			fail(cause)
		}
	} else if step.Verdict != "pass" {
		for _, cause := range step.Causes {
			inconclusive(cause)
		}
	}
	passes := map[string]bool{}
	for _, e := range result.Events {
		if e.Outcome != "inconclusive" {
			result.ConclusiveEvents++
		}
		if e.Outcome == "pass" {
			passes[e.Kind] = true
		}
	}
	if len(result.Events) > 0 {
		result.ConclusiveEventPercent = 100 * float64(result.ConclusiveEvents) / float64(len(result.Events))
	}
	if len(result.Events) == 0 || result.ConclusiveEvents*100 < len(result.Events)*95 {
		inconclusive(fmt.Sprintf("conclusive event coverage %.3f%% is below 95%%", result.ConclusiveEventPercent))
	}
	for _, kind := range []string{"move", "drain", "kill"} {
		if !passes[kind] {
			inconclusive("no conclusive " + kind + " pass")
		}
	}
	if result.Saturation.Outcome != "pass" {
		inconclusive(fmt.Sprintf("generator saturation %.3f%% exceeds 1%% run budget", result.Saturation.Percent))
	}
	if len(r.Events) != len(c.Events) {
		inconclusive("chaos schedule incomplete")
	}
	addMemory := func(name string, pid int, samples []capacity.MemorySample, worker bool, heap bool, observedSeconds float64) {
		memoryMetric := r.MemoryMetric
		if memoryMetric == "" {
			memoryMetric = "rss"
		}
		metrics := []string{memoryMetric}
		if heap {
			metrics = append(metrics, "heap-in-use")
		}
		if name == "caller" {
			metrics = []string{"heap-live", "rss", "physical-footprint", "heap-in-use"}
		}
		for _, metric := range metrics {
			v := judgeMemory(name, pid, metric, samples, r.StartedAt, r.Duration, worker)
			if name == r.SentinelWorker || worker && observedSeconds >= (10*time.Minute).Seconds() {
				v.LongLived = true
			}
			result.Memory = append(result.Memory, v)
			if v.Outcome == "fail" && !v.Diagnostic {
				fail(fmt.Sprintf("%s pid=%d %s projects %.0f bytes/hour above %.0f", name, pid, metric, v.ProjectedHourBytes, v.LimitBytes))
			}
		}
	}
	for _, p := range r.Telemetry.Processes {
		worker := strings.HasPrefix(p.Name, "worker-")
		addMemory(p.Name, p.PID, p.Memory, worker, p.Name != "store", p.ObservedSeconds)
		if worker && p.Peaks["relais_worker_active_sessions"] > float64(c.WorkerFloor) {
			fail(p.Name + " exceeded measured worker floor")
		}
		if p.Name == "control" {
			for name, value := range p.Last {
				if (name == "relais_control_losses_total" || strings.HasPrefix(name, "relais_control_losses_total{")) && value > 0 {
					fail("control reports unintended lost calls")
					break
				}
			}
		}
		// Counters are absent from Peaks; inspect the last raw counter too.
		if worker && p.Last["relais_worker_decryption_failures_total"] > 0 {
			fail(p.Name + " worker decryption failures")
		}
	}
	for _, p := range r.Processes {
		var samples []capacity.MemorySample
		for _, s := range p.Samples {
			sample := capacity.MemorySample{At: s.At, RSSBytes: s.RSSBytes, PhysicalFootprintBytes: s.PhysicalFootprintBytes, HeapInUseBytes: s.HeapInUseBytes}
			if s.RuntimeMemory != nil {
				sample.HeapLiveBytes = s.RuntimeMemory.HeapLiveBytes
			}
			samples = append(samples, sample)
		}
		addMemory("caller", p.PID, samples, true, true, 0)
		for _, m := range p.Calls {
			if m.Call != nil {
				for _, cause := range soakContinuityCauses(r, p, m, result.Saturation) {
					if cause.Failed {
						fail(cause.Reason)
					} else {
						inconclusive(cause.Reason)
					}
				}
			}
			if m.Call == nil {
				continue
			}
			if m.Call.StartedAt.Add(m.Call.HungUpAt).Before(r.StartedAt.Add(r.Duration)) {
				fail("caller ended before intended media lifetime")
			}
			if m.Call.DecryptionFailures.Total() > 0 {
				fail("caller decryption failures")
			}
		}
	}
	// Whole-call delivery covers loss the event windows do not (#134).
	result.Delivery = judgeDelivery(r, result.Saturation)
	failures, unknown := result.Delivery.causes()
	for _, cause := range failures {
		fail(cause)
	}
	for _, cause := range unknown {
		inconclusive(cause)
	}
	leaks := r.Leaks
	leaks.Checked = true
	leaks.Errors = nil
	for _, cause := range leakCauses(leaks) {
		fail(cause)
	}
	for name := range r.Leaks.Sessions {
		if strings.HasPrefix(name, "relay-") && (r.Leaks.RelayWait < soakRelayWait || r.Leaks.RelayObservedAt.IsZero()) {
			inconclusive("relay cleanup not observed after inactivity timeout")
			break
		}
	}
	if len(r.Leaks.StoreKeys) != len(r.Leaks.Keys) {
		inconclusive("Redis retention observations missing")
	}
	for _, control := range []string{"session_epoch", "relay-fencing-hash"} {
		if !r.Leaks.ScanControls[control] {
			inconclusive("Redis scan positive control missing: " + control)
		}
	}
	if !r.Leaks.Checked || len(r.Leaks.Errors) > 0 {
		inconclusive("leak checks incomplete: " + strings.Join(r.Leaks.Errors, "; "))
	}
	// Crash and partial-drain proof uses #40's independent failure rule.
	if confirmedServiceFailure(r) {
		fail("independently confirmed service failure")
	}
	result.judgeAcceptance()
	return result
}

func (r *soakReport) judgeAcceptance() {
	r.Acceptance = false
	r.AcceptanceReasons = nil
	if !r.FullScale {
		r.AcceptanceReasons = append(r.AcceptanceReasons, "requires at least 50 concurrent calls for at least one hour")
	}
	if r.Outcome != "pass" {
		r.AcceptanceReasons = append(r.AcceptanceReasons, "run outcome is "+r.Outcome)
	}
	if len(r.Memory) == 0 {
		r.AcceptanceReasons = append(r.AcceptanceReasons, "no process memory verdicts")
	}
	roles := map[string]bool{}
	callerPIDs := map[int]bool{}
	for _, p := range r.Run.Processes {
		callerPIDs[p.PID] = false
	}
	for _, m := range r.Memory {
		if m.Name == "caller" {
			callerPIDs[m.PID] = callerPIDs[m.PID] || (m.Metric == "heap-live" && !m.Diagnostic && m.LongLived && m.Outcome == "pass")
			if m.Metric != "heap-live" {
				continue
			}
		}
		if m.Diagnostic {
			continue
		}
		role := m.Name
		if strings.HasPrefix(role, "worker-") {
			role = "worker"
		}
		roles[role] = true
		if m.LongLived && m.Outcome != "pass" {
			r.AcceptanceReasons = append(r.AcceptanceReasons, fmt.Sprintf("%s pid=%d %s: %s", m.Name, m.PID, m.Metric, m.Outcome))
		}
	}
	for _, role := range []string{"relay", "control", "store", "caller", "worker"} {
		if !roles[role] {
			r.AcceptanceReasons = append(r.AcceptanceReasons, "missing "+role+" memory verdicts")
		}
	}
	var callers []int
	for pid := range callerPIDs {
		callers = append(callers, pid)
	}
	sort.Ints(callers)
	for _, pid := range callers {
		if !callerPIDs[pid] {
			r.AcceptanceReasons = append(r.AcceptanceReasons, fmt.Sprintf("caller pid=%d live heap memory unproven", pid))
		}
	}
	workerMetrics := map[string]bool{}
	workerPIDs := map[int]bool{}
	for _, m := range r.Memory {
		if r.SentinelWorker != "" && m.Name == r.SentinelWorker && !m.Diagnostic && m.LongLived && m.Outcome == "pass" {
			workerMetrics[m.Metric] = true
			workerPIDs[m.PID] = true
		}
	}
	metric := r.Run.MemoryMetric
	if metric == "" {
		metric = "rss"
	}
	if !workerMetrics[metric] || !workerMetrics["heap-in-use"] || len(workerPIDs) != 1 {
		r.AcceptanceReasons = append(r.AcceptanceReasons, "worker memory unproven")
	}
	r.Acceptance = len(r.AcceptanceReasons) == 0
}

// Each service incarnation gets one monitor. No counters or memory trends
// cross PID boundaries. Unchanged roles keep a single uninterrupted window.
type soakExecution struct {
	ctx      context.Context
	every    time.Duration
	active   map[string]*capacity.Monitor
	data     capacity.Telemetry
	finished bool
}

func startSoakExecution(ctx context.Context, t *topology, every time.Duration) *soakExecution {
	s := &soakExecution{ctx: ctx, every: every, active: map[string]*capacity.Monitor{}}
	for _, target := range t.Targets {
		s.add(target)
	}
	return s
}
func (s *soakExecution) add(target capacity.Target) {
	s.active[target.Name] = capacity.StartSoak(s.ctx, []capacity.Target{target}, s.every)
}
func (s *soakExecution) collect(name string) {
	m := s.active[name]
	if m == nil {
		return
	}
	telemetry := m.Finish(1)
	// Finish(1) rates are converted below to the common run denominator.
	for i := range telemetry.Processes {
		for metric, rate := range telemetry.Processes[i].RatesPerCallSecond {
			telemetry.Processes[i].RatesPerCallSecond[metric] = rate * telemetry.WindowSeconds
		}
	}
	s.data.Processes = append(s.data.Processes, telemetry.Processes...)
	s.data.Errors = append(s.data.Errors, telemetry.Errors...)
	delete(s.active, name)
}
func (s *soakExecution) retire(name string) { s.collect(name) }
func (s *soakExecution) finish(callers int, duration time.Duration) capacity.Telemetry {
	if s.finished {
		return s.data
	}
	names := make([]string, 0, len(s.active))
	for name := range s.active {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		s.collect(name)
	}
	s.data.WindowSeconds = duration.Seconds()
	for i := range s.data.Processes {
		p := &s.data.Processes[i]
		for metric, delta := range p.RatesPerCallSecond {
			p.RatesPerCallSecond[metric] = delta / (float64(callers) * duration.Seconds())
		}
	}
	s.finished = true
	return s.data
}

func resolveSoakEvent(ctx context.Context, t *topology, e loadgen.Event, floor int) (loadgen.Event, error) {
	var status controlplane.Status
	t.statusRequests++
	if err := request(ctx, http.MethodGet, strings.TrimSuffix(t.External.SignalingURL, "/calls")+"/status", nil, &status); err != nil {
		return e, err
	}
	if status.LostCount != 0 {
		return e, fmt.Errorf("control reports %d lost calls", status.LostCount)
	}
	var live, occupied []controlplane.WorkerStatus
	for _, w := range status.Workers {
		if !w.Dead && !w.Draining && !w.Recovering {
			live = append(live, w)
			if w.Calls > 0 && (e.Kind == "move" || w.Name != strconv.Itoa(t.Sentinel)) {
				occupied = append(occupied, w)
			}
			if w.Calls > floor {
				return e, errors.New("worker exceeds measured floor before chaos")
			}
		}
	}
	if len(live) != len(t.Workers) || len(occupied) == 0 {
		return e, errors.New("N+1 pool not fully ready for next event")
	}
	sort.Slice(live, func(i, j int) bool {
		if live[i].Calls != live[j].Calls {
			return live[i].Calls < live[j].Calls
		}
		return live[i].Name < live[j].Name
	})
	sort.Slice(occupied, func(i, j int) bool { return occupied[i].Name < occupied[j].Name })
	selected := occupied[e.Worker%len(occupied)]
	if e.Kind == "move" {
		selected = live[0]
		if selected.Calls >= floor {
			return e, errors.New("no move slot below worker floor")
		}
		e.Count = 1
	}
	e.Worker, _ = strconv.Atoi(selected.Name)
	return e, nil
}

func (t *topology) recycle(ctx context.Context, id int) error {
	if id == t.Sentinel {
		return errors.New("cannot recycle the memory sentinel")
	}
	name := fmt.Sprintf("worker-%d", id)
	launch := t.Launch[id]
	if launch == nil {
		return errors.New("missing owned worker launch specification")
	}
	if t.Soak != nil {
		t.Soak.retire(name)
	}
	t.Workers[id].Stop()
	deadline, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	controlURL := strings.TrimSuffix(t.External.SignalingURL, "/calls")
	// Wait for the real detector and all old leases to settle before replacement.
	for {
		var status controlplane.Status
		t.statusRequests++
		if err := request(deadline, http.MethodGet, controlURL+"/status", nil, &status); err != nil {
			return err
		}
		ready := false
		for _, w := range status.Workers {
			if w.Name == strconv.Itoa(id) && w.Dead && !w.Recovering && w.Calls == 0 {
				ready = true
			}
		}
		if ready {
			break
		}
		if err := waitUntil(deadline, time.Now().Add(50*time.Millisecond)); err != nil {
			return err
		}
	}
	if err := request(deadline, http.MethodPost, controlURL+"/private/workers/"+strconv.Itoa(id)+"/replace", nil, nil); err != nil {
		return err
	}
	oldPID := t.Workers[id].PID()
	startedAt := time.Now()
	launch.Incarnation++
	dir := filepath.Join(t.Dir, fmt.Sprintf("%s-incarnation-%03d", name, launch.Incarnation))
	if err := os.Mkdir(dir, 0700); err != nil {
		return err
	}
	child, err := startRole(t.Manager, t.Network, dir, name, launch.Env, launch.Args...)
	if err != nil {
		return err
	}
	t.processes = append(t.processes, child)
	t.Workers[id] = child
	rejoinIndex := len(t.Rejoins)
	t.Rejoins = append(t.Rejoins, loadgen.WorkerRejoin{Worker: name, OldPID: oldPID, PID: child.PID(), StartedAt: startedAt})
	ready, err := child.Ready(deadline)
	if err != nil {
		return err
	}
	if ready.HTTP != launch.URL {
		return errors.New("replacement changed registered worker endpoint")
	}
	// HTTP readiness does not prove membership. Require a fresh heartbeat
	// after launch, and a live registration reached through the token handshake.
	for {
		var status controlplane.Status
		t.statusRequests++
		if err := request(deadline, http.MethodGet, controlURL+"/status", nil, &status); err != nil {
			return err
		}
		joined := false
		for _, worker := range status.Workers {
			if worker.Name == strconv.Itoa(id) && !worker.Dead && !worker.Draining && !worker.Recovering && worker.LastHeartbeat.After(startedAt) {
				t.Rejoins[rejoinIndex].CompletedAt = time.Now()
				t.Rejoins[rejoinIndex].LastHeartbeat = worker.LastHeartbeat
				joined = true
			}
		}
		if joined {
			break
		}
		if err := waitUntil(deadline, time.Now().Add(50*time.Millisecond)); err != nil {
			return err
		}
	}
	delete(t.Killed, id)
	target := capacity.Target{Name: name, PID: child.PID(), URL: ready.HTTP, Done: child.Done(), Err: child.Err}
	t.Targets = append(t.Targets, target)
	t.Soak.add(target)
	return nil
}

func (t *topology) checkSoakLeaks(generators []generator) loadgen.LeakReport {
	l := loadgen.LeakReport{Sessions: map[string]int{}}
	ctx, cancel := context.WithTimeout(context.Background(), soakRelayWait+15*time.Second)
	defer cancel()
	recordError := func(err error) {
		if err != nil {
			l.Errors = append(l.Errors, err.Error())
		}
	}
	var status controlplane.Status
	recordError(request(ctx, http.MethodGet, strings.TrimSuffix(t.External.SignalingURL, "/calls")+"/status", nil, &status))
	l.Sessions["control"] = len(status.Calls)
	if status.LostCount != 0 {
		l.Errors = append(l.Errors, fmt.Sprintf("control lost %d calls", status.LostCount))
	}
	// execute waits for every caller report (including hangup) before this check.
	// Keep the relay alive through its inactivity timeout and sweep margin.
	waitAt := time.Now()
	timer := time.NewTimer(soakRelayWait)
	select {
	case <-timer.C:
		l.RelayWait = time.Since(waitAt)
	case <-ctx.Done():
		timer.Stop()
		recordError(ctx.Err())
	}
	endpoints := map[string]string{}
	for _, target := range t.Targets {
		if strings.HasPrefix(target.Name, "worker-") {
			// Only current incarnations still expose status; retired PIDs remain in
			// the process inventory checked after all shutdowns.
			id, _ := strconv.Atoi(strings.TrimPrefix(target.Name, "worker-"))
			if t.Workers[id].PID() != target.PID {
				continue
			}
			var worker mediaworker.WorkerStatus
			recordError(request(ctx, http.MethodGet, target.URL+"/status", nil, &worker))
			l.Sessions[target.Name] = worker.Sessions
		}
		if target.Name == "relay" {
			l.RelayObservedAt = time.Now()
			var relayStatus relay.RelayStatus
			recordError(request(ctx, http.MethodGet, target.URL+"/status", nil, &relayStatus))
			l.Sessions["relay-buffer"] = relayStatus.Stats.BufferedSessions
			l.Sessions["relay-holds"] = relayStatus.Stats.Holds
			l.Sessions["relay-routes"] = relayStatus.Stats.Flows
			l.Sessions["relay-pending-routes"] = relayStatus.Stats.PendingFlows
		}
		if target.URL != "" {
			endpoints[strings.TrimPrefix(target.URL, "http://")] = "tcp"
		}
	}
	endpoints[t.RedisAddr] = "tcp"
	endpoints[t.External.RelayAddr.String()] = "udp"
	for _, launch := range t.Launch {
		for i, arg := range launch.Args {
			if i > 0 && (launch.Args[i-1] == "-media" || launch.Args[i-1] == "-relay-leg") {
				endpoints[arg] = "udp"
			}
		}
	}
	children := append([]*clusterprocess.Child(nil), t.processes...)
	for _, g := range generators {
		g.child.Stop()
		children = append(children, g.child)
		endpoints[strings.TrimPrefix(g.url, "http://")] = "tcp"
	}
	// Shut down services, but keep the owned Redis alive for an actual prefix
	// SCAN and TTL checks. Never delete keys before this observation or wait
	// for store TTLs.
	for i := len(t.processes) - 1; i >= 1; i-- {
		t.processes[i].Stop()
	}
	client := redis.NewClient(&redis.Options{Addr: t.RedisAddr, Password: t.RedisPassword, MaxRetries: -1, ReadTimeout: time.Second, WriteTimeout: time.Second})
	keys, err := scanSoakKeys(ctx, client, t.Prefix)
	l.StoreKeys = keys
	l.ScanControls = soakScanControls(keys, t.Prefix)
	for name, found := range l.ScanControls {
		if !found {
			l.Errors = append(l.Errors, "Redis scan positive control missing: "+name)
		}
	}
	recordError(err)
	for _, key := range keys {
		observation, err := observeSoakKey(ctx, client, t.Prefix, key)
		if err != nil {
			observation.Error = err.Error()
		}
		l.Keys = append(l.Keys, observation)
		recordError(err)
	}
	recordError(client.Close())
	recordError(t.close())
	l.Processes, err = soakProcesses(children)
	recordError(err)
	if t.Network == nil {
		for addr, network := range endpoints {
			if network == "tcp" {
				listener, err := net.Listen("tcp4", addr)
				if err == nil {
					recordError(listener.Close())
				} else {
					l.Listeners = append(l.Listeners, network+":"+addr)
					recordError(err)
				}
			} else {
				socket, err := net.ListenPacket("udp4", addr)
				if err == nil {
					recordError(socket.Close())
				} else {
					l.Listeners = append(l.Listeners, network+":"+addr)
					recordError(err)
				}
			}
		}
	} else {
		// Successful owned namespace deletion removes every listener in those
		// isolated namespaces. Host caller listeners still need a bind check.
		for _, g := range generators {
			addr := strings.TrimPrefix(g.url, "http://")
			listener, err := net.Listen("tcp4", addr)
			if err == nil {
				recordError(listener.Close())
			} else {
				l.Listeners = append(l.Listeners, "tcp:"+addr)
				recordError(err)
			}
		}
	}
	sort.Strings(l.StoreKeys)
	sort.Strings(l.Listeners)
	sort.Ints(l.Processes)
	l.Checked = len(l.Errors) == 0
	return l
}

func runSoak(ctx context.Context, m *clusterprocess.Manager, c config, root string) error {
	fmt.Printf("soak sentinel-worker=worker-0 memory-metric=%s\n", processrss.TrendMetric())
	fmt.Printf("soak seed=%d calls=%d processes=%d interval=%s duration=%s worker-floor=%d relay-floor=%d\n", c.Seed, c.Callers, c.Processes, c.ChaosInterval, c.Duration, c.WorkerFloor, c.RelayFloor)
	for i, e := range c.Events {
		fmt.Printf("schedule index=%d after=%s kind=%s selection-offset=%d\n", i, e.After, e.Kind, e.Worker)
	}
	runCtx, cancel := context.WithTimeout(ctx, 4*time.Minute+c.Duration)
	defer cancel()
	kernelStart, kernelStartErr := sampleKernelUDP()
	report, runErr := execute(runCtx, m, c, root)
	kernelEnd, kernelEndErr := sampleKernelUDP()
	result := judgeSoak(c, report)
	result.KernelUDP = kernelUDPDelta(runtime.GOOS, c.Topology, kernelStart, kernelEnd, kernelStartErr, kernelEndErr)
	if runErr != nil {
		result.Causes = append(result.Causes, runErr.Error())
		if result.Outcome != "fail" {
			result.Outcome = "inconclusive"
		}
	}
	result.judgeAcceptance() // execute errors can change the final outcome.
	printReport(report)
	printSoakVerdict(result, c.Output)
	writeErr := writeJSON(c.Output, result)
	if result.Outcome != "pass" {
		return errors.Join(runErr, writeErr, errors.New("soak did not pass; see report"))
	}
	return errors.Join(runErr, writeErr)
}

func scanSoakKeys(ctx context.Context, client *redis.Client, prefix string) ([]string, error) {
	if prefix == "" {
		return nil, errors.New("empty owned Redis prefix")
	}
	pattern := strings.NewReplacer("\\", "\\\\", "*", "\\*", "?", "\\?", "[", "\\[", "]", "\\]").Replace(prefix) + "*"
	var result []string
	var cursor uint64
	for {
		keys, next, err := client.Scan(ctx, cursor, pattern, 128).Result()
		if err != nil {
			return result, err
		}
		result = append(result, keys...)
		cursor = next
		if cursor == 0 {
			break
		}
	}
	sort.Strings(result)
	return result, nil
}

// Probe both owned leaders and their groups to detect surviving descendants.
func soakProcesses(children []*clusterprocess.Child) ([]int, error) {
	var remaining []int
	var problems error
	for _, child := range children {
		for _, pid := range []int{child.PID(), -child.PID()} {
			err := syscall.Kill(pid, 0)
			if err == nil || errors.Is(err, syscall.EPERM) {
				remaining = append(remaining, pid)
			} else if !errors.Is(err, syscall.ESRCH) {
				problems = errors.Join(problems, err)
			}
		}
	}
	return remaining, problems
}

// soakAudioTick matches the caller's unexported 20 ms Opus send interval.
const soakAudioTick = 20 * time.Millisecond

// Apply #40's interruption rows to each event, in addition to whole-run p95.
// A single slow event must not disappear inside hours of other successful moves.
func soakGapRows(r loadgen.RunReport, e loadgen.EventResult, index int) []loadgen.MetricRow {
	rows := map[string]*loadgen.MetricRow{}
	gaps, excess := map[string][]time.Duration{}, map[string][]time.Duration{}
	// Worst cases add each track's stall bound; unaffected tracks are unchanged.
	worstGaps, worstExcess := map[string][]time.Duration{}, map[string][]time.Duration{}
	for _, p := range r.Processes {
		for _, c := range p.Calls {
			if c.Call == nil {
				continue
			}
			for _, move := range c.Call.Moves {
				if loadgen.MatchesEvent(e, c.Call, move) {
					// A caller stall that covers the last packet before an outage is
					// stamped late after ReadRTP, so it can shorten the measured gap.
					// With GOMAXPROCS=1 a process stall of D delays the next send by at
					// least D minus one tick, so D <= interval max lateness + one tick.
					// Sends within the limit are the existing measurement envelope.
					var bound time.Duration
					from, to := c.Call.StartedAt.Add(move.Start-time.Second), c.Call.StartedAt.Add(max(move.End, move.Measurement.MediaResumedAt))
					for _, s := range p.Samples {
						if s.IntervalLateness.Max > r.Limits.P99 && s.At.After(from) && s.At.Add(-s.Interval).Before(to) {
							bound = max(bound, s.IntervalLateness.Max+soakAudioTick)
						}
					}
					for _, track := range move.Tracks {
						row := rows[track.Kind]
						if row == nil {
							row = &loadgen.MetricRow{EventIndex: index, After: e.Event.After, Scenario: move.Kind, Metric: track.Kind + "_media_gap", Events: e.Affected}
							rows[track.Kind] = row
						}
						if track.PacketsAfter == 0 {
							row.Trusted++
							row.Failed++
							continue
						}
						if move.Kind == "takeover" {
							// A pass within the stall bound of 2 s is untrusted.
							if track.Gap < 2*time.Second && track.Gap+bound >= 2*time.Second {
								continue
							}
							row.Trusted++
							if track.Gap >= 2*time.Second {
								row.Failed++
							}
							continue
						}
						if track.BaselineGapSamples < 10 {
							continue
						}
						row.Trusted++
						gaps[track.Kind] = append(gaps[track.Kind], track.Gap)
						excess[track.Kind] = append(excess[track.Kind], track.Gap-track.BaselineGapP95)
						worstGaps[track.Kind] = append(worstGaps[track.Kind], track.Gap+bound)
						worstExcess[track.Kind] = append(worstExcess[track.Kind], track.Gap-track.BaselineGapP95+bound)
					}
				}
			}
		}
	}
	var result []loadgen.MetricRow
	kinds := make([]string, 0, len(rows))
	for kind := range rows {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	for _, kind := range kinds {
		row := rows[kind]
		if percentile95(gaps[kind]) > 150*time.Millisecond || percentile95(excess[kind]) > 50*time.Millisecond {
			row.Failed++
		}
		if row.Events > 0 {
			row.TrustedCoverage = float64(row.Trusted) / float64(row.Events)
		}
		row.Outcome = "pass"
		if row.Events == 0 || row.TrustedCoverage < .8 {
			row.Outcome = "inconclusive"
		}
		// A measured pass that a stall bound could turn into a failure is
		// unproven. The worst case only adds, so a measured failure still fails.
		if percentile95(worstGaps[kind]) > 150*time.Millisecond || percentile95(worstExcess[kind]) > 50*time.Millisecond {
			row.Outcome = "inconclusive"
		}
		if row.Failed > 0 {
			row.Outcome = "fail"
		}
		result = append(result, *row)
	}
	return result
}

// These bounds match the owned topology's unmodified control/store defaults:
// lease=3s, retention=5m, index grace=5m. No product retention is shortened.
const (
	soakRelayWait    = relay.DefaultFlowTimeout + 5*time.Second
	soakSessionLimit = 3*time.Second + 5*time.Minute + 5*time.Second
	soakIndexLimit   = 10*time.Minute + 5*time.Second
)

func boundedSoakKey(k loadgen.RetainedKey) bool {
	switch k.Class {
	case "expired":
		return k.TTLMS == -2
	case "session-counter":
		return k.TTLMS == -1 || k.TTLMS > 0
	case "relay-lease":
		// The hash persists for its monotone epoch. Release clears the identities;
		// expiry is a field, not PTTL. Missing expiry evidence must fail.
		return k.LeaseRemainingMS != nil && (*k.LeaseRemainingMS <= relaylease.DefaultTTL.Milliseconds())
	case "frame-cache":
		return k.TTLMS > 0 && k.TTLMS <= (30*time.Second).Milliseconds()
	case "session-ended":
		return k.TTLMS > 0 && k.TTLMS <= sessionstore.EndedRetention.Milliseconds()
	case "session", "worker-index":
		limit := soakSessionLimit.Milliseconds()
		if k.Class == "worker-index" {
			limit = soakIndexLimit.Milliseconds()
		}
		return k.TTLMS > 0 && k.TTLMS <= limit
	default:
		return false
	}
}

func observeSoakKey(ctx context.Context, client *redis.Client, prefix, key string) (loadgen.RetainedKey, error) {
	k := loadgen.RetainedKey{Key: key, Class: "unknown"}
	ttl, err := client.PTTL(ctx, key).Result()
	if err != nil {
		return k, err
	}
	// go-redis preserves Redis sentinel -1/-2 as nanoseconds, not milliseconds.
	k.TTLMS = ttl.Milliseconds()
	if ttl < 0 {
		k.TTLMS = int64(ttl)
	}
	suffix := strings.TrimPrefix(key, prefix)
	if !strings.HasPrefix(key, prefix) {
		return k, errors.New("key outside owned prefix")
	}
	switch {
	case ttl == -2:
		// SCAN and PTTL are separate reads; positive-TTL keys can expire between them.
		k.Class = "expired"
	case strings.HasPrefix(suffix, "frames:{sess:") && strings.HasSuffix(suffix, "}"):
		k.Class, k.LimitMS = "frame-cache", (30 * time.Second).Milliseconds()
	case strings.HasPrefix(suffix, "ended:{sess:") && strings.HasSuffix(suffix, "}"):
		// Release passes EndedRetention as ARGV[6]; release Lua expires this
		// tombstone at Redis TIME + ARGV[6], independently of the lease TTL.
		k.Class, k.LimitMS = "session-ended", sessionstore.EndedRetention.Milliseconds()
	case suffix == "session_epoch":
		k.Class = "session-counter"
	case strings.HasPrefix(suffix, "worker:"):
		k.Class, k.LimitMS = "worker-index", soakIndexLimit.Milliseconds()
	case suffix == "relay:{relay:ZGVmYXVsdA}":
		k.Class, k.LimitMS = "relay-lease", relaylease.DefaultTTL.Milliseconds()
		// Atomically compare the lease expiry to Redis server time. A missing or
		// malformed field is an observation error, never an expired lease.
		values, err := client.Eval(ctx, `local c=redis.call('HMGET',KEYS[1],'holder','expires','forwarder','previous'); local expires=tonumber(c[2]); if not expires or not c[1] or not c[3] or not c[4] then return redis.error_reply('invalid relay fencing hash') end; local t=redis.call('TIME'); return {expires-tonumber(t[1])*1000-math.floor(tonumber(t[2])/1000), c[1]=='' and c[3]=='' and c[4]=='' and expires==0 and 1 or 0}`, []string{key}).Slice()
		if err != nil {
			return k, err
		}
		if len(values) != 2 {
			return k, errors.New("invalid relay expiry reply")
		}
		remaining, ok := values[0].(int64)
		released, valid := values[1].(int64)
		if !ok || !valid {
			return k, errors.New("invalid relay expiry types")
		}
		k.LeaseRemainingMS, k.Released = &remaining, released == 1
	default:
		for _, kind := range []string{"lease", "state", "record", "epoch", "routes"} {
			if strings.HasPrefix(suffix, kind+":{sess:") && strings.HasSuffix(suffix, "}") {
				k.Class, k.LimitMS = "session", soakSessionLimit.Milliseconds()
				break
			}
		}
	}
	return k, nil
}

func printSoakVerdict(r soakReport, output string) {
	for _, e := range r.Events {
		fmt.Printf("soak event=%d kind=%s outcome=%s saturated-window=%t\n", e.Index, e.Kind, e.Outcome, e.Saturated)
		for _, cause := range e.Causes {
			fmt.Printf("soak event=%d cause=%s\n", e.Index, cause)
		}
	}
	printSoakDelivery(r.Delivery, r.KernelUDP)
	fmt.Printf("soak outcome=%s saturation=%.3f%% conclusive-events=%d/%d (%.3f%%) report=%s\n", r.Outcome, r.Saturation.Percent, r.ConclusiveEvents, len(r.Events), r.ConclusiveEventPercent, output)
	for _, cause := range r.Causes {
		fmt.Println("soak cause:", cause)
	}
	fmt.Printf("soak acceptance=%t\n", r.Acceptance)
	for _, reason := range r.AcceptanceReasons {
		fmt.Println("soak acceptance reason:", reason)
	}
}

// Recycled-worker RSS at matched age is informational. It never substitutes
// for the uninterrupted sentinel's conclusive memory verdict.
type workerTrendPoint struct {
	PID                    int       `json:"pid"`
	StartedAt              time.Time `json:"started_at"`
	SampleAt               time.Time `json:"sample_at"`
	RSSBytes               uint64    `json:"rss_bytes"`
	PhysicalFootprintBytes uint64    `json:"physical_footprint_bytes,omitempty"`
	Available              bool      `json:"available"`
}
type recycledWorkerTrend struct {
	Worker              string             `json:"worker"`
	MatchedAge          time.Duration      `json:"matched_age"`
	Informational       bool               `json:"informational"`
	Points              []workerTrendPoint `json:"points"`
	TrendAvailable      bool               `json:"trend_available"`
	RSSFirstToLastBytes int64              `json:"rss_first_to_last_bytes"`
}

func recycledWorkerTrends(processes []capacity.Process, sentinel string) []recycledWorkerTrend {
	byName := map[string]*recycledWorkerTrend{}
	for _, p := range processes {
		if !strings.HasPrefix(p.Name, "worker-") || p.Name == sentinel {
			continue
		}
		trend := byName[p.Name]
		if trend == nil {
			trend = &recycledWorkerTrend{Worker: p.Name, MatchedAge: 30 * time.Second, Informational: true}
			byName[p.Name] = trend
		}
		point := workerTrendPoint{PID: p.PID}
		if len(p.Memory) > 0 {
			point.StartedAt = p.Memory[0].At
			target := point.StartedAt.Add(trend.MatchedAge)
			for _, s := range p.Memory {
				if !s.At.Before(target) && s.At.Before(target.Add(2*time.Second)) {
					point.SampleAt, point.RSSBytes, point.PhysicalFootprintBytes, point.Available = s.At, s.RSSBytes, s.PhysicalFootprintBytes, s.RSSBytes > 0
					break
				}
			}
		}
		trend.Points = append(trend.Points, point)
	}
	var result []recycledWorkerTrend
	for _, trend := range byName {
		sort.Slice(trend.Points, func(i, j int) bool { return trend.Points[i].StartedAt.Before(trend.Points[j].StartedAt) })
		var available []workerTrendPoint
		for _, p := range trend.Points {
			if p.Available {
				available = append(available, p)
			}
		}
		if len(available) > 1 {
			trend.TrendAvailable = true
			trend.RSSFirstToLastBytes = int64(available[len(available)-1].RSSBytes) - int64(available[0].RSSBytes)
		}
		result = append(result, *trend)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Worker < result[j].Worker })
	return result
}

type continuityCause struct {
	Failed bool
	Reason string
}

func soakContinuityCauses(r loadgen.RunReport, p loadgen.ProcessReport, message loadgen.Message, b saturationBudget) []continuityCause {
	var causes []continuityCause
	for _, track := range message.Call.Tracks {
		if !track.MediaGapsRecorded {
			causes = append(causes, continuityCause{Reason: track.Kind + " whole-call gap inventory unavailable"})
			continue
		}
		if track.MediaGapOverflow > 0 {
			causes = append(causes, continuityCause{Failed: true, Reason: track.Kind + " whole-call gap inventory overflow"})
		}
		for _, gap := range track.MediaGaps {
			if !gapOutsideScheduledEvent(r.Events, message.Session, message.Call, gap.EndedAt, gap.Duration) {
				continue
			}
			end := message.Call.StartedAt.Add(gap.EndedAt)
			start := end.Add(-gap.Duration)
			saturated := false
			for _, s := range b.Excluded {
				if s.PID == p.PID && s.Start.Before(end) && s.End.After(start) {
					saturated = true
					break
				}
			}
			reason := fmt.Sprintf("%s unscheduled media gap %s ended at %s", track.Kind, gap.Duration, gap.EndedAt)
			if saturated {
				reason += " overlaps this caller's generator saturation"
			}
			causes = append(causes, continuityCause{Failed: !saturated, Reason: reason})
		}
	}
	return causes
}
func soakScanControls(keys []string, prefix string) map[string]bool {
	controls := map[string]bool{"session_epoch": false, "relay-fencing-hash": false}
	for _, key := range keys {
		if key == prefix+"session_epoch" {
			controls["session_epoch"] = true
		}
		if key == prefix+"relay:{relay:ZGVmYXVsdA}" {
			controls["relay-fencing-hash"] = true
		}
	}
	return controls
}
