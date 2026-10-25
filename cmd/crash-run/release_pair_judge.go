package main

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/relaishq/relais/pkg/callharness"
)

// Planned-move gates match the load judges (#40): p95 of each track's move gap
// at most 150 ms, and at most 50 ms above its own baseline gap p95, applied to
// each rollout phase separately. A relay standby handover keeps the
// relay-restart limit of 1 s. Content uses the crash-run and lean judges' rules.
const (
	pairMoveGapCeiling   = callharness.MediaGapCeiling
	pairMoveExcessLimit  = 50 * time.Millisecond
	pairRelayGapLimit    = time.Second
	pairBaselineSamples  = 10
	pairEndOfMediaWindow = 500 * time.Millisecond
	// pairContentWindowMargin is the yardstick's content window margin: an
	// event's loss window is [start-250ms, max(end, media resumed)+250ms).
	pairContentWindowMargin = 250 * time.Millisecond
)

type pairWindow struct {
	Kind, Participant string
	Start, End        time.Time
}

type pairObservation struct {
	Label          string
	LostCount      uint64
	CrashTakeovers int
	Calls          int
	Err            string
}

type pairEvidence struct {
	Reports       []*callharness.Report
	ExpectedCalls int
	RelayMedia    string
	RelayWindows  []pairWindow
	// Phases are the upgrade and rollback windows; each is judged alone.
	Phases        []pairWindow
	ObservedUntil time.Time
	Observations  []pairObservation
	// UnexpectedExits names processes that ended without being replaced, up
	// to the observation boundary.
	UnexpectedExits []string
}

type pairGapSummary struct {
	Phase, Kind       string
	Samples, Trusted  int
	P95Gap, P95Excess time.Duration
	MaxGap            time.Duration
}

type pairPhaseSummary struct {
	Phase                string
	Moves, Conclusive    int
	RelayLostAudio       int
	RelayLostVideoFrames int
}

type pairJudgement struct {
	Outcome                  string
	Failures                 []string
	Unknowns                 []string
	Moves                    int
	Phases                   []pairPhaseSummary
	MoveGaps                 []pairGapSummary
	RelayGapMax              time.Duration
	RelayHandovers           int
	CallerDecryptionFailures int
}

func (j *pairJudgement) fail(format string, args ...any) {
	j.Failures = append(j.Failures, fmt.Sprintf(format, args...))
}
func (j *pairJudgement) unknown(format string, args ...any) {
	j.Unknowns = append(j.Unknowns, fmt.Sprintf(format, args...))
}

func pairP95(values []time.Duration) time.Duration {
	if len(values) == 0 {
		return 0
	}
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	return sorted[(95*len(sorted)+99)/100-1]
}

// pairCovered reports whether gap [start, end] lies between consecutive
// packets that the window's measurement observes. aroundMove spans the last
// packet before Start to the first packet after End, so only a gap that
// begins before End and ends after Start is measured. No margin applies.
func pairCovered(start, end, windowStart, windowEnd time.Duration) bool {
	return start < windowEnd && end > windowStart
}

// pairMoveContent applies crash-run's event-content gates to one planned move:
// a missing measurement or untrusted loss is inconclusive; failed content
// recovery or any trusted lost content fails (planned moves are exact).
func pairMoveContent(j *pairJudgement, call string, m callharness.MoveReport) {
	e := m.Measurement
	if e.SettleWindow <= 0 {
		j.unknown("%s: move %d->%d event measurement not available", call, m.From, m.To)
		return
	}
	if e.ContentRecoveryFailed() {
		j.fail("%s: move %d->%d content recovery failed: %s", call, m.From, m.To, strings.Join(e.Reasons, "; "))
	}
	for _, loss := range []struct {
		kind    string
		verdict callharness.MetricVerdict
		lost    bool
	}{{"audio", e.AudioLoss, e.LostAudio > 0}, {"video", e.VideoLoss, e.LostVideoFrames > 0}} {
		switch {
		case !loss.verdict.Trusted:
			j.unknown("%s: move %d->%d %s loss inconclusive: %s", call, m.From, m.To, loss.kind, loss.verdict.Summary())
		case loss.verdict.Failed || loss.lost:
			j.fail("%s: move %d->%d %s content lost: %s", call, m.From, m.To, loss.kind, loss.verdict.Summary())
		}
	}
}

// pairCallContent applies the whole-call integrity checks of the profile and
// drain judges, and the lean judge's delivery accounting.
func pairCallContent(j *pairJudgement, call string, r *callharness.Report, relay [][2]time.Duration, phaseOf func(time.Duration) int) {
	for _, t := range r.Tracks {
		if t.UnmatchedPayloads > 0 {
			j.fail("%s: %d unmatched %s payloads", call, t.UnmatchedPayloads, t.Kind)
		}
		if t.Video == nil {
			continue
		}
		v := t.Video
		if v.UnmatchedFrames > 0 || v.KeyframeDecodeErrors > 0 {
			j.fail("%s: invalid video content: unmatched=%d keyframe_errors=%d", call, v.UnmatchedFrames, v.KeyframeDecodeErrors)
		}
		switch full := v.FullDecode; {
		case v.OnlineDecode.Every > 0:
			if v.OnlineDecode.Errors > 0 {
				j.fail("%s: %d online video decode errors: %s", call, v.OnlineDecode.Errors, v.OnlineDecode.LastError)
			} else if !v.OnlineDecode.Conclusive() {
				j.unknown("%s: online video decode inconclusive: decoded %d of %d samples", call, v.OnlineDecode.UniqueDecoded, v.OnlineDecode.Sampled)
			}
		case full.Ran() && full.FramesIn > 0:
			if full.Errors != "" || full.FramesDecoded != full.FramesIn {
				j.fail("%s: full video decode %d/%d: %s", call, full.FramesDecoded, full.FramesIn, full.Errors)
			}
		default:
			j.unknown("%s: video decode unavailable: %q", call, full.Skipped)
		}
	}
	d := r.Recording.Delivery
	if d == nil {
		j.unknown("%s: whole-call delivery unavailable", call)
		return
	}
	for _, k := range []struct {
		kind    string
		units   callharness.ContentDelivery
		written int
	}{{"audio", d.Audio, r.SentAudio.Frames}, {"video", d.Video, r.SentVideo.Frames}} {
		u := k.units
		if u.Sent+u.Unsettled != k.written || u.Returned+u.Lost != u.Sent || len(u.LostAt)+u.LostOverflow != u.Lost {
			j.unknown("%s: %s delivery accounting incomplete", call, k.kind)
		}
		if u.LostOverflow > 0 {
			j.fail("%s: %s lost-unit inventory overflow", call, k.kind)
		}
	loss:
		for _, at := range u.LostAt {
			// Loss is delegated to a planned move only inside the window that
			// move's measurement actually judged.
			for _, m := range r.Moves {
				e := m.Measurement
				if e.SettleWindow > 0 && at >= e.WindowStart && at < e.WindowEnd {
					continue loss
				}
			}
			// The relay buffer is not a no-loss log (ADR 0005): report loss in
			// a relay window bounded the yardstick's way, never beyond it.
			for _, w := range relay {
				if at >= w[0] && at < w[1] {
					if p := phaseOf(at); p >= 0 {
						if k.kind == "audio" {
							j.Phases[p].RelayLostAudio++
						} else {
							j.Phases[p].RelayLostVideoFrames++
						}
					}
					continue loss
				}
			}
			j.fail("%s: %s content sent at %s lost outside every scheduled rollout event", call, k.kind, at)
		}
	}
}

// judgeReleasePair is a three-state verdict. Missing or untrusted evidence is
// inconclusive and never a pass; any trusted failure fails.
func judgeReleasePair(e pairEvidence) pairJudgement {
	j := pairJudgement{RelayHandovers: len(e.RelayWindows)}
	for _, p := range e.Phases {
		j.Phases = append(j.Phases, pairPhaseSummary{Phase: p.Kind})
	}
	if len(e.Phases) == 0 {
		j.unknown("no rollout phase windows")
	}
	if e.ExpectedCalls <= 0 || len(e.Reports) != e.ExpectedCalls {
		j.unknown("caller reports %d of %d", len(e.Reports), e.ExpectedCalls)
	}
	if len(e.RelayWindows) == 0 {
		j.unknown("no relay standby handover was observed")
	}
	for _, name := range e.UnexpectedExits {
		j.fail("%s exited unexpectedly before the observation boundary", name)
	}
	if len(e.Observations) == 0 {
		j.unknown("no control-plane ownership observations")
	}
	for _, o := range e.Observations {
		if o.Err != "" {
			j.unknown("%s: ownership status unavailable: %s", o.Label, o.Err)
			continue
		}
		if o.LostCount != 0 {
			j.fail("%s: control reports %d lost calls", o.Label, o.LostCount)
		}
		if o.CrashTakeovers != 0 {
			j.fail("%s: %d crash takeovers during a planned rollout", o.Label, o.CrashTakeovers)
		}
		if e.ExpectedCalls > 0 && o.Calls != e.ExpectedCalls {
			j.fail("%s: control owns %d of %d calls", o.Label, o.Calls, e.ExpectedCalls)
		}
	}
	type pool struct {
		gaps, excess     []time.Duration
		samples, trusted int
		maxGap           time.Duration
	}
	pools := map[string]*pool{}
	at := func(phase int, kind string) *pool {
		key := fmt.Sprint(phase, "/", kind)
		if pools[key] == nil {
			pools[key] = &pool{}
		}
		return pools[key]
	}
	for i, r := range e.Reports {
		call := fmt.Sprintf("call %d", i+1)
		if r == nil {
			j.unknown("%s: no caller report", call)
			continue
		}
		if r.HasDistributedEvidence() {
			j.unknown("%s: distributed evidence is outside this local judge", call)
			continue
		}
		phaseOf := func(offset time.Duration) int {
			instant := r.StartedAt.Add(offset)
			for p, w := range e.Phases {
				if !instant.Before(w.Start) && instant.Before(w.End) {
					return p
				}
			}
			return -1
		}
		if n := r.DecryptionFailures.Total(); n != 0 {
			j.CallerDecryptionFailures += n
			j.fail("%s: %d caller decryption failures", call, n)
		}
		if r.ICERestarts != 0 || r.Renegotiations != 0 || r.OfferAnswerExchanges != 1 {
			j.fail("%s: ICE restarts=%d renegotiations=%d offer/answer=%d", call, r.ICERestarts, r.Renegotiations, r.OfferAnswerExchanges)
		}
		if !r.ConnectedThroughout() {
			j.fail("%s: caller connection left connected", call)
		}
		if r.RemoteAddr != e.RelayMedia {
			j.fail("%s: caller remote %s is not the relay %s", call, r.RemoteAddr, e.RelayMedia)
		}
		if r.Consent.ResponsesAfter == 0 {
			j.fail("%s: no ICE consent responses after the last move", call)
		}
		for _, m := range r.Moves {
			j.Moves++
			phase := phaseOf(m.Start)
			if phase < 0 {
				j.unknown("%s: move %d->%d at %s lies outside every rollout phase", call, m.From, m.To, m.Start)
			}
			if m.Kind != "move" {
				j.fail("%s: unplanned %s during the rollout", call, m.Kind)
			}
			if m.Error != "" || m.Lost {
				j.fail("%s: move %d->%d lost=%t error=%q", call, m.From, m.To, m.Lost, m.Error)
			}
			pairMoveContent(&j, call, m)
			if phase >= 0 {
				j.Phases[phase].Moves++
				if m.Measurement.Conclusive() {
					j.Phases[phase].Conclusive++
				}
			}
			for _, t := range m.Tracks {
				p := at(phase, t.Kind)
				p.samples++
				p.maxGap = max(p.maxGap, t.Gap)
				if t.PacketsAfter == 0 {
					j.fail("%s: no %s packets after move %d->%d", call, t.Kind, m.From, m.To)
					continue
				}
				if t.BaselineGapSamples < pairBaselineSamples {
					continue
				}
				p.trusted++
				p.gaps = append(p.gaps, t.Gap)
				p.excess = append(p.excess, t.Gap-t.BaselineGapP95)
			}
		}
		var relayWindows, relayLoss [][2]time.Duration
		for _, w := range e.RelayWindows {
			start, end := w.Start.Sub(r.StartedAt), w.End.Sub(r.StartedAt)
			relayWindows = append(relayWindows, [2]time.Duration{start, end})
			// Media resumes when this call's handover gap ends, if later.
			resumed := end
			for _, t := range r.Tracks {
				for _, g := range t.MediaGaps {
					if pairCovered(g.EndedAt-g.Duration, g.EndedAt, start, end) {
						resumed = max(resumed, g.EndedAt)
					}
				}
			}
			relayLoss = append(relayLoss, [2]time.Duration{max(0, start-pairContentWindowMargin), resumed + pairContentWindowMargin})
		}
		pairCallContent(&j, call, r, relayLoss, phaseOf)
		kinds := map[string]bool{}
		until := e.ObservedUntil.Sub(r.StartedAt)
		for _, t := range r.Tracks {
			kinds[t.Kind] = true
			if t.Packets == 0 || until-t.LastArrival > pairEndOfMediaWindow {
				j.fail("%s: %s media stopped %s before the end of observation", call, t.Kind, until-t.LastArrival)
			}
			if !t.MediaGapsRecorded || t.MediaGapOverflow > 0 {
				j.unknown("%s: %s whole-call gap inventory incomplete", call, t.Kind)
			}
		gap:
			for _, g := range t.MediaGaps {
				start := g.EndedAt - g.Duration
				for _, m := range r.Moves {
					if pairCovered(start, g.EndedAt, m.Start, m.End) {
						continue gap // measured by that move's gap metric
					}
				}
				for _, w := range relayWindows {
					if pairCovered(start, g.EndedAt, w[0], w[1]) {
						j.RelayGapMax = max(j.RelayGapMax, g.Duration)
						if g.Duration >= pairRelayGapLimit {
							j.fail("%s: %s relay handover gap %s reaches %s", call, t.Kind, g.Duration, pairRelayGapLimit)
						}
						continue gap
					}
				}
				if g.Duration > callharness.MediaGapCeiling {
					j.fail("%s: %s gap %s at %s outside every scheduled rollout event", call, t.Kind, g.Duration, g.EndedAt)
				}
			}
		}
		for _, kind := range []string{"audio", "video"} {
			if !kinds[kind] {
				j.fail("%s: no echoed %s track", call, kind)
			}
		}
	}
	for p, phase := range j.Phases {
		if phase.Moves == 0 {
			j.unknown("%s: no planned-move evidence", phase.Phase)
		} else if !callharness.EnoughConclusive(phase.Conclusive, phase.Moves) {
			j.unknown("%s: move measurement coverage %d/%d below %d%%", phase.Phase, phase.Conclusive, phase.Moves, callharness.MinimumConclusivePercent)
		}
		for _, kind := range []string{"audio", "video"} {
			pl := at(p, kind)
			s := pairGapSummary{Phase: phase.Phase, Kind: kind, Samples: pl.samples, Trusted: pl.trusted, P95Gap: pairP95(pl.gaps), P95Excess: pairP95(pl.excess), MaxGap: pl.maxGap}
			j.MoveGaps = append(j.MoveGaps, s)
			switch {
			case s.Samples == 0:
				j.unknown("%s: no planned-move %s evidence", phase.Phase, kind)
			case !callharness.EnoughConclusive(s.Trusted, s.Samples):
				j.unknown("%s: %s planned-move coverage %d/%d below %d%% (baseline under %d gaps)", phase.Phase, kind, s.Trusted, s.Samples, callharness.MinimumConclusivePercent, pairBaselineSamples)
			}
			if s.Trusted > 0 && (s.P95Gap > pairMoveGapCeiling || s.P95Excess > pairMoveExcessLimit) {
				j.fail("%s: %s planned-move p95 gap %s (excess %s) exceeds %s or baseline+%s", phase.Phase, kind, s.P95Gap, s.P95Excess, pairMoveGapCeiling, pairMoveExcessLimit)
			}
		}
	}
	switch {
	case len(j.Failures) > 0:
		j.Outcome = "fail"
	case len(j.Unknowns) > 0:
		j.Outcome = "inconclusive"
	default:
		j.Outcome = "pass"
	}
	return j
}

func (j pairJudgement) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "RELEASE_PAIR_VERDICT outcome=%s moves=%d relay_handovers=%d relay_gap_max_ms=%.1f caller_decryption_failures=%d\n", j.Outcome, j.Moves, j.RelayHandovers, pairMillis(j.RelayGapMax), j.CallerDecryptionFailures)
	for _, p := range j.Phases {
		fmt.Fprintf(&b, "RELEASE_PAIR_PHASE_EVIDENCE phase=%s moves=%d conclusive=%d relay_lost_audio_units=%d relay_lost_video_frames=%d\n", p.Phase, p.Moves, p.Conclusive, p.RelayLostAudio, p.RelayLostVideoFrames)
	}
	for _, g := range j.MoveGaps {
		fmt.Fprintf(&b, "RELEASE_PAIR_MOVE_GAPS phase=%s kind=%s samples=%d trusted=%d p95_gap_ms=%.1f p95_excess_ms=%.1f max_gap_ms=%.1f\n", g.Phase, g.Kind, g.Samples, g.Trusted, pairMillis(g.P95Gap), pairMillis(g.P95Excess), pairMillis(g.MaxGap))
	}
	for _, f := range j.Failures {
		fmt.Fprintf(&b, "RELEASE_PAIR_FAIL %s\n", f)
	}
	for _, u := range j.Unknowns {
		fmt.Fprintf(&b, "RELEASE_PAIR_INCONCLUSIVE %s\n", u)
	}
	return strings.TrimRight(b.String(), "\n")
}

func pairMillis(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
