package main

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/relais/internal/nettopology"
	"github.com/relais/pkg/callharness"
	"github.com/relais/pkg/controlplane"
)

const profileGapMargin = 20 * time.Millisecond

// Content age includes caller-to-relay and relay-to-caller transit. Sum the
// jitter on those applied endpoints; private-link jitter is outside this budget.
func profileContentAgeSpreadLimit(links []nettopology.LinkSetting) time.Duration {
	limit := 20 * time.Millisecond
	for _, link := range links {
		if link.Role == "caller" {
			limit += link.Jitter
		}
	}
	return limit
}

var errProfileTrialFailed = errors.New("definite profile trial failure")

func definiteProfileFailure(reason string) error {
	return fmt.Errorf("%w: %s", errProfileTrialFailed, reason)
}

func profileFailureRow(trial int, err error) profileRow {
	definite := errors.Is(err, errProfileTrialFailed)
	return profileRow{Trial: trial, TrialError: err.Error(), Integrity: callharness.MetricVerdict{Trusted: definite, Failed: definite, Reasons: []string{"trial could not supply caller measurement: " + err.Error()}}}
}

type profileGapWindow struct {
	Start, End, PlannedReturnBound, BaselineReturnEstimate, Margin time.Duration
	Complete                                                       bool
}

type profileProbeVerdict struct {
	Verdict            string
	Trials             int
	AudioGap, VideoGap time.Duration
	MediaResumed       bool
	Reasons            []string
}

func profileCallerDelay(links []nettopology.LinkSetting) (delay, jitter time.Duration, found bool) {
	for _, link := range links {
		if link.Role == "caller" {
			if !found || link.Delay+link.Jitter > delay+jitter {
				delay, jitter = link.Delay, link.Jitter
			}
			found = true
		}
	}
	return
}

// Include the first arrival after the extended window, so a gap crossing its
// right boundary is measured too. Incomplete history cannot prove a gap.
func profileEventGap(arrivals []time.Duration, start, end time.Duration) (time.Duration, bool) {
	if len(arrivals) < 2 {
		return 0, false
	}
	before := sort.Search(len(arrivals), func(i int) bool { return arrivals[i] > start }) - 1
	after := sort.Search(len(arrivals), func(i int) bool { return arrivals[i] > end })
	gap := time.Duration(0)
	for i := max(0, before); i < after && i+1 < len(arrivals); i++ {
		gap = max(gap, arrivals[i+1]-arrivals[i])
	}
	return gap, before >= 0 && after < len(arrivals)
}

func measureProfileGap(report *callharness.Report, event callharness.MoveReport, kind string, links []nettopology.LinkSetting) (time.Duration, profileGapWindow, []string) {
	delay, jitter, found := profileCallerDelay(links)
	baseline := event.Measurement.Audio
	if kind == "video" {
		baseline = event.Measurement.Video
	}
	window := profileGapWindow{Start: event.Start, PlannedReturnBound: delay + jitter, BaselineReturnEstimate: baseline.BaselineP95 / 2, Margin: profileGapMargin}
	window.End = max(event.Start, event.End) + max(window.PlannedReturnBound, window.BaselineReturnEstimate) + window.Margin
	var reasons []string
	if baseline.BaselineSamples < 10 {
		reasons = append(reasons, kind+" content-age baseline unavailable for return-delay cross-check")
	}
	if !found {
		reasons = append(reasons, "caller-link plan unavailable")
	}
	// Content age includes the outbound and return legs. Its half-RTT estimate
	// must agree with the caller link envelope, allowing DC/processing margin.
	if baseline.BaselineMedian < 2*max(0, delay-jitter)-profileGapMargin || baseline.BaselineMedian > 2*(delay+jitter)+profileGapMargin {
		reasons = append(reasons, kind+" content-age baseline disagrees with caller-link return delay")
	}
	for _, track := range report.Tracks {
		if track.Kind == kind {
			gap, complete := profileEventGap(track.Arrivals, window.Start, window.End)
			window.Complete = complete
			if !complete {
				reasons = append(reasons, kind+" extended event-gap history unavailable")
			}
			return gap, window, reasons
		}
	}
	return 0, window, append(reasons, kind+" arrival track unavailable")
}

func judgeProfileProbe(report *callharness.Report, event callharness.MoveReport, links []nettopology.LinkSetting, barrier bool) profileProbeVerdict {
	out := profileProbeVerdict{Verdict: "pass", Trials: 1, MediaResumed: true}
	fail, unknown := false, false
	for _, kind := range []string{"audio", "video"} {
		gap, _, reasons := measureProfileGap(report, event, kind, links)
		if kind == "audio" {
			out.AudioGap = gap
		} else {
			out.VideoGap = gap
		}
		if len(reasons) > 0 {
			unknown = true
			out.Reasons = append(out.Reasons, reasons...)
		}
		resumed := false
		for _, track := range event.Tracks {
			if track.Kind == kind && track.PacketsAfter > 0 {
				resumed = true
			}
		}
		if !resumed {
			out.MediaResumed = false
			fail = true
			out.Reasons = append(out.Reasons, kind+" probe media did not resume")
		}
		if barrier && gap > 225*time.Millisecond || !barrier && gap >= 2*time.Second {
			fail = true
			out.Reasons = append(out.Reasons, kind+" probe gap exceeds ceiling")
		}
	}
	if event.Kind != "move" || barrier && event.Error != "" || event.Measurement.ResumeVerdict.Trusted && event.Measurement.ResumeVerdict.Failed {
		fail = true
		out.Reasons = append(out.Reasons, "probe event failed or media did not resume")
	}
	if fail {
		out.Verdict = "fail"
	} else if unknown {
		out.Verdict = "inconclusive"
	}
	return out
}

func poolProfileProbe(out *profileProbeVerdict, in profileProbeVerdict) {
	if out.Verdict == "" {
		out.Verdict = "pass"
		out.MediaResumed = true
	}
	if in.Verdict == "fail" {
		out.Verdict = "fail"
	} else if in.Verdict != "pass" && out.Verdict != "fail" {
		out.Verdict = "inconclusive"
	}
	out.Trials += in.Trials
	out.AudioGap = max(out.AudioGap, in.AudioGap)
	out.VideoGap = max(out.VideoGap, in.VideoGap)
	out.MediaResumed = out.MediaResumed && in.MediaResumed
	out.Reasons = append(out.Reasons, in.Reasons...)
}

// crashTakeovers selects the automatic crash events from the status history.
func crashTakeovers(events []controlplane.MoveResult) []controlplane.MoveResult {
	var out []controlplane.MoveResult
	for _, event := range events {
		if event.Kind == "takeover" {
			out = append(out, event)
		}
	}
	return out
}

// A process counter is evidence only when both samples exist and its value
// did not reset. Count paths observed during this trial, not an absolute total.
func observedUncertainResumeDelta(before float64, beforeAvailable bool, after float64, afterAvailable bool) (uint64, bool) {
	if !beforeAvailable || !afterAvailable || after < before {
		return 0, false
	}
	for _, value := range []float64{before, after} {
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value >= math.Exp2(64) || value != math.Trunc(value) {
			return 0, false
		}
	}
	return uint64(after - before), true
}
