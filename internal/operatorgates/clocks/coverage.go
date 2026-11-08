package clocks

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/relaishq/relais/internal/operatorgates/observe"
)

const CoverageAttemptLimit = 256
const CoverageDrainAttemptLimit = 64
const CoverageControlBoundNS int64 = 2000000
const CoverageWorkerBoundNS int64 = 3000000
const CoverageDriftPPM float64 = 100

func FixedCoverageBounds() Bounds {
	return Bounds{FrontendControlBoundNS: CoverageControlBoundNS, FrontendWorkerBoundNS: CoverageWorkerBoundNS, DriftPPM: CoverageDriftPPM}
}

// CoverageEvidence belongs to one run, not individual phases. Its evaluated
// results qualify judge windows, without supplying admission evidence.
type CoverageEvidence struct {
	Falsified            bool                             `json:"falsified"`
	Version              int                              `json:"version"`
	FrontendSlot         string                           `json:"frontendSlot"`
	ControlSlot          string                           `json:"controlSlot"`
	Bounds               Bounds                           `json:"bounds"`
	ClockReceiptSHA256   string                           `json:"clockReceiptSha256,omitempty"`
	Binding              *observe.ClockBinding            `json:"binding,omitempty"`
	DrainAttempts        []CoverageAttempt                `json:"drainAttempts"`
	OmittedDrainAttempts int                              `json:"omittedDrainAttempts"`
	OmittedBrackets      int                              `json:"omittedBrackets"`
	MissingIntervals     map[string]observe.CoverageBreak `json:"missingIntervals"`
	MediaTimeOffsetNS    int64                            `json:"mediaTimeOffsetNs"`
	Attempts             []CoverageAttempt                `json:"attempts"`
	OmittedAttempts      int                              `json:"omittedAttempts"`
	KillBrackets         []observe.CoverageBracket        `json:"killBrackets"`
	Windows              []observe.CoverageWindow         `json:"windows"`
	Results              []observe.CoverageResult         `json:"results"`
	DrainComplete        bool                             `json:"drainComplete"`
}

// CoverageAttempt keeps only retrospective numeric tracking data. Opaque
// cursors and raw probe batches never become part of the raw-media contract.
type CoverageAttempt struct {
	Version int                                 `json:"version"`
	At      time.Time                           `json:"at"`
	Clocks  map[string]observe.CoverageTimeline `json:"clocks"`
}

func NewCoverageAttempt(e Evidence) CoverageAttempt {
	return newCoverageAttempt(e, false)
}
func NewBoundCoverageAttempt(e Evidence) CoverageAttempt { return newCoverageAttempt(e, true) }
func newCoverageAttempt(e Evidence, bound bool) CoverageAttempt {
	out := CoverageAttempt{Version: e.Version, At: e.At, Clocks: map[string]observe.CoverageTimeline{}}
	for _, slot := range []string{"relay", "shared", "w1", "w2"} {
		s, ok := e.Clocks[slot]
		if !ok {
			continue
		}
		s = PublicSample(s)
		t := observe.CoverageTimeline{Rows: []observe.CoverageRow{}, Breaks: []observe.CoverageBreak{}}
		if l := s.TrackingLog; l != nil {
			t.ReadComplete = l.ReadComplete
			for _, r := range l.Rows {
				t.Rows = append(t.Rows, observe.CoverageRow{UnixSeconds: r.UnixSeconds, MaxErrorSeconds: r.MaxErrorSeconds, LeapStatus: r.LeapStatus})
				if bound {
					t.Rows[len(t.Rows)-1].MaxErrorDecimal = r.MaxErrorDecimal
				}
			}
			for _, b := range l.Breaks {
				t.Breaks = append(t.Breaks, observe.CoverageBreak{FromUnixSeconds: b.FromUnixSeconds, UnixSeconds: b.UnixSeconds, Kind: b.Kind})
			}
		}
		out.Clocks[slot] = t
	}
	return out
}
func PublicEvidence(e Evidence) Evidence {
	rows := map[string]Sample{}
	for _, slot := range []string{"relay", "shared", "w1", "w2"} {
		if s, ok := e.Clocks[slot]; ok {
			s = PublicSample(s)
			if !nsPattern.MatchString(s.MeasuredUnixNS) {
				s.MeasuredUnixNS = "unavailable"
			}
			rows[slot] = s
		}
	}
	e.Clocks = rows
	return e
}
func publicCoverageSlot(s string) string {
	switch s {
	case "relay", "shared", "w1", "w2":
		return s
	}
	return "unavailable"
}
func (e CoverageEvidence) Public() CoverageEvidence {
	e.FrontendSlot = publicCoverageSlot(e.FrontendSlot)
	e.ControlSlot = publicCoverageSlot(e.ControlSlot)
	e.Attempts = publicAttempts(e.Attempts)
	e.DrainAttempts = publicAttempts(e.DrainAttempts)
	missing := map[string]observe.CoverageBreak{}
	for _, slot := range []string{"relay", "shared", "w1", "w2"} {
		if b, ok := e.MissingIntervals[slot]; ok {
			b.Kind = "log-loss"
			missing[slot] = b
		}
	}
	e.MissingIntervals = missing

	e.KillBrackets = append([]observe.CoverageBracket{}, e.KillBrackets...)
	for i := range e.KillBrackets {
		r := &e.KillBrackets[i]
		r.Worker = publicCoverageSlot(r.Worker)
		for _, s := range []*string{&r.BeforeUnixNS, &r.AfterUnixNS, &r.MidpointUnixNS} {
			if !nsPattern.MatchString(*s) {
				*s = "unavailable"
			}
		}
	}
	e.Windows = append([]observe.CoverageWindow{}, e.Windows...)
	for i := range e.Windows {
		w := &e.Windows[i]
		w.Worker = publicCoverageSlot(w.Worker)
		if w.Kind != "move" && w.Kind != "takeover" && w.Kind != "kill" {
			w.Kind = "unavailable"
		}
		for _, s := range []*string{&w.StartUnixNS, &w.EndUnixNS} {
			if !nsPattern.MatchString(*s) {
				*s = "unavailable"
			}
		}
	}
	// Recompute numeric diagnostics instead of publishing arbitrary cause labels.
	e.Evaluate()
	if e.Binding != nil && !e.Binding.Valid(e.ClockReceiptSHA256) {
		e.Binding = nil
	}
	if e.ClockReceiptSHA256 != "" && !observe.ClockDigestValid(e.ClockReceiptSHA256) {
		e.ClockReceiptSHA256 = "unavailable"
	}
	return e
}
func (e CoverageEvidence) MarshalJSON() ([]byte, error) {
	type wire CoverageEvidence
	return json.Marshal(wire(e.Public()))
}

// Timelines preserves every observed interval, including rows from a partially
// failed batch. Only the latest read's completeness controls drain termination.
func (e CoverageEvidence) Timelines() map[string]observe.CoverageTimeline {
	out := map[string]observe.CoverageTimeline{}
	attempts := append(append([]CoverageAttempt{}, e.Attempts...), e.DrainAttempts...)
	for _, attempt := range attempts {
		if attempt.Version != EvidenceVersion {
			continue
		}
		for slot, next := range attempt.Clocks {
			t := out[slot]
			t.ReadComplete = next.ReadComplete
			t.Rows = append(t.Rows, next.Rows...)
			t.Breaks = append(t.Breaks, next.Breaks...)
			out[slot] = t
		}
	}
	for slot, loss := range e.MissingIntervals {
		t := out[slot]
		t.Breaks = append(t.Breaks, loss)
		out[slot] = t
	}
	return out
}
func (e *CoverageEvidence) Evaluate() {
	e.Version = EvidenceVersion
	t := e.Timelines()
	bounds, k, err := e.coverageParameters()
	if e.Binding != nil && err == nil {
		e.Results = observe.EvaluateBoundClockCoverage(t, e.FrontendSlot, e.ControlSlot, bounds.FrontendControlBoundNS, bounds.FrontendWorkerBoundNS, k, e.Windows, e.KillBrackets)
	} else {
		e.Results = observe.EvaluateClockCoverage(t, e.FrontendSlot, e.ControlSlot, bounds.FrontendControlBoundNS, bounds.FrontendWorkerBoundNS, e.Windows, e.KillBrackets)
	}
	if err != nil {
		cause := "clock-coverage-bound"
		if observe.IsBound(e.ClockReceiptSHA256, e.Binding) {
			cause = "clock-coverage-binding"
		}
		for i := range e.Results {
			if !slices.Contains(e.Results[i].Causes, cause) {
				e.Results[i].Causes = append(e.Results[i].Causes, cause)
			}
			sort.Strings(e.Results[i].Causes)
		}
	}
	e.DrainComplete = observe.CoverageTailReady(t, e.FrontendSlot, e.ControlSlot, bounds.FrontendControlBoundNS, bounds.FrontendWorkerBoundNS, e.Windows, e.KillBrackets)
	if err != nil && observe.IsBound(e.ClockReceiptSHA256, e.Binding) {
		e.DrainComplete = false
	}
}
func TimestampNS(n int64) string { return fmt.Sprintf("%019d", n) }

func publicAttempts(attempts []CoverageAttempt) []CoverageAttempt {
	out := append([]CoverageAttempt{}, attempts...)
	for i := range out {
		a := &out[i]

		rows := map[string]observe.CoverageTimeline{}
		for _, slot := range []string{"relay", "shared", "w1", "w2"} {
			if t, ok := a.Clocks[slot]; ok {
				t.Rows = append([]observe.CoverageRow{}, t.Rows...)
				t.Breaks = append([]observe.CoverageBreak{}, t.Breaks...)
				for j := range t.Rows {
					if raw := t.Rows[j].MaxErrorDecimal; raw != "" {
						if _, err := observe.ClockMaximumNS(raw); err != nil {
							t.Rows[j].MaxErrorDecimal = "unavailable"
						}
					}
					switch t.Rows[j].LeapStatus {
					case "N", "+", "-", "?":
					default:
						t.Rows[j].LeapStatus = "unavailable"
					}
				}
				for j := range t.Breaks {
					switch t.Breaks[j].Kind {
					case "cursor-gap", "log-rotation", "log-loss", "clock-step", "daemon-restart", "source-change", "leap-status", "self-consistency":
					default:
						t.Breaks[j].Kind = "unavailable"
					}
				}
				rows[slot] = t
			}
		}
		a.Clocks = rows
	}
	return out
}

// HasFalsification includes cumulative counters: evicted probe details cannot
// restore clock trust. A disabled prober supplies neither counters nor records.
func HasFalsification(e Evidence) bool {
	f := e.Falsification
	if f == nil {
		return false
	}
	if f.CountsByStatus["falsified"] > 0 {
		return true
	}
	for _, record := range f.Records {
		if record.Status == "falsified" {
			return true
		}
	}
	return false
}
func (e CoverageEvidence) Qualification() observe.CoverageQualification {
	e.Evaluate()
	return observe.CoverageQualification{Results: e.Results, Falsified: e.Falsified, ClockReceiptSHA256: e.ClockReceiptSHA256, Binding: e.Binding}
}
