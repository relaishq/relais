package loadgen

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/relais/pkg/callharness"
)

// Message is the child JSON-lines protocol. Call is the existing caller-side
// report, including every affected call's per-event yardstick and verdicts.
type Message struct {
	Type    string              `json:"type"`
	PID     int                 `json:"pid"`
	CallID  int                 `json:"call_id,omitempty"`
	Session string              `json:"session,omitempty"`
	Stats   *Stats              `json:"stats,omitempty"`
	Call    *callharness.Report `json:"call,omitempty"`
	Error   string              `json:"error,omitempty"`
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
	PID                  int           `json:"pid"`
	ExpectedCalls        int           `json:"expected_calls"`
	Calls                []Message     `json:"calls"`
	Samples              []Stats       `json:"samples"`
	SamplesDropped       int           `json:"samples_dropped"`
	PeakIntervalP99      time.Duration `json:"peak_interval_p99_ns"`
	Complete             bool          `json:"complete"`
	Saturated            bool          `json:"saturated"`
	Causes               []string      `json:"causes"`
	PeakCPUPercent       float64       `json:"peak_cpu_percent_one_core"`
	PeakCPUPerGOMAXPROCS float64       `json:"peak_cpu_percent_per_gomaxprocs"`
	PeakHeapBytes        uint64        `json:"peak_heap_bytes"`
	PeakRSSBytes         uint64        `json:"peak_rss_bytes"`
	Lateness             Lateness      `json:"lateness"`
}

func ReadProcess(r io.Reader, pid, expected int, limits Limits) (ProcessReport, error) {
	out := ProcessReport{PID: pid, ExpectedCalls: expected}
	decoder := json.NewDecoder(r)
	seen := map[int]bool{}
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
			if len(out.Samples) == 256 {
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
			if s.IntervalLateness.P99 > limits.P99 || s.CPUPerGOMAXPROCS > limits.CPUPercent {
				out.Saturated = true
			}

		case "done":
			out.Complete = true
		default:
			return out, fmt.Errorf("unknown child message %q", m.Type)
		}
	}
	peak := Stats{Lateness: out.Lateness, IntervalLateness: Lateness{P99: out.PeakIntervalP99}, CPUPerGOMAXPROCS: out.PeakCPUPerGOMAXPROCS}
	out.Causes = append(out.Causes, limits.Causes(peak)...)
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
	Event    Event     `json:"event"`
	At       time.Time `json:"at"`
	Affected int       `json:"affected"`
	Error    string    `json:"error,omitempty"`
}
type RunReport struct {
	StartedAt time.Time       `json:"started_at"`
	Duration  time.Duration   `json:"duration_ns"`
	Callers   int             `json:"callers"`
	Limits    Limits          `json:"limits"`
	Processes []ProcessReport `json:"processes"`
	Events    []EventResult   `json:"events"`
	Yardstick []MetricRow     `json:"yardstick"`
	Outcome   string          `json:"outcome"`
	Causes    []string        `json:"causes"`
}

// Judge keeps generator validity separate from caller failures. A saturated or
// incomplete run cannot establish capacity. Caller results remain inspectable.
func (r *RunReport) Judge() {
	r.Yardstick = yardstick(r.Processes)
	r.Outcome = "valid"
	r.Causes = nil
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
