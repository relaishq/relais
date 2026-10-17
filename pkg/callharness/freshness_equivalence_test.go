package callharness

import (
	"cmp"
	"math/rand/v2"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Frozen pre-118 yardstick oracle.
func legacyMeasureFreshness(units []contentUnit, start, resumed, limit, baselineFloor time.Duration, policies ...FreshnessPolicy) FreshnessReport {
	policy := FreshnessPolicy{}.defaults()
	if len(policies) > 0 {
		policy = policies[0].defaults()
	}
	f := FreshnessReport{RecoveryLimit: policy.RecoveryLimit, StabilityWindow: policy.StabilityWindow, EvaluationEnd: min(limit, start+policy.RecoveryLimit+policy.StabilityWindow), BaselineStart: max(0, start-baselineWindow, baselineFloor), Tolerance: freshnessTolerance, SustainedUnits: freshnessSustainedUnits, SpreadLimit: baselineSpreadLimit, LateWindowUnits: freshnessLateWindowUnits, LateWindowLimit: freshnessLateWindowLimit, LatePercentLimit: freshnessLatePercentLimit, AbsoluteLatencyCeiling: policy.AbsoluteLatencyCeiling, LiveLatenessCap: policy.LiveLatenessCap}
	f.StabilityObserved = max(0, f.EvaluationEnd-start-f.RecoveryLimit)
	var baseline []time.Duration
	var post []contentUnit
	seen := map[string]bool{}
	seenPost := map[string]bool{}
	slices.SortFunc(units, func(a, b contentUnit) int { return cmp.Compare(a.returnedAt, b.returnedAt) })
	for i, unit := range units {
		if unit.returnedAt == 0 || unit.returnedAt < unit.at {
			continue
		}
		if unit.identity == "" {
			unit.identity = strconv.Itoa(i)
		}
		if !seen[unit.identity] && !unit.replayed && unit.returnedAt >= f.BaselineStart && unit.returnedAt < start && unit.at < start {
			baseline = append(baseline, unit.returnedAt-unit.at)
		}
		seen[unit.identity] = true
		if unit.returnedAt >= start && unit.returnedAt <= f.EvaluationEnd {
			f.PeakLatency = max(f.PeakLatency, unit.returnedAt-unit.at)
			post = append(post, unit)
			seenPost[unit.identity] = true
		}
	}
	f.BaselineSamples, f.PostEventSamples = len(baseline), len(seenPost)
	slices.Sort(baseline)
	if len(baseline) > 0 {
		f.BaselineMedian = baseline[len(baseline)/2]
		f.BaselineP95 = baseline[(95*len(baseline)+99)/100-1]
	}
	f.FirstBaselineSamples, f.FirstBaselineMedian, f.FirstBaselineP95 = f.BaselineSamples, f.BaselineMedian, f.BaselineP95
	var runStart time.Duration
	sustained, lateLive := 0, 0
	var lateExcess time.Duration
	flushJitter := func() {
		if lateLive > 0 && lateLive < f.SustainedUnits {
			f.JitterUnits += lateLive
			f.JitterMaxExcess = max(f.JitterMaxExcess, lateExcess)
		}
		lateLive, lateExcess = 0, 0
	}
	var recent [freshnessLateWindowUnits]bool
	recentNext, recentLate, runLive, runLate := 0, 0, 0, 0
	resetRun := func() {
		sustained, runStart, recentNext, recentLate, runLive, runLate = 0, 0, 0, 0, 0, 0
		recent = [freshnessLateWindowUnits]bool{}
	}
	counted := map[string]bool{}
	for _, unit := range post {
		latency := unit.returnedAt - unit.at
		excess := latency - f.BaselineP95 - f.Tolerance
		if excess > 0 {
			f.LastAboveTolerance = unit.returnedAt - start
		}
		if resumed == 0 || unit.returnedAt < resumed {
			continue
		}
		switch {
		case unit.at < resumed:
			flushJitter()
			resetRun()
			counted[unit.identity] = true
			continue
		case latency-f.BaselineP95 > f.LiveLatenessCap:
			flushJitter()
			resetRun()
			counted[unit.identity] = true
			f.LiveUnits++
			f.LateLiveUnits++
			continue
		case excess > 0:
			lateLive++
			lateExcess = max(lateExcess, excess)

		default:
			flushJitter()
			if counted[unit.identity] || unit.replayed {
				continue
			}
			if sustained == 0 {
				runStart = unit.returnedAt - start
			}
			sustained++
		}
		counted[unit.identity] = true
		f.LiveUnits++
		late := excess > 0
		if late {
			f.LateLiveUnits++
		}
		if sustained == 0 {
			continue
		}
		runLive++
		if recent[recentNext] {
			recentLate--
		}
		recent[recentNext] = late
		recentNext = (recentNext + 1) % len(recent)
		if late {
			recentLate++
			runLate++
		}
		if recentLate >= f.LateWindowLimit {
			resetRun()
		}
	}
	flushJitter()
	f.Recovered = resumed > 0 && sustained >= f.SustainedUnits && runLate*100 <= runLive*f.LatePercentLimit
	if f.Recovered {
		f.BackToBaseline = runStart
	}
	f.PeakAboveBaseline = max(0, f.PeakLatency-f.BaselineP95)
	f.FirstBaselineTrusted = f.baselineTrusted()
	f.updateVerdict()
	return f
}

func TestFreshnessWorkspaceEquivalent(t *testing.T) {
	rng := rand.New(rand.NewPCG(118, 1))
	var work freshnessWorkspace
	for trial := 0; trial < 100; trial++ {
		var units []contentUnit
		for i := 0; i < 400; i++ {
			at := time.Duration(i) * 20 * time.Millisecond
			u := contentUnit{at: at, returnedAt: at + time.Duration(rng.IntN(250))*time.Millisecond, identity: strconv.Itoa(i), written: true}
			if i%17 == 0 {
				u.returnedAt = 0
			}
			if i%19 == 0 {
				u.returnedAt = at - time.Millisecond
			}
			units = append(units, u)
			if i%13 == 0 {
				u.returnedAt += 300 * time.Millisecond
				u.replayed = true
				units = append(units, u)
			}
		}
		start := time.Duration(rng.IntN(5000)) * time.Millisecond
		resumed := start + time.Duration(rng.IntN(1000))*time.Millisecond
		if trial%11 == 0 {
			resumed = 0
		}
		floor := time.Duration(rng.IntN(3000)) * time.Millisecond
		limit := start + time.Duration(rng.IntN(6000))*time.Millisecond
		expected := legacyMeasureFreshness(slices.Clone(units), start, resumed, limit, floor)
		actual := measureFreshnessWithWorkspace(slices.Clone(units), start, resumed, limit, floor, &work)
		require.Equal(t, expected, actual, "trial %d", trial)
	}
}

func TestIndexedFreshnessCacheEquivalent(t *testing.T) {
	r := newRecorder()
	rng := rand.New(rand.NewPCG(118, 2))
	for i := 0; i < 400; i++ {
		at := time.Duration(i) * 20 * time.Millisecond
		key := strconv.Itoa(i)
		u := contentUnit{at: at, returnedAt: at + time.Duration(rng.IntN(250))*time.Millisecond, identity: key, written: true}
		if i%17 == 0 {
			u.returnedAt = 0
		}
		if i%19 == 0 {
			u.returnedAt = at - time.Millisecond
		}
		r.sentAudioUnits[key] = u
		if i%13 == 0 {
			u.returnedAt += 300 * time.Millisecond
			u.replayed = true
			r.repeatedAudioReturns = append(r.repeatedAudioReturns, u)
			// Missing identities use the original slice-index identity. This can
			// collide with an explicit numeric identity in directly built fixtures.
			u.identity = ""
			r.repeatedAudioReturns = append(r.repeatedAudioReturns, u)
		}
	}
	r.prepareMeasurementIndex()
	work := &r.prepared.audioWork
	work.ordered = true
	for _, start := range []time.Duration{time.Second, 3 * time.Second, 6 * time.Second} {
		for _, resumed := range []time.Duration{0, start, start + 500*time.Millisecond} {
			for _, limit := range []time.Duration{start + time.Second, 9 * time.Second} {
				for floor := time.Duration(0); floor < 9*time.Second; floor += 7 * time.Millisecond {
					lower := min(start, max(0, start-baselineWindow, floor))
					units := r.indexedUnits(false, true, lower, min(limit, start+r.freshnessPolicy.EvaluationWindow()))
					expected := legacyMeasureFreshness(slices.Clone(units), start, resumed, limit, floor)
					actual := r.measureFreshnessCell(units, start, resumed, limit, floor, work)
					require.Equal(t, expected, actual, "start=%s resume=%s limit=%s floor=%s", start, resumed, limit, floor)
				}
			}
		}
	}
}

func TestFreshnessCacheReasonsDoNotAlias(t *testing.T) {
	r := newRecorder()
	work := &freshnessWorkspace{ordered: true}
	units := []contentUnit{
		{at: time.Second, returnedAt: 1010 * time.Millisecond, identity: "a", written: true},
		{at: 1100 * time.Millisecond, returnedAt: 1110 * time.Millisecond, identity: "b", written: true},
		{at: 1200 * time.Millisecond, returnedAt: 1400 * time.Millisecond, identity: "c", written: true},
	}
	first := r.measureFreshnessCell(units, 2*time.Second, 2*time.Second, 2100*time.Millisecond, 0, work)
	require.Len(t, first.Verdict.Reasons, 3)
	first.Verdict.Reasons = append(first.Verdict.Reasons, "first consumer")
	second := r.measureFreshnessCell(units, 2*time.Second, 2*time.Second, 2100*time.Millisecond, 0, work)
	second.Verdict.Reasons = append(second.Verdict.Reasons, "second consumer")
	require.Equal(t, "first consumer", first.Verdict.Reasons[3])
	require.Equal(t, "second consumer", second.Verdict.Reasons[3])
}
