package callharness

import (
	"sort"
	"time"
)

// ContentLossCounts counts identified, successfully written content, once per
// audio unit or complete video frame. Loss means no valid return by the same
// observation horizon used by EventMeasurement, independent of decoding.
type ContentLossCounts struct {
	Sent, Lost     int
	Trusted        bool
	LongestLostRun int
	RunTrusted     bool
}

type ContentLossWindow struct {
	Start, End   time.Duration
	Audio, Video ContentLossCounts
}

// ContentLossEvidence retains raw denominators and matched no-event windows.
// Baselines have the event window's exact length, do not overlap each other,
// and end at least one settle window before the event window starts. It adds
// evidence without changing the caller yardstick's exact zero-loss verdicts.
type ContentLossEvidence struct {
	Event     ContentLossWindow
	Baselines []ContentLossWindow
}

func countContentLoss(units []contentUnit, start, end, until time.Duration) ContentLossCounts {
	out := ContentLossCounts{Trusted: true}
	window := EventMeasurement{WindowStart: start, WindowEnd: end, ObservedUntil: until}
	ordered := append([]contentUnit(nil), units...)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].at == ordered[j].at {
			return ordered[i].identity < ordered[j].identity
		}
		return ordered[i].at < ordered[j].at
	})
	run := 0
	for _, unit := range ordered {
		if !unit.written || unit.at < start || unit.at >= end {
			continue
		}
		out.Sent++
		if missingInWindow(unit, window) {
			out.Lost++
			run++
			out.LongestLostRun = max(out.LongestLostRun, run)
		} else {
			run = 0
		}
	}
	out.Trusted = out.Sent > 0
	out.RunTrusted = out.Trusted
	return out
}

func (r *recorder) contentLossEvidence(move MoveReport, m EventMeasurement, audio, video []contentUnit, floor time.Duration) ContentLossEvidence {
	window := func(start, end time.Duration) ContentLossWindow {
		w := ContentLossWindow{Start: start, End: end,
			Audio: countContentLoss(audio, start, end, m.ObservedUntil),
			Video: countContentLoss(video, start, end, m.ObservedUntil)}
		covered := start >= r.historyStart && m.ObservedUntil >= end+contentSettleWindow
		w.Audio.Trusted = w.Audio.Trusted && covered
		w.Video.Trusted = w.Video.Trusted && covered && r.contentIdentityErrors == 0
		return w
	}
	out := ContentLossEvidence{Event: window(m.WindowStart, m.WindowEnd)}
	width := m.WindowEnd - m.WindowStart
	if width <= 0 {
		return out
	}
	floor = max(floor, r.connectedAt, r.historyStart)
	// Exclude earlier moves/probes as well as takeovers. Retained summaries
	// and pending records both matter when a long call has compacted history.
	for _, previous := range r.savedMoves {
		if previous.Start < move.Start {
			floor = max(floor, previous.Measurement.WindowEnd+contentSettleWindow, previous.End+contentWindowMargin+contentSettleWindow)
		}
	}
	for _, previous := range r.moves {
		if r.since(previous.start) < move.Start {
			floor = max(floor, r.since(previous.end)+contentWindowMargin+contentSettleWindow)
		}
	}
	end := m.WindowStart - contentSettleWindow
	for i := 0; i < 3 && end-width >= floor; i++ {
		out.Baselines = append(out.Baselines, window(end-width, end))
		end -= width
	}
	return out
}
