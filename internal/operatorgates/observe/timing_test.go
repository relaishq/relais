package observe

import "testing"

func TestTimingCauseProductReasons(t *testing.T) {
	// Formats emitted by distributed.go, distributed_classification.go and
	// yardstick.go. Unknown reasons retain the existing coverage fallback and stay opaque.
	cases := []struct{ reason, cause string }{
		{"first_content classification changes within event-time interval; width=1ms", "classification-varies-with-clock-interval"},
		{"first_content not proven throughout event-time interval; width=1ms", "classification-unproven-within-clock-interval"},
		{"interval evaluation unavailable or exceeds 4096 evaluations; width=1ms", "interval-evaluation-unavailable"},
		{"duration=[1.997s,2.003s]; event interval width=6ms", "duration-bounds-uncertain"},
		{"event window evidence incomplete", "event-window-incomplete"},
		{"audio gap baseline has fewer than 10 guaranteed intervals", "baseline-intervals-insufficient"},
		{"audio pause may reach 2s within clock interval", "pause-bound-uncertain"},
		{"audio baseline spread not proven within 50ms", "baseline-spread-uncertain"},
		{"audio move p95 policy not proven across clock interval", "move-p95-unproven"},
		{"too few distributed trials: moves 19/20, takeovers 9/10", "trial-count-insufficient"},
		{"audio: content loss not proven across clock interval", "content-loss-unproven"},
		{"interval evaluation unavailable or exceeds 4096 evaluations; classification changes within event-time interval; width=1ms", "classification-varies-with-clock-interval"},
	}
	for _, reason := range []string{
		"server reported call loss: ownership_expired",
		"server operation failed or outcome uncertain",
		"mapped event violates arm/receipt ordering or host clock bound",
		"mapped event history or settle window unavailable",
		"no caller media observations",
		"audio: [insufficient caller window coverage]",
		"audio: [gap ceiling fails for every mapped window]",
		"audio: [gap judgment varies within clock interval]",
		"audio: caller content loss across every event window",
		"first identified post-recovery live content unavailable",
		"server loss or caller content failure",
		"audio pause reaches 2s for every mapped window",
		"audio move p95 exceeds policy across clock interval",
		"content settle window unavailable across interval",
		"no identified sends across event interval",
		"sent content never returned in every mapped window",
		"missing content lies in clock uncertainty margin",
		"conflicting video content identities",
		"event identity unavailable",
		"event clock mapping unavailable",
		"clock trust invalidated after event summary",
		"move timestamps inconsistent with shared control-clock rate bound",
		"first event baseline interval unavailable",
		"content settle window truncated by hangup",
		"no identified sent audio",
		"no identified sent video",
		"sent audio never returned",
		"sent video never returned complete",
		"returned content identity unavailable in freshness window",
		"media resume not observed before limit",
		"first decoded content identity unavailable",
		"decoded content identity unavailable before first new content",
		"decoded content identity unavailable before first live content",
		"no decoded frame after resume before limit",
		"no decoded new content after resume before limit",
		"no decoded frame sent after media resumed before limit",
		"video baseline cannot distinguish live from outage media",
		"event history expired before observation",
		"online decoder reported invalid bitstream",
		"too few online decoded samples",
		"online decode samples unavailable or dropped",
		"stability window truncated: observed 1s, need 2s",
		"baseline has 9 samples (need 10)",
		"baseline p95-minus-median exceeds 20ms",
		"AbsoluteLag: first baseline median 1s exceeds 100ms",
		"freshness not recovered before window end (need 3 unique units and live lateness within budget)",
		"freshness recovery exceeds 2s",
		"mixed distributed run identities",
		"armed distributed receipt missing",
		"armed event receipt missing: event-1",
		"invalid clock evidence",
		"invalid clock interval",
		"clock interval composition overflow",
		"invalid clock interval or drift",
		"clock drift overflow",
		"clock interval widening overflow",
		"clock jump or invalid probe ordering",
		"empty clock probe interval",
		"clock probe: 500 Internal Server Error",
		"clock probe timestamps unavailable",
		"invalid clock evidence: clock jump",
		"clock jump or invalid ordering between probes",
		"clock jump or observed rate [-1,1] ppm exceeds stated drift bound 0 ppm",
		"clock probe capacity exceeded",
		"clock mapping unavailable",
		"clock anchor distance overflow",
		"clock probes disagree after drift widening",
		"private-secret",
		"unknown metric: duration=[1ms,2ms]",
		"first content:duration=[1ms,2ms]",
		"",
	} {
		cases = append(cases, struct{ reason, cause string }{reason, "trusted-coverage-below-80-percent"})
	}
	// Exactly the prefixes emitted by EventMeasurement.updateJudgment.
	for _, prefix := range []string{"", "audio loss: ", "video loss: ", "audio freshness: ", "video freshness: ", "resume: ", "first content: ", "first new content: ", "first live: "} {
		for _, tc := range cases {
			t.Run(prefix+tc.reason, func(t *testing.T) {
				reason := prefix + tc.reason
				if got := TimingCause(reason); got != tc.cause {
					t.Fatalf("TimingCause(%q) = %q, want %q", reason, got, tc.cause)
				}
				canonical, ok := CanonicalTimingReason(reason)
				if ok != (tc.cause != "trusted-coverage-below-80-percent") {
					t.Fatalf("CanonicalTimingReason(%q) = %q, %t", reason, canonical, ok)
				}
				if ok {
					again, recognized := CanonicalTimingReason(canonical)
					if !recognized || again != canonical || TimingCause(canonical) != tc.cause {
						t.Fatalf("canonical reason changed classification: %q", canonical)
					}
				}
			})
		}
	}
}

func TestClockCoverageTimingReasonsStayPublic(t *testing.T) {
	for _, cause := range []string{"clock-coverage-missing", "clock-coverage-bound", "clock-coverage-break", "clock-coverage-kill-bracket", "clock-coverage-falsified"} {
		for _, prefix := range []string{"", "first content: "} {
			got, ok := CanonicalTimingReason(prefix + cause)
			if !ok || got != cause || TimingCause(prefix+cause) != cause {
				t.Fatal("coverage cause lost", prefix, cause, got)
			}
		}
	}
	if IsCoverageCause("clock-coverage-private-text") {
		t.Fatal("arbitrary cause accepted")
	}
}
