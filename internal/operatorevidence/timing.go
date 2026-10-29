package operatorevidence

// UndecidedMetric is a diagnostic projection of a strict product yardstick row.
// Phase and Event are zero-based indexes, never operation or session identities.
type UndecidedMetric struct {
	Phase  int      `json:"phase"`
	Event  int      `json:"event"`
	Metric string   `json:"metric"`
	Causes []string `json:"causes"`
}

func TimingMetric(metric string) bool {
	return metric == "first_content" || metric == "first_new_content" || metric == "distributed_window"
}

func PublicMetric(metric string) string {
	switch metric {
	case "audio_loss", "audio_freshness", "resume", "video_loss", "video_freshness", "first_content", "first_new_content", "first_live", "distributed_window":
		return metric
	}
	return "unclassified"
}

func PublicTimingCause(cause string) string {
	switch cause {
	case "trusted-coverage-below-80-percent", "classification-varies-with-clock-interval", "classification-unproven-within-clock-interval", "interval-evaluation-unavailable", "duration-bounds-uncertain", "event-window-incomplete", "baseline-intervals-insufficient", "pause-bound-uncertain", "baseline-spread-uncertain", "move-p95-unproven", "trial-count-insufficient", "content-loss-unproven", "metric-undecided":
		return cause
	}
	return "unclassified"
}
