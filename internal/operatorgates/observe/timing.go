package observe

import "strings"

// TimingCause projects a diagnostic reason to its fixed public code. It never
// participates in the product judge's verdict or threshold comparisons.
func TimingCause(reason string) string {
	switch {
	case strings.Contains(reason, "classification changes within event-time interval;"):
		return "classification-varies-with-clock-interval"
	case strings.Contains(reason, "not proven throughout event-time interval;"):
		return "classification-unproven-within-clock-interval"
	case strings.HasPrefix(reason, "interval evaluation unavailable or exceeds "):
		return "interval-evaluation-unavailable"
	case strings.HasPrefix(reason, "duration=["):
		return "duration-bounds-uncertain"
	case reason == "event window evidence incomplete":
		return "event-window-incomplete"
	case strings.HasSuffix(reason, " gap baseline has fewer than 10 guaranteed intervals"):
		return "baseline-intervals-insufficient"
	case strings.HasSuffix(reason, " pause may reach 2s within clock interval"):
		return "pause-bound-uncertain"
	case strings.HasSuffix(reason, " baseline spread not proven within 50ms"):
		return "baseline-spread-uncertain"
	case strings.HasSuffix(reason, " move p95 policy not proven across clock interval"):
		return "move-p95-unproven"
	case strings.HasPrefix(reason, "too few distributed trials:"):
		return "trial-count-insufficient"
	case strings.Contains(reason, "content loss not proven across clock interval"):
		return "content-loss-unproven"
	}
	// The product emits an inconclusive row when trusted coverage is below 80%.
	return "trusted-coverage-below-80-percent"
}

// CanonicalTimingReason removes all dynamic text while retaining exactly the
// same classifier result and precedence. Unknown reasons must remain opaque.
func CanonicalTimingReason(reason string) (string, bool) {
	switch TimingCause(reason) {
	case "classification-varies-with-clock-interval":
		return "classification changes within event-time interval;", true
	case "classification-unproven-within-clock-interval":
		return "not proven throughout event-time interval;", true
	case "interval-evaluation-unavailable":
		return "interval evaluation unavailable or exceeds ", true
	case "duration-bounds-uncertain":
		return "duration=[", true
	case "event-window-incomplete":
		return "event window evidence incomplete", true
	case "baseline-intervals-insufficient":
		return " gap baseline has fewer than 10 guaranteed intervals", true
	case "pause-bound-uncertain":
		return " pause may reach 2s within clock interval", true
	case "baseline-spread-uncertain":
		return " baseline spread not proven within 50ms", true
	case "move-p95-unproven":
		return " move p95 policy not proven across clock interval", true
	case "trial-count-insufficient":
		return "too few distributed trials:", true
	case "content-loss-unproven":
		return "content loss not proven across clock interval", true
	}
	return "", false
}
