package main

import (
	"fmt"
	"time"

	"github.com/relaishq/relais/internal/loadgen"
	"github.com/relaishq/relais/pkg/callharness"
)

// Generator overload rules shared by the soak (#128) and capacity (#133)
// judges. Raw samples and the strict processes[].saturated field are left
// unchanged. Calibration keeps that strict single-sample gate, because it
// measures where one generator process saturates.

// maxToleratedStall is the largest stall whose shortened gaps the caller's
// whole-call gap inventory still retains (callharness.MediaGapRetention).
const maxToleratedStall = callharness.MaxToleratedStall

// overLimit reports a sample above the lateness or skipped-frame limit.
func overLimit(s loadgen.Stats, l loadgen.Limits) bool {
	return s.IntervalLateness.P99 > l.P99 || s.IntervalPacing.SkippedPercent() > l.SkippedPercent
}

// flagged reports a sample above any generator limit.
func flagged(s loadgen.Stats, l loadgen.Limits) bool {
	return overLimit(s, l) || s.CPUPerGOMAXPROCS > l.CPUPercent
}

// saturationCauses returns why sample i shows generator overload, or nil.
// One modest, isolated excess is a tick slip, not sustained overload: interval
// send-lateness p99 or skipped frames above the limit but within 2x the limit,
// in a sample whose neighbours are within both limits. It is tolerated only
// while its maximum lateness keeps the stall within MaxToleratedStall, the
// largest stall whose shortened gaps the caller still retains. CPU excess,
// either excess above 2x the limit, a longer stall, or an excess in two
// consecutive samples (both are saturated) still counts.
func saturationCauses(samples []loadgen.Stats, i int, l loadgen.Limits, start, end time.Time) []string {
	s := samples[i]
	skipped := s.IntervalPacing.SkippedPercent()
	immediate := s.CPUPerGOMAXPROCS > l.CPUPercent || s.IntervalLateness.P99 > 2*l.P99 || skipped > 2*l.SkippedPercent
	stall := overLimit(s, l) && s.IntervalLateness.Max+soakAudioTick > maxToleratedStall
	if !immediate && !stall {
		if overLimit(s, l) && consecutiveExcess(samples, i, l, start, end) {
			return append(l.Causes(s), "limit excess in consecutive samples")
		}
		return nil
	}
	var causes []string
	if immediate {
		// Name a lateness or skipped-frame excess only when it is severe alone;
		// each immediate signal still names itself.
		shown := l
		if s.IntervalLateness.P99 <= 2*l.P99 {
			shown.P99 = max(shown.P99, s.IntervalLateness.P99)
		}
		if skipped <= 2*l.SkippedPercent {
			shown.SkippedPercent = max(shown.SkippedPercent, skipped)
		}
		causes = shown.Causes(s)
	}
	if stall {
		causes = append(causes, fmt.Sprintf("interval maximum send-lateness %s exceeds the %s tolerated stall", s.IntervalLateness.Max, maxToleratedStall-soakAudioTick))
	}
	if len(causes) == 0 { // A saturated sample always names a cause.
		causes = append(l.Causes(s), "generator limit excess")
	}
	return causes
}

func sampleSaturated(samples []loadgen.Stats, i int, l loadgen.Limits, start, end time.Time) bool {
	return len(saturationCauses(samples, i, l, start, end)) > 0
}

// mediaEnd is when process p's last call stops sending media: the run window
// end plus its largest seeded start offset (#85). Reports without offsets end
// at the run window.
func mediaEnd(r loadgen.RunReport, p loadgen.ProcessReport) time.Time {
	var offset time.Duration
	for _, c := range p.Calls {
		offset = max(offset, c.MediaOffset)
	}
	return r.StartedAt.Add(r.Duration + offset)
}

// consecutiveExcess reports a time-adjacent neighbour of sample i that is
// also above a limit. Startup or hangup-only samples cannot establish
// persistence: a neighbour must overlap [start, end), where end is the
// process's media end (mediaEnd).
func consecutiveExcess(samples []loadgen.Stats, i int, l loadgen.Limits, start, end time.Time) bool {
	s := samples[i]
	for _, j := range []int{i - 1, i + 1} {
		if j < 0 || j >= len(samples) || !flagged(samples[j], l) {
			continue
		}
		if !samples[j].At.After(start) || !samples[j].At.Add(-samples[j].Interval).Before(end) {
			continue
		}
		previous, next := s, samples[j]
		if j < i {
			previous, next = samples[j], s
		}
		// Samples must cover consecutive time, not just be adjacent entries in
		// an incomplete report. Permit only timestamp rounding/clock noise.
		gap := next.At.Add(-next.Interval).Sub(previous.At)
		if previous.Interval > 0 && next.Interval > 0 && next.At.After(previous.At) && gap >= -time.Millisecond && gap <= time.Millisecond {
			return true
		}
	}
	return false
}

// stallBound bounds how much a caller stall can shorten a gap measured in
// [from, to]. A stall that covers the last packet before an outage stamps it
// late after ReadRTP. With GOMAXPROCS=1 a process stall of D delays the next
// send by at least D minus one tick, so D <= interval max lateness + one tick.
// Only samples whose maximum exceeds the limit count; sends within the limit
// are the existing measurement envelope.
func stallBound(samples []loadgen.Stats, limit time.Duration, from, to time.Time) time.Duration {
	var bound time.Duration
	for _, s := range samples {
		if s.IntervalLateness.Max > limit && s.At.After(from) && s.At.Add(-s.Interval).Before(to) {
			bound = max(bound, s.IntervalLateness.Max+soakAudioTick)
		}
	}
	return bound
}

// stalledGapCause returns why an unscheduled whole-call gap at or below the
// ceiling is inconclusive, or "". This caller's own stall, from samples that
// overlap [gap start - 1 s, gap end], could have shortened it from above the
// ceiling. A gap above the ceiling keeps its existing judgement.
func stalledGapCause(r loadgen.RunReport, p loadgen.ProcessReport, session string, report *callharness.Report, kind string, end, gap time.Duration) string {
	if gap > callharness.MediaGapCeiling || !unscheduledGap(r.Events, session, report, end, gap) {
		return ""
	}
	at := report.StartedAt.Add(end)
	bound := stallBound(p.Samples, r.Limits.P99, at.Add(-gap-time.Second), at)
	if gap+bound <= callharness.MediaGapCeiling {
		return ""
	}
	return fmt.Sprintf("%s unscheduled media gap %s ended at %s is within this caller's stall bound %s of %s", kind, gap, end, bound, callharness.MediaGapCeiling)
}

// gapInventoryFullCause returns why a full gap inventory is inconclusive, or
// "". Overflow counts only dropped gaps above the ceiling, so a full
// inventory may have dropped near-ceiling gaps that a stall shortened.
func gapInventoryFullCause(track callharness.TrackReport) string {
	if len(track.MediaGaps) < callharness.MaxRetainedMediaGaps {
		return ""
	}
	return track.Kind + " whole-call gap inventory is full; later near-ceiling gaps may be missing"
}
