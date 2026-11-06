package clocks

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/relaishq/relais/internal/operatorgates/observe"
)

const CoverageAttemptLimit = 256
const CoverageDrainAttemptLimit = 64

// CoverageEvidence belongs to one run, not individual phases. These diagnostics
// do not supply admission evidence or change a judge's input media records.
type CoverageEvidence struct {
	Version              int                              `json:"version"`
	FrontendSlot         string                           `json:"frontendSlot"`
	ControlSlot          string                           `json:"controlSlot"`
	Bounds               Bounds                           `json:"bounds"`
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
	e.Results = observe.EvaluateClockCoverage(t, e.FrontendSlot, e.ControlSlot, e.Bounds.FrontendControlBoundNS, e.Bounds.FrontendWorkerBoundNS, e.Windows, e.KillBrackets)
	e.DrainComplete = observe.CoverageTailReady(t, e.FrontendSlot, e.ControlSlot, e.Bounds.FrontendControlBoundNS, e.Bounds.FrontendWorkerBoundNS, e.Windows, e.KillBrackets)
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
