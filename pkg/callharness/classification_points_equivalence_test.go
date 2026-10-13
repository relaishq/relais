package callharness

import (
	"github.com/stretchr/testify/require"
	"slices"
	"sort"
	"testing"
	"time"
)

// Frozen pre-118 boundary builder.
func (r *recorder) legacyEventClassificationPoints(limit time.Duration, floors []time.Duration) (starts, ends []time.Duration) {
	policy := r.freshnessPolicy.defaults()
	starts = []time.Duration{0, r.historyStart + baselineWindow, limit - policy.EvaluationWindow()}
	ends = []time.Duration{limit - contentWindowMargin - contentSettleWindow}
	for _, floor := range floors {
		starts = append(starts, floor+baselineWindow)
	}
	unit := func(u contentUnit) {
		starts = append(starts, u.at, u.at+contentWindowMargin, u.returnedAt, u.returnedAt+baselineWindow, u.returnedAt-policy.EvaluationWindow(), u.returnedAt-policy.RecoveryLimit)
		ends = append(ends, u.at-contentWindowMargin, u.returnedAt-contentWindowMargin-contentSettleWindow, u.returnedAt)
	}
	for _, u := range r.sentAudioUnits {
		unit(u)
	}
	for _, f := range r.sentVideoFrames {
		unit(f.unit())
		starts = append(starts, f.requestedAt)
	}
	for _, u := range r.repeatedAudioReturns {
		unit(u)
	}
	for _, u := range r.repeatedVideoReturns {
		unit(u)
	}
	for _, t := range r.tracks {
		// Only margin jumps and outage-gap ends can change takeover resume.
		// Ordinary RTP packet boundaries do not affect start classifications.
		interval := opusFrameDuration
		if t.video != nil {
			interval = r.frameInterval
			if interval == 0 {
				interval = time.Second / 30
			}
		}
		for i := 1; i < len(t.arrivals); i++ {
			gap := t.arrivals[i] - t.arrivals[i-1]
			margin := i < len(t.headers) && int16(t.headers[i].seq-t.headers[i-1].seq) >= 8192-5500
			if margin || gap > max(50*time.Millisecond, 3*interval) {
				starts = append(starts, t.arrivals[i])
			}
		}
		starts = append(starts, t.unmatchedTimes...)
		for _, at := range t.unmatchedTimes {
			starts = append(starts, at+baselineWindow, at-policy.EvaluationWindow())
		}

		if t.video != nil {
			for _, f := range t.video.frames {
				starts = append(starts, f.at, f.source.at, f.source.returnedAt, f.source.requestedAt, f.source.at+contentWindowMargin)
				ends = append(ends, f.at, f.source.at, f.source.returnedAt)
				unit(f.source.unit())
			}
			for _, s := range t.video.sampleResults {
				starts = append(starts, s.at)
			}
		}
	}
	// For a move, resume is the first RTP arrival at/after end. Its
	// classification changes only when that arrival crosses a send, return,
	// decode or content-window boundary. Dense fragments between two such
	// observations need only their first/last resume values (the endpoints
	// already cover duration extrema). Retain the predecessor packet at each
	// boundary, with adjacent-nanosecond evaluations supplied by the caller.
	var packets []time.Duration
	for _, t := range r.tracks {
		packets = append(packets, t.arrivals...)
	}
	slices.Sort(packets)
	thresholds := slices.Clone(ends)
	for _, u := range r.sentAudioUnits {
		thresholds = append(thresholds, u.at, u.returnedAt)
	}
	for _, f := range r.sentVideoFrames {
		thresholds = append(thresholds, f.at, f.returnedAt)
	}
	for _, u := range r.repeatedAudioReturns {
		thresholds = append(thresholds, u.at, u.returnedAt)
	}
	for _, u := range r.repeatedVideoReturns {
		thresholds = append(thresholds, u.at, u.returnedAt)
	}
	if len(packets) > 0 {
		ends = append(ends, packets[len(packets)-1])
		for _, at := range thresholds {
			i := sort.Search(len(packets), func(i int) bool { return packets[i] > at }) - 1
			if i >= 0 {
				ends = append(ends, packets[i])
			}
		}
	}
	return starts, ends
}
func TestClassificationPointsEquivalent(t *testing.T) {
	r, _ := denseDistributedCall(t, true, 50*time.Millisecond, time.Millisecond)
	floors := []time.Duration{0, 4 * time.Second, 9500 * time.Millisecond, 170 * time.Second}
	for _, span := range []DurationInterval{{}, {Lower: 28 * time.Millisecond, Upper: 32 * time.Millisecond}} {
		for _, lo := range []time.Duration{0, 5 * time.Second, 10 * time.Second, 160 * time.Second, 184 * time.Second} {
			hi := lo + 52*time.Millisecond
			endLo, endHi := lo+400*time.Millisecond, lo+452*time.Millisecond
			sr := DurationInterval{Lower: lo, Upper: hi}
			er := DurationInterval{Lower: min(endLo, lo+span.Lower), Upper: max(endHi, hi+span.Upper)}
			a, b := r.legacyEventClassificationPoints(r.hungUpAt, floors)
			c, d := r.eventClassificationPoints(r.hungUpAt, floors, sr, er)
			if span.Upper > 0 {
				for _, at := range b {
					a = append(a, at-span.Lower, at-span.Upper)
				}
				for _, at := range d {
					c = append(c, at-span.Lower, at-span.Upper)
				}
			}
			require.Equal(t, classificationPoints(lo, hi, a), classificationPoints(lo, hi, c))
			require.Equal(t, classificationPoints(endLo, endHi, b), classificationPoints(endLo, endHi, d))
		}
	}
}
