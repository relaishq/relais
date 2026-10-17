package callharness

import (
	"fmt"
	"math/rand/v2"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Frozen pre-118 counting oracle.
func legacyCountContentLoss(units []contentUnit, start, end, until time.Duration) ContentLossCounts {
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

func TestContentLossWindowEquivalent(t *testing.T) {
	rng := rand.New(rand.NewPCG(118, 3))
	var units []contentUnit
	for i := 0; i < 1000; i++ {
		at := time.Duration(rng.IntN(10000)) * time.Millisecond
		units = append(units, contentUnit{at: at, returnedAt: at + time.Duration(rng.IntN(1000))*time.Millisecond, identity: fmt.Sprint(i), written: i%7 != 0})
	}
	for trial := 0; trial < 1000; trial++ {
		start := time.Duration(rng.IntN(10000)) * time.Millisecond
		end := start + time.Duration(rng.IntN(2000)-100)*time.Millisecond
		until := end + time.Duration(rng.IntN(1000))*time.Millisecond
		require.Equal(t, legacyCountContentLoss(units, start, end, until), countContentLoss(units, start, end, until))
	}
}
func TestIndexedFirstContentDurationEquivalent(t *testing.T) {
	r, _ := denseDistributedCall(t, true, 50*time.Millisecond, time.Millisecond)
	for i := range r.tracks[1].video.frames {
		if i%5 == 0 {
			r.tracks[1].video.frames[i].decodable = false
		}
	}
	r.prepareMeasurementIndex()
	prepared := r.prepared
	for _, resume := range []time.Duration{0, time.Second, 10 * time.Second, 10500 * time.Millisecond, 180 * time.Second} {
		for _, limit := range []time.Duration{resume, resume + 100*time.Millisecond, 185 * time.Second} {
			m := EventMeasurement{VideoExpected: true, MediaResumedAt: resume}
			p := MoveReport{Start: resume - time.Second}
			a, b := map[string]DurationInterval{}, map[string]DurationInterval{}
			r.prepared = nil
			measurementDurations(a, p, m, r, limit)
			r.prepared = prepared
			measurementDurations(b, p, m, r, limit)
			require.Equal(t, a, b)
		}
	}
}
