package loadgen

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/relais/internal/capacity"
	"github.com/relais/pkg/callharness"
)

// Message is the child JSON-lines protocol. Call is the existing caller-side
// report, including every affected call's per-event yardstick and verdicts.
// Dial is what the caller observed while a "dial" message's call connected
// or failed to.
type Message struct {
	Type    string                  `json:"type"`
	PID     int                     `json:"pid"`
	CallID  int                     `json:"call_id,omitempty"`
	Session string                  `json:"session,omitempty"`
	Stats   *Stats                  `json:"stats,omitempty"`
	Call    *callharness.Report     `json:"call,omitempty"`
	Dial    *callharness.DialReport `json:"dial,omitempty"`
	Error   string                  `json:"error,omitempty"`
}
type Writer struct {
	mu      sync.Mutex
	encoder *json.Encoder
	err     error
}

func NewWriter(w io.Writer) *Writer { return &Writer{encoder: json.NewEncoder(w)} }
func (w *Writer) Write(m Message) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err == nil {
		w.err = w.encoder.Encode(m)
	}
}
func (w *Writer) Err() error { w.mu.Lock(); defer w.mu.Unlock(); return w.err }

type ProcessReport struct {
	PID                         int           `json:"pid"`
	ExpectedCalls               int           `json:"expected_calls"`
	Dials                       []Message     `json:"dials,omitempty"`
	Calls                       []Message     `json:"calls"`
	Samples                     []Stats       `json:"samples"`
	SamplesDropped              int           `json:"samples_dropped"`
	PeakIntervalP99             time.Duration `json:"peak_interval_p99_ns"`
	PeakSkippedPercent          float64       `json:"peak_interval_skipped_frame_percent"`
	Pacing                      Pacing        `json:"pacing"`
	StatusRequests              uint64        `json:"status_requests"`
	StatusRequestsPerSecond     float64       `json:"status_requests_per_second"`
	PeakStatusRequestsPerSecond float64       `json:"peak_status_requests_per_second"`
	Complete                    bool          `json:"complete"`
	Saturated                   bool          `json:"saturated"`
	Causes                      []string      `json:"causes"`
	PeakCPUPercent              float64       `json:"peak_cpu_percent_one_core"`
	PeakCPUPerGOMAXPROCS        float64       `json:"peak_cpu_percent_per_gomaxprocs"`
	PeakHeapBytes               uint64        `json:"peak_heap_bytes"`
	PeakRSSBytes                uint64        `json:"peak_rss_bytes"`
	Lateness                    Lateness      `json:"lateness"`
}

func ReadProcess(r io.Reader, pid, expected int, limits Limits) (ProcessReport, error) {
	return readProcess(r, pid, expected, limits, false)
}

// ReadSoakProcess retains all intervals; short-load reports remain bounded.
func ReadSoakProcess(r io.Reader, pid, expected int, limits Limits) (ProcessReport, error) {
	return readProcess(r, pid, expected, limits, true)
}

func readProcess(r io.Reader, pid, expected int, limits Limits, all bool) (ProcessReport, error) {
	out := ProcessReport{PID: pid, ExpectedCalls: expected}
	decoder := json.NewDecoder(r)
	seen, dialed := map[int]bool{}, map[int]bool{}
	var statusRequests uint64
	var measured time.Duration
	var peakPacing Pacing
	for {
		var m Message
		if err := decoder.Decode(&m); err != nil {
			if err == io.EOF {
				break
			}
			return out, err
		}
		if m.PID != pid {
			return out, fmt.Errorf("report PID %d differs from owned child %d", m.PID, pid)
		}
		if out.Complete {
			return out, fmt.Errorf("message after terminal done")
		}
		switch m.Type {
		case "dial":
			if m.CallID < 0 || m.CallID >= expected || dialed[m.CallID] {
				return out, fmt.Errorf("invalid or duplicate dial %d", m.CallID)
			}
			dialed[m.CallID] = true
			out.Dials = append(out.Dials, m)
			switch {
			case m.Dial == nil:
				out.Causes = append(out.Causes, fmt.Sprintf("dial %d: %s", m.CallID, m.Error))
			case m.Dial.Outcome != callharness.DialConnected:
				out.Causes = append(out.Causes, fmt.Sprintf("dial %d: %s: %s", m.CallID, m.Dial.Failure, m.Error))
			}
		case "call":
			if m.CallID < 0 || m.CallID >= expected || seen[m.CallID] {
				return out, fmt.Errorf("invalid or duplicate call %d", m.CallID)
			}
			seen[m.CallID] = true
			out.Calls = append(out.Calls, m)
			if m.Error != "" || m.Call == nil {
				out.Causes = append(out.Causes, fmt.Sprintf("call %d: %s", m.CallID, m.Error))
			}
		case "stats":
			if m.Stats == nil {
				return out, fmt.Errorf("empty stats message")
			}
			s := *m.Stats
			if !all && len(out.Samples) == 256 {
				copy(out.Samples, out.Samples[1:])
				out.Samples = out.Samples[:255]
				out.SamplesDropped++
			}
			out.Samples = append(out.Samples, s)
			out.PeakIntervalP99 = max(out.PeakIntervalP99, s.IntervalLateness.P99)
			out.PeakCPUPercent = max(out.PeakCPUPercent, s.CPUPercent)
			out.PeakCPUPerGOMAXPROCS = max(out.PeakCPUPerGOMAXPROCS, s.CPUPerGOMAXPROCS)
			out.PeakHeapBytes = max(out.PeakHeapBytes, s.HeapBytes)
			out.PeakRSSBytes = max(out.PeakRSSBytes, s.PeakRSSBytes)
			out.Lateness = s.Lateness
			out.Pacing = s.Pacing
			if s.IntervalPacing.SkippedPercent() > out.PeakSkippedPercent {
				out.PeakSkippedPercent = s.IntervalPacing.SkippedPercent()
				peakPacing = s.IntervalPacing
			}
			out.StatusRequests = s.StatusRequests
			statusRequests += s.IntervalStatusRequests
			measured += s.Interval
			out.PeakStatusRequestsPerSecond = max(out.PeakStatusRequestsPerSecond, s.StatusRequestsPerSecond)
			if s.IntervalLateness.P99 > limits.P99 || s.CPUPerGOMAXPROCS > limits.CPUPercent || s.IntervalPacing.SkippedPercent() > limits.SkippedPercent {
				out.Saturated = true
			}

		case "done":
			out.Complete = true
		default:
			return out, fmt.Errorf("unknown child message %q", m.Type)
		}
	}
	if measured > 0 {
		out.StatusRequestsPerSecond = float64(statusRequests) / measured.Seconds()
	}
	peak := Stats{Lateness: out.Lateness, IntervalLateness: Lateness{P99: out.PeakIntervalP99}, IntervalPacing: peakPacing, CPUPerGOMAXPROCS: out.PeakCPUPerGOMAXPROCS}
	if !all {
		out.Causes = append(out.Causes, limits.Causes(peak)...)
	} else if out.Lateness.Packets == 0 {
		out.Causes = append(out.Causes, "no successful media sends")
	}
	if !out.Complete || len(out.Calls) != expected || len(out.Samples) == 0 {
		out.Complete = false
		out.Causes = append(out.Causes, "missing terminal message, calls or resource samples")
	}
	return out, nil
}

type Event struct {
	After time.Duration `json:"after_ns"`
	Kind  string        `json:"kind"`
	// Count selects the first N calls for move. Kill always affects the whole worker.
	Count  int `json:"count"`
	Worker int `json:"worker"`
}
type EventResult struct {
	Identities []callharness.OperationIdentity `json:"identities,omitempty"`

	Event      Event     `json:"event"`
	At         time.Time `json:"at"`
	FinishedAt time.Time `json:"finished_at"`
	Affected   int       `json:"affected"`
	Sessions   []string  `json:"sessions"`
	Recorded   int       `json:"recorded"`
	Failed     bool      `json:"failed,omitempty"`
	Error      string    `json:"error,omitempty"`
}

// RetainedKey records the physical Redis TTL and, for the persistent relay
// fencing hash, the logical lease expiry measured against Redis TIME.
type RetainedKey struct {
	Error            string `json:"error,omitempty"`
	Key              string `json:"key"`
	Class            string `json:"class"`
	TTLMS            int64  `json:"ttl_ms"`
	LimitMS          int64  `json:"limit_ms"`
	LeaseRemainingMS *int64 `json:"lease_remaining_ms,omitempty"`
	Released         bool   `json:"released,omitempty"`
}
type LeakReport struct {
	ScanControls    map[string]bool `json:"scan_controls"`
	Keys            []RetainedKey   `json:"keys"`
	RelayWait       time.Duration   `json:"relay_wait"`
	RelayObservedAt time.Time       `json:"relay_observed_at"`
	Checked         bool            `json:"checked"`
	Sessions        map[string]int  `json:"sessions"`
	StoreKeys       []string        `json:"store_keys"`
	Processes       []int           `json:"processes"`
	Listeners       []string        `json:"listeners"`
	Errors          []string        `json:"errors,omitempty"`
}

type WorkerRejoin struct {
	Worker        string    `json:"worker"`
	OldPID        int       `json:"old_pid"`
	PID           int       `json:"pid"`
	StartedAt     time.Time `json:"started_at"`
	CompletedAt   time.Time `json:"completed_at"`
	LastHeartbeat time.Time `json:"last_heartbeat"`
}
type DistributedTrialResult struct {
	Outcome string   `json:"outcome"`
	Causes  []string `json:"causes,omitempty"`
}

type RunReport struct {
	Distributed                   *DistributedTrialResult `json:"distributed,omitempty"`
	SentinelWorker                string                  `json:"sentinel_worker,omitempty"`
	MemoryMetric                  string                  `json:"memory_metric,omitempty"`
	WorkerRejoins                 []WorkerRejoin          `json:"worker_rejoins,omitempty"`
	Leaks                         LeakReport              `json:"leaks,omitempty"`
	Telemetry                     capacity.Telemetry      `json:"telemetry"`
	WorkerPeaks                   map[string]int          `json:"worker_peak_calls"`
	StartedAt                     time.Time               `json:"started_at"`
	Duration                      time.Duration           `json:"duration_ns"`
	Callers                       int                     `json:"callers"`
	Limits                        Limits                  `json:"limits"`
	Processes                     []ProcessReport         `json:"processes"`
	Events                        []EventResult           `json:"events"`
	Yardstick                     []MetricRow             `json:"yardstick"`
	Outcome                       string                  `json:"outcome"`
	Causes                        []string                `json:"causes"`
	DriverStatusRequests          uint64                  `json:"driver_status_requests"`
	DriverStatusRequestsPerSecond float64                 `json:"driver_status_requests_per_second"`
}

// Judge keeps generator validity separate from caller failures. A saturated or
// incomplete run cannot establish capacity. Caller results remain inspectable.
func (r *RunReport) Judge() {
	var eventCauses []string
	r.Yardstick, eventCauses = yardstick(r.Processes, r.Events)
	r.Distributed = nil
	if outcome, causes, present := distributedTrials(r.Processes); present {
		r.Distributed = &DistributedTrialResult{Outcome: outcome, Causes: causes}
	}
	r.Outcome = "valid"
	r.Causes = nil
	if len(eventCauses) > 0 {
		r.Outcome = "inconclusive"
		r.Causes = append(r.Causes, eventCauses...)
	}
	completed := 0
	for _, p := range r.Processes {
		completed += len(p.Calls)
		if p.Saturated || !p.Complete || len(p.Causes) > 0 {
			r.Outcome = "inconclusive"
			for _, c := range p.Causes {
				r.Causes = append(r.Causes, fmt.Sprintf("process %d: %s", p.PID, c))
			}
		}
	}
	if r.Callers > 0 && completed != r.Callers {
		r.Outcome = "inconclusive"
		r.Causes = append(r.Causes, fmt.Sprintf("received %d/%d caller reports", completed, r.Callers))
	}
	for _, e := range r.Events {
		if e.Error != "" {
			r.Outcome = "inconclusive"
			r.Causes = append(r.Causes, e.Error)
		}
	}
	if len(r.Processes) == 0 {
		r.Outcome = "inconclusive"
		r.Causes = append(r.Causes, "no caller processes")
	}
}
