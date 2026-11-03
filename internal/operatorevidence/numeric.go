package operatorevidence

import (
	"encoding/json"
	"io"
	"strings"
	"time"

	"github.com/relaishq/relais/internal/loadgen"
	"github.com/relaishq/relais/pkg/callharness"
)

const maxNumericEvents = 128
const maxNumericBytes = 1 << 20

// NumericTiming is a cumulative pool snapshot. Phase/call/move are local
// indexes, never deployment identities. Event joins the scheduled event list.
// Shared endpoints apply to every metric. The capped projection retains the
// latest rows in pool traversal order.
// Missing Timing means an older caller did not record the decomposition.
type NumericTiming struct {
	Version            int            `json:"version"`
	Events             []NumericEvent `json:"events"`
	OmittedEvents      int            `json:"omitted_events"`
	UnavailableStreams int            `json:"unavailable_streams"`
}

type NumericInterval struct {
	Lower time.Duration `json:"lower_ns"`
	Upper time.Duration `json:"upper_ns"`
	Width time.Duration `json:"width_ns"`
}

type NumericGap struct {
	Kind        string                       `json:"kind"`
	BaselineP95 callharness.DurationInterval `json:"baseline_p95"`
	EventGap    callharness.DurationInterval `json:"event_gap"`
}

type NumericEvent struct {
	Phase       int                                     `json:"phase"`
	Call        int                                     `json:"call"`
	Move        int                                     `json:"move"`
	Event       int                                     `json:"event"`
	Kind        string                                  `json:"kind"`
	Mapped      bool                                    `json:"mapped"`
	Start       NumericInterval                         `json:"start"`
	End         NumericInterval                         `json:"end"`
	Durations   map[string]callharness.DurationInterval `json:"durations"`
	OmittedGaps int                                     `json:"omitted_gaps"`
	Gaps        []NumericGap                            `json:"gaps"`
	Timing      *callharness.TimingEvidence             `json:"timing,omitempty"`
}

func numericInterval(i callharness.TimeInterval, origin time.Time) NumericInterval {
	return NumericInterval{Lower: i.Earliest.Sub(origin), Upper: i.Latest.Sub(origin), Width: i.Latest.Sub(i.Earliest)}
}

// TimingDiagnostics projects only fixed labels and numeric data. It does not
// rejudge, validate or repair the evidence consumed by the pinned judge.
func TimingDiagnostics(pool Pool) NumericTiming {
	out := NumericTiming{Version: 1, Events: []NumericEvent{}}
	bytes := 0
	var sizes []int
	for phase, input := range pool.Phases {
		decoder := json.NewDecoder(strings.NewReader(input.CallerJSONL))
		call := 0
		for {
			var message loadgen.Message
			if err := decoder.Decode(&message); err != nil {
				if err != io.EOF {
					out.UnavailableStreams++
				}
				break
			}
			if message.Type != "call" || message.Call == nil {
				continue
			}
			for event, move := range message.Call.Moves {
				d := move.Distributed
				if d == nil {
					continue
				}
				row := NumericEvent{Phase: phase, Call: call, Move: event, Event: -1, Kind: "unavailable", Mapped: d.Mapped, Durations: map[string]callharness.DurationInterval{}}
				matches := 0
				for index, scheduled := range input.Events {
					if loadgen.MatchesEvent(scheduled, message.Call, move) {
						row.Event, matches = index, matches+1
					}
				}
				if matches != 1 {
					row.Event = -1
				}
				if move.Kind == "move" || move.Kind == "takeover" {
					row.Kind = move.Kind
				}
				if d.Mapped {
					row.Start, row.End = numericInterval(d.Start, message.Call.StartedAt), numericInterval(d.End, message.Call.StartedAt)
				}
				for _, name := range []string{"resume", "audio_freshness", "video_freshness", "first_content", "first_new_content", "first_live", "kill_to_live"} {
					if interval, ok := d.Durations[name]; ok {
						row.Durations[name] = interval
					}
				}
				if d.KillToLive != nil {
					row.Durations["kill_to_live"] = *d.KillToLive
				}
				for _, gap := range d.Gaps {
					if (gap.Kind == "audio" || gap.Kind == "video") && len(row.Gaps) < 2 {
						row.Gaps = append(row.Gaps, NumericGap{Kind: gap.Kind, BaselineP95: gap.BaselineP95, EventGap: gap.EventGap})
					} else {
						row.OmittedGaps++
					}
				}
				row.Timing = publicTiming(d.Timing)
				raw, err := json.Marshal(row)
				if err != nil || len(raw) > maxNumericBytes {
					out.OmittedEvents++
					continue
				}
				for len(out.Events) >= maxNumericEvents || bytes+len(raw) > maxNumericBytes {
					bytes -= sizes[0]
					out.Events[0] = NumericEvent{}
					out.Events, sizes = out.Events[1:], sizes[1:]
					out.OmittedEvents++
				}
				bytes += len(raw)
				sizes = append(sizes, len(raw))
				out.Events = append(out.Events, row)
			}
			call++
		}
	}
	return out
}

func publicTiming(e *callharness.TimingEvidence) *callharness.TimingEvidence {
	if e == nil {
		return nil
	}
	projected := *e
	for _, mapping := range []*callharness.ClockMappingEvidence{&projected.Start, &projected.End} {
		if len(mapping.Probes) > callharness.MaxTimingProbesPerEndpoint {
			mapping.OmittedProbes += len(mapping.Probes) - callharness.MaxTimingProbesPerEndpoint
			mapping.Probes = mapping.Probes[:callharness.MaxTimingProbesPerEndpoint]
		}
	}
	candidates := e.Candidates
	if len(candidates) > callharness.MaxTimingCandidateStates {
		projected.OmittedCandidates += len(candidates) - callharness.MaxTimingCandidateStates
		candidates = candidates[:callharness.MaxTimingCandidateStates]
	}
	projected.Candidates = append([]callharness.ClassificationCandidate{}, candidates...)
	for i := range projected.Candidates {
		row := &projected.Candidates[i]
		row.Metric = PublicMetric(row.Metric)
		switch row.Label {
		case "", "cache", "keyframe", "outage media", "natural keyframe", "live", "unknown":
		default:
			row.Label = "unclassified"
		}
	}
	return &projected
}
