package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/relais/internal/clusterprocess"
	"github.com/relais/internal/nettopology"
	"github.com/relais/pkg/callharness"
	"github.com/relais/pkg/mediaworker"
)

type profileTrialOptions struct{ Name, Scenario string }
type gapBaseline struct {
	Intervals   []time.Duration `json:"intervals,omitempty"`
	Samples     int
	Median, P95 time.Duration
}
type profileRow struct {
	// Raw per-row counts are in Measurement.ContentLoss. These comparisons
	// name the pooled run decision, never a separate per-row statistical test.
	AudioContentLoss, VideoContentLoss profileLossComparison
	ProbeEvents                        []callharness.MoveReport   `json:"probe_events,omitempty"`
	EventResult                        mediaworker.HandoverResult `json:"event_result"`
	Verdict                            string
	Reasons                            []string
	Trial                              int
	AudioGap, VideoGap                 time.Duration
	AudioBaseline, VideoBaseline       gapBaseline
	Measurement                        callharness.EventMeasurement
	Integrity                          callharness.MetricVerdict
	VideoDecode                        callharness.FullDecode
	BarrierResends                     uint64
	UncertainOutcomes                  int
}
type profileReport struct {
	AudioContentLoss, VideoContentLoss                               profileLossComparison
	Profile, Scenario, Verdict                                       string
	FaultPolicy                                                      string
	DurationUnit                                                     string
	Load                                                             int
	Rows                                                             []profileRow
	Links                                                            []nettopology.LinkSetting
	AudioP95, VideoP95, AudioBaselineP95, VideoBaselineP95           time.Duration
	GapAllowance, AbsoluteGapCeiling, FreshnessCeiling, PauseCeiling time.Duration
	MinimumTrials                                                    int
	BarrierResends                                                   uint64
	UncertainOutcomes                                                int
	Reasons                                                          []string
}

func validateProfileScenario(topology, scenario string, incompatible bool) error {
	if topology != "netns" || (scenario != "move" && scenario != "takeover") || incompatible {
		return errors.New("profile scenarios require netns, move or takeover, and no buffer/cache/drain/relay comparison flags")
	}
	return nil
}
func profileMove(ctx context.Context, call *callharness.Call, owner string) error {
	to := 1
	if owner == "1" {
		to = 0
	} else if owner != "0" {
		return fmt.Errorf("unknown owner %q", owner)
	}
	return call.MoveExternal(ctx, to)
}
func profileBarrierMove(ctx context.Context, t *nettopology.Topology, call *callharness.Call, owner string) error {
	remove, err := t.BarrierLoss(ctx)
	if err != nil {
		return err
	}
	done := make(chan error, 1)
	timer := time.AfterFunc(40*time.Millisecond, func() { done <- remove() })
	moveErr := profileMove(ctx, call, owner)
	if timer.Stop() {
		done <- remove()
	}
	return errors.Join(moveErr, <-done)
}
func baselineGaps(arrivals []time.Duration, start time.Duration) gapBaseline {
	var gaps []time.Duration
	for i := 1; i < len(arrivals); i++ {
		if arrivals[i-1] >= start-3*time.Second && arrivals[i] < start {
			gaps = append(gaps, arrivals[i]-arrivals[i-1])
		}
	}
	return gapBaseline{Intervals: gaps, Samples: len(gaps), Median: quantile(gaps, .5), P95: quantile(gaps, .95)}
}
func quantile(values []time.Duration, q float64) time.Duration {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted[max(0, int(math.Ceil(q*float64(len(sorted))))-1)]
}
func measureProfile(report *callharness.Report, relay string, options profileTrialOptions, after time.Duration) profileRow {
	row := profileRow{Integrity: callharness.MetricVerdict{Trusted: true}}
	fail := func(reason string) {
		row.Integrity.Failed = true
		row.Integrity.Reasons = append(row.Integrity.Reasons, reason)
	}
	if report.DecryptionFailures.Total() != 0 {
		fail("decryption failures")
	}
	if report.ICERestarts != 0 || report.Renegotiations != 0 || report.OfferAnswerExchanges != 1 || !report.ConnectedThroughout() || report.RemoteAddr != relay {
		fail("connection continuity failed")
	}
	if report.Consent.ResponsesAfter == 0 {
		fail("no post-event consent")
	}
	if after < 60*time.Second {
		row.Integrity.Trusted = false
		row.Integrity.Reasons = append(row.Integrity.Reasons, "60s consent observation NOT-RUN")
	} else if report.Consent.ObservedFor < 60*time.Second {
		fail("consent observation below 60s")
	}
	if len(report.Moves) == 0 {
		fail("no caller event record")
		return row
	}
	row.ProbeEvents = append([]callharness.MoveReport(nil), report.Moves[:len(report.Moves)-1]...)
	move := report.Moves[len(report.Moves)-1]
	if move.Kind != options.Scenario || move.Error != "" {
		fail("event kind/error: " + move.Kind + " " + move.Error)
	}
	row.Measurement = move.Measurement
	row.EventResult = move.Result
	for _, track := range move.Tracks {
		switch track.Kind {
		case "audio":
			row.AudioGap = track.Gap
		case "video":
			row.VideoGap = track.Gap
		}
		if track.PacketsAfter == 0 {
			fail(track.Kind + " did not resume")
		}
	}
	for _, track := range report.Tracks {
		b := baselineGaps(track.Arrivals, move.Start)
		switch track.Kind {
		case "audio":
			row.AudioBaseline = b
		case "video":
			row.VideoBaseline = b
		}
		if track.UnmatchedPayloads > 0 {
			fail("unmatched media payload")
		}
		if track.Video != nil && (track.Video.UnmatchedFrames > 0 || track.Video.KeyframeDecodeErrors > 0) {
			fail("invalid video content")
		}
		if track.Video != nil {
			row.VideoDecode = track.Video.FullDecode
		}
		if track.Video != nil && !track.Video.FullDecode.Ran() {
			row.Integrity.Trusted = false
			row.Integrity.Reasons = append(row.Integrity.Reasons, "full video decode unavailable: "+track.Video.FullDecode.Skipped)
		}
	}
	if row.AudioGap <= 0 || row.VideoGap <= 0 {
		fail("missing per-track event gap")
	}
	return row
}

// The public caller/relay and private relay/worker links carry media. Control
// and store links do not. Read loss from the applied plan, not the profile name.
func mediaPathHasLoss(links []nettopology.LinkSetting) bool {
	for _, link := range links {
		if link.Loss > 0 && (link.Role == "caller" || link.Role == "relay" || strings.HasPrefix(link.Role, "worker-")) {
			return true
		}
	}
	return false
}

// Judge metrics independently: a confirmed integrity/loss/ceiling failure
// remains failed even when another metric lacks samples. Unknown never passes.
func judgeProfile(options profileTrialOptions, rows []profileRow, links []nettopology.LinkSetting) profileReport {
	p, _ := nettopology.ParseProfile(options.Name)
	out := profileReport{Profile: options.Name, Scenario: options.Scenario, Load: 1, Rows: rows, Links: links, GapAllowance: 50 * time.Millisecond, AbsoluteGapCeiling: 150 * time.Millisecond, FreshnessCeiling: p.FreshnessCeiling, PauseCeiling: 2 * time.Second, MinimumTrials: 10, Verdict: "pass", DurationUnit: "nanoseconds"}
	if options.Scenario == "move" {
		out.MinimumTrials = 20
	}
	if options.Name == "lossy-dc" {
		out.FaultPolicy = "one discarded committed resume reply; 40ms private drain-marker loss; 6s probe recovery windows"
	}
	failed, unknown := false, false
	reason := func(bad bool, s string) {
		if bad {
			failed = true
		} else {
			unknown = true
		}
		out.Reasons = append(out.Reasons, s)
	}
	if len(rows) < out.MinimumTrials {
		reason(false, fmt.Sprintf("too few trials: %d, need %d", len(rows), out.MinimumTrials))
	}
	mediaLoss := mediaPathHasLoss(links)
	for _, track := range []struct {
		video bool
		out   *profileLossComparison
	}{{false, &out.AudioContentLoss}, {true, &out.VideoContentLoss}} {
		event, baseline, windows := poolContentLoss(rows, track.video)
		*track.out = judgeContentLoss(event, baseline, windows, mediaLoss)
		track.out.GatesAcceptance = options.Scenario == "takeover"
	}
	var audio, video, ab, vb []time.Duration
	for i, row := range rows {
		reasonStart := len(out.Reasons)
		rowFailed, rowUnknown := false, false
		rowReason := func(bad bool, s string) {
			if bad {
				rowFailed = true
			} else {
				rowUnknown = true
			}
			reason(bad, s)
		}
		prefix := fmt.Sprintf("trial %d: ", row.Trial)
		if row.Integrity.Failed {
			rowReason(true, prefix+strings.Join(row.Integrity.Reasons, "; "))
		} else if !row.Integrity.Trusted {
			rowReason(false, prefix+strings.Join(row.Integrity.Reasons, "; "))
		}
		for _, b := range []struct {
			kind  string
			value gapBaseline
		}{{"audio", row.AudioBaseline}, {"video", row.VideoBaseline}} {
			if b.value.Samples < 10 {
				rowReason(false, prefix+b.kind+" gap baseline has fewer than 10 intervals")
			}
			// Packet-gap spread includes the normal intra-frame burst/33ms cadence.
			// 50ms is the noise budget for gaps; #32's independent content-age
			// baseline still uses its original 20ms spread limit below.
			if b.value.P95-b.value.Median > 50*time.Millisecond {
				rowReason(false, prefix+b.kind+" gap baseline p95-minus-median exceeds 50ms")
			}
		}
		audio, video = append(audio, row.AudioGap), append(video, row.VideoGap)
		ab, vb = append(ab, row.AudioBaseline.Intervals...), append(vb, row.VideoBaseline.Intervals...)
		m := row.Measurement
		for _, f := range []struct {
			kind   string
			report callharness.FreshnessReport
		}{{"audio", m.Audio}, {"video", m.Video}} {
			if f.report.BaselineSamples < 10 || f.report.BaselineP95-f.report.BaselineMedian > 20*time.Millisecond {
				rowReason(false, prefix+f.kind+" content-age baseline too small or noisy")
			}
		}
		if options.Scenario == "takeover" {
			if row.AudioGap >= out.PauseCeiling || row.VideoGap >= out.PauseCeiling {
				rowReason(true, prefix+"pause reaches 2s ceiling")
			}
			for _, loss := range []struct {
				kind       string
				verdict    callharness.MetricVerdict
				comparison profileLossComparison
			}{{"audio", m.AudioLoss, out.AudioContentLoss}, {"video", m.VideoLoss, out.VideoContentLoss}} {
				if !loss.verdict.Trusted {
					rowReason(false, prefix+loss.kind+" loss inconclusive: "+loss.verdict.Summary())
				}
				if !mediaLoss && loss.verdict.Failed {
					rowReason(true, prefix+loss.kind+" content lost: "+loss.verdict.Summary())
				}
				if mediaLoss && loss.verdict.Failed {
					// Only the ordinary missing-content verdict is reattributed.
					// Identity corruption and other known failures must still fail.
					ordinary := "sent audio never returned"
					if loss.kind == "video" {
						ordinary = "sent video never returned complete"
					}
					for _, why := range loss.verdict.Reasons {
						if why != ordinary {
							rowReason(true, prefix+loss.kind+" content failure: "+why)
						}
					}
				}
				if loss.comparison.Verdict != "pass" {
					rowReason(loss.comparison.Verdict == "fail", prefix+loss.kind+" pooled content loss: "+strings.Join(loss.comparison.Reasons, "; "))
				}
			}
			if !mediaLoss && (m.AudioLoss.Trusted && m.LostAudio > 0 || m.VideoLoss.Trusted && m.LostVideoFrames > 0) {
				rowReason(true, prefix+"nonzero caller content loss")
			}
		}
		out.BarrierResends += row.BarrierResends
		out.UncertainOutcomes += row.UncertainOutcomes
		row.Verdict = "conclusive"
		if rowFailed {
			row.Verdict = "fail"
		} else if rowUnknown {
			row.Verdict = "inconclusive"
		}
		row.AudioContentLoss, row.VideoContentLoss = out.AudioContentLoss, out.VideoContentLoss
		row.Reasons = append([]string(nil), out.Reasons[reasonStart:]...)
		out.Rows[i] = row
	}
	out.AudioP95, out.VideoP95 = quantile(audio, .95), quantile(video, .95)
	out.AudioBaselineP95, out.VideoBaselineP95 = quantile(ab, .95), quantile(vb, .95)
	if options.Scenario == "move" {
		for _, track := range []struct {
			kind               string
			observed, baseline time.Duration
		}{{"audio", out.AudioP95, out.AudioBaselineP95}, {"video", out.VideoP95, out.VideoBaselineP95}} {
			if track.observed > out.AbsoluteGapCeiling {
				reason(true, track.kind+" move p95 exceeds absolute 150ms ceiling")
			}
			if track.observed > track.baseline+out.GapAllowance {
				reason(true, track.kind+" move p95 exceeds same-run baseline p95 + 50ms")
			}
		}
	}
	if options.Name == "lossy-dc" {
		if out.BarrierResends == 0 {
			reason(false, "barrier resend NOT-PROVEN")
		}
		if out.UncertainOutcomes == 0 {
			reason(false, "uncertain outcome NOT-PROVEN")
		}
	}
	if failed {
		out.Verdict = "fail"
	} else if unknown {
		out.Verdict = "inconclusive"
	}
	return out
}

func profileResultLine(report profileReport) string {
	line := fmt.Sprintf("PROFILE_RESULT verdict=%s audio_p95=%s audio_baseline_p95=%s video_p95=%s video_baseline_p95=%s barrier_resends=%d uncertain_outcomes=%d", report.Verdict, report.AudioP95, report.AudioBaselineP95, report.VideoP95, report.VideoBaselineP95, report.BarrierResends, report.UncertainOutcomes)
	if len(report.Rows) == 0 {
		return line + " slowest_trial=unavailable slowest_phases=unavailable"
	}
	// Keep the largest media gap visible even when p95 excludes that trial.
	slowest := report.Rows[0]
	for _, row := range report.Rows[1:] {
		if max(row.AudioGap, row.VideoGap) > max(slowest.AudioGap, slowest.VideoGap) {
			slowest = row
		}
	}
	line += fmt.Sprintf(" slowest_trial=%d slowest_audio_gap=%s slowest_video_gap=%s", slowest.Trial, slowest.AudioGap, slowest.VideoGap)
	r := slowest.EventResult
	if r.Duration <= 0 {
		return line + " slowest_phases=unavailable"
	}
	// Transfer, route change, release and coordination overhead are not timed
	// separately by the controller. Report their combined residual honestly.
	other := r.Duration - r.Drain - r.Export - r.Resume
	return line + fmt.Sprintf(" slowest_duration=%s slowest_barrier=%s slowest_export=%s slowest_resume=%s slowest_other=%s", r.Duration, r.Drain, r.Export, r.Resume, other)
}

func writeProfileReport(dir string, report profileReport) error {
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "profile-report.json"), encoded, 0600); err != nil {
		return err
	}
	fmt.Printf("PROFILE_POLICY profile=%s scenario=%s load=%d minimum_trials=%d move_allowance=%s absolute_move_ceiling=%s takeover_pause_ceiling=%s freshness_median_ceiling=%s\n", report.Profile, report.Scenario, report.Load, report.MinimumTrials, report.GapAllowance, report.AbsoluteGapCeiling, report.PauseCeiling, report.FreshnessCeiling)
	fmt.Println(profileResultLine(report))
	fmt.Printf("PROFILE_REPORT duration_unit=nanoseconds %s\n", encoded)
	return nil
}
func runProfileTrials(ctx context.Context, manager *clusterprocess.Manager, t *nettopology.Topology, bin, dir, key, addr, password string, options profileTrialOptions, links []nettopology.LinkSetting, runs int, after time.Duration, verbose bool) error {
	var rows []profileRow
	for i := 0; i < runs; i++ {
		trialDir := filepath.Join(dir, fmt.Sprintf("%s-%s-%02d", options.Name, options.Scenario, i+1))
		if err := os.Mkdir(trialDir, 0700); err != nil {
			return err
		}
		env := processEnv(key, addr, fmt.Sprintf("relais:profile:%s:%d:", filepath.Base(dir), i))
		env = append(env, "RELAIS_PRIVATE_NETS="+t.Plan.PrivateNets(), "RELAIS_REDIS_PASSWORD="+password)
		trialCtx, cancel := context.WithTimeout(ctx, after+50*time.Second)
		r, err := trial(trialCtx, manager, bin, trialDir, env, after, 5*time.Second, verbose, false, false, t, &options)
		cancel()
		if err != nil {
			rows = append(rows, profileRow{Trial: i + 1, Integrity: callharness.MetricVerdict{Reasons: []string{"trial could not supply caller measurement: " + err.Error()}}})
			if writeErr := writeProfileReport(dir, judgeProfile(options, rows, links)); writeErr != nil {
				return errors.Join(err, writeErr)
			}
			return trialFailure(i+1, options.Scenario, trialDir, err)
		}
		r.ProfileRow.Trial = i + 1
		rows = append(rows, *r.ProfileRow)
		fmt.Printf("PROFILE_TRIAL %d audio_gap=%s video_gap=%s barrier_resends=%d uncertain_outcomes=%d integrity=%s\n", i+1, r.ProfileRow.AudioGap, r.ProfileRow.VideoGap, r.ProfileRow.BarrierResends, r.ProfileRow.UncertainOutcomes, r.ProfileRow.Integrity.Summary())
	}
	report := judgeProfile(options, rows, links)
	if err := writeProfileReport(dir, report); err != nil {
		return err
	}
	if report.Verdict != "pass" {
		return fmt.Errorf("profile=%s scenario=%s %s: %s", options.Name, options.Scenario, report.Verdict, strings.Join(report.Reasons, "; "))
	}
	return nil
}
