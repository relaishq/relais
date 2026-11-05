package operatorrehearsal

import (
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/relaishq/relais/internal/operatorevidence"
	"github.com/relaishq/relais/pkg/callharness"
	"github.com/relaishq/relais/pkg/operator"
)

// CallerTiming is diagnostic clock-probe evidence for one caller call: probe
// sends near each move or takeover, its timing_evidence binding probes, and
// spans without probes (for example frontend restarts). Evaluate never reads it.
type CallerTiming struct {
	Gate                operator.Gate `json:"gate"`
	Probes              int           `json:"probes"`
	CapacityInvalidated bool          `json:"capacityInvalidated"`
	ProbeGaps           []ProbeGap    `json:"probeGaps"`
	Events              []EventTiming `json:"events"`
}

// ProbeGap is a span longer than probeGapMinimum between successive probe sends.
type ProbeGap struct {
	StartUnixMS int64 `json:"startUnixMs"`
	EndUnixMS   int64 `json:"endUnixMs"`
	DurationMS  int64 `json:"durationMs"`
}

// EventTiming compares one receipt start with the caller's probe sends.
// NearestProbeMS is probe send minus receipt start; the two clocks are only
// chrony-aligned, so the binding probes in Start and End are the exact record.
type EventTiming struct {
	Kind              string         `json:"kind"`
	StartUnixMS       int64          `json:"startUnixMs"`
	NearestProbeMS    *int64         `json:"nearestProbeMs"`
	ProbesWithin300MS int            `json:"probesWithin300ms"`
	TimingEvidence    bool           `json:"timingEvidence"`
	Start             *BindingTiming `json:"start,omitempty"`
	End               *BindingTiming `json:"end,omitempty"`
}

// BindingTiming holds the lower and upper binding probes of one mapped event
// endpoint: the larger |age| bound and drift_per_side, in ns; -1 if absent.
// callharness Map intersects all probes and binds the tightest bound, not the
// nearest probe, so a large binding age does not mean a loose bound.
type BindingTiming struct {
	AgeNS   [2]int64 `json:"bindingAgeNs"`
	DriftNS [2]int64 `json:"bindingDriftNs"`
}

const probeGapMinimum = 30 * time.Second
const eventProbeWindow = 300 * time.Millisecond

// callerLine decodes only the caller report members this summary reads.
type callerLine struct {
	Type string `json:"type"`
	Call *struct {
		Distributed *struct {
			Probes []struct {
				CallerSend time.Time `json:"caller_send"`
			} `json:"probes"`
			ClockError string   `json:"clock_error"`
			Causes     []string `json:"causes"`
		} `json:"distributed"`
		Moves []struct {
			Kind    string
			Receipt *struct {
				Start time.Time `json:"start"`
			} `json:"receipt"`
			Distributed *struct {
				Causes []string                    `json:"causes"`
				Timing *callharness.TimingEvidence `json:"timing_evidence"`
			} `json:"distributed"`
		}
	} `json:"call"`
}

func callerTiming(p operatorevidence.Pool) []CallerTiming {
	rows := []CallerTiming{}
	for _, phase := range p.Phases {
		decoder := json.NewDecoder(strings.NewReader(phase.CallerJSONL))
		for {
			var raw json.RawMessage
			if decoder.Decode(&raw) != nil {
				break
			}
			var line callerLine
			if json.Unmarshal(raw, &line) != nil || line.Type != "call" || line.Call == nil {
				continue
			}
			rows = append(rows, line.timing(phase.Gate))
		}
	}
	return rows
}

func capacityExceeded(s string) bool { return strings.Contains(s, "clock probe capacity exceeded") }

func (l callerLine) timing(gate operator.Gate) CallerTiming {
	row := CallerTiming{Gate: gate, ProbeGaps: []ProbeGap{}, Events: []EventTiming{}}
	var sends []time.Time
	if d := l.Call.Distributed; d != nil {
		for _, p := range d.Probes {
			if !p.CallerSend.IsZero() {
				sends = append(sends, p.CallerSend)
			}
		}
		row.CapacityInvalidated = capacityExceeded(d.ClockError) || slices.ContainsFunc(d.Causes, capacityExceeded)
	}
	slices.SortFunc(sends, time.Time.Compare)
	row.Probes = len(sends)
	for i := 1; i < len(sends); i++ {
		if gap := sends[i].Sub(sends[i-1]); gap > probeGapMinimum {
			row.ProbeGaps = append(row.ProbeGaps, ProbeGap{sends[i-1].UnixMilli(), sends[i].UnixMilli(), gap.Milliseconds()})
		}
	}
	for _, m := range l.Call.Moves {
		e := EventTiming{Kind: m.Kind}
		if d := m.Distributed; d != nil {
			row.CapacityInvalidated = row.CapacityInvalidated || slices.ContainsFunc(d.Causes, capacityExceeded)
			if d.Timing != nil {
				e.TimingEvidence = true
				e.Start, e.End = bindingTiming(d.Timing.Start), bindingTiming(d.Timing.End)
			}
		}
		if m.Receipt != nil && !m.Receipt.Start.IsZero() {
			start := m.Receipt.Start
			e.StartUnixMS = start.UnixMilli()
			var nearest time.Duration
			for i, s := range sends {
				offset := s.Sub(start)
				if i == 0 || offset.Abs() < nearest.Abs() {
					nearest = offset
				}
				if offset.Abs() <= eventProbeWindow {
					e.ProbesWithin300MS++
				}
			}
			if len(sends) > 0 {
				ms := nearest.Milliseconds()
				e.NearestProbeMS = &ms
			}
		}
		row.Events = append(row.Events, e)
	}
	return row
}

func bindingTiming(e callharness.ClockMappingEvidence) *BindingTiming {
	b := &BindingTiming{AgeNS: [2]int64{-1, -1}, DriftNS: [2]int64{-1, -1}}
	for i, index := range []int{e.LowerProbe, e.UpperProbe} {
		for _, p := range e.Probes {
			if p.Index == index {
				b.AgeNS[i] = max(p.Age.Lower.Abs(), p.Age.Upper.Abs()).Nanoseconds()
				b.DriftNS[i] = p.Drift.Nanoseconds()
			}
		}
	}
	return b
}

// sanitizeCallerTiming keeps numbers and fixed labels only.
func sanitizeCallerTiming(rows []CallerTiming) []CallerTiming {
	out := make([]CallerTiming, len(rows))
	for i, row := range rows {
		if !row.Gate.Valid() {
			row.Gate = "unavailable"
		}
		row.ProbeGaps = append([]ProbeGap{}, row.ProbeGaps...)
		row.Events = append([]EventTiming{}, row.Events...)
		for j := range row.Events {
			if k := row.Events[j].Kind; k != "move" && k != "takeover" {
				row.Events[j].Kind = "unavailable"
			}
		}
		out[i] = row
	}
	return out
}
