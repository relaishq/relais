package main

import (
	"fmt"
	"math"

	"github.com/relais/pkg/callharness"
)

const (
	contentLossConfidence      = 0.95
	contentLossTailAlpha       = (1 - contentLossConfidence) / 4
	minimumLossBaselineUnits   = 100
	minimumLossBaselineWindows = 10
)

// The decision pools event and matched baseline windows across the run. The
// upper rate accounts for baseline uncertainty; the predictive bound accounts
// for event sampling. Four one-sided tails (two per track) share a 5% budget.
// This is a bound under independent, stationary Bernoulli content losses, not
// exact per-packet attribution or proof of zero additional loss.
type profileLossComparison struct {
	Scope, Method, Verdict          string
	Event, Baseline                 callharness.ContentLossCounts
	BoundAvailable, GatesAcceptance bool
	BaselineWindows                 int
	Confidence, TailAlpha           float64
	BaselineRate, BaselineUpperRate float64
	ExpectedCount                   float64
	UpperBound                      int
	Reasons                         []string
}

func judgeContentLoss(event, baseline callharness.ContentLossCounts, windows int, lossy bool) profileLossComparison {
	out := profileLossComparison{Scope: "pooled run", Method: "exact zero loss", Event: event, Baseline: baseline, BaselineWindows: windows, Verdict: "pass"}
	if lossy {
		out.Method = "Clopper-Pearson upper rate + binomial predictive upper bound"
		out.Confidence, out.TailAlpha = contentLossConfidence, contentLossTailAlpha
	}
	if baseline.Sent > 0 && baseline.Lost >= 0 && baseline.Lost <= baseline.Sent {
		out.BaselineRate = float64(baseline.Lost) / float64(baseline.Sent)
		out.ExpectedCount = float64(max(0, event.Sent)) * out.BaselineRate
	}
	if !event.Trusted || event.Sent <= 0 || event.Lost < 0 || event.Lost > event.Sent {
		out.Verdict, out.Reasons = "inconclusive", []string{"event content counts unavailable or untrusted"}
		return out
	}
	if !lossy {
		out.BoundAvailable = true
		if event.Lost > 0 {
			out.Verdict, out.Reasons = "fail", []string{"nonzero caller content loss"}
		}
		return out
	}
	if !baseline.Trusted || baseline.Sent < minimumLossBaselineUnits || baseline.Sent < event.Sent || windows < minimumLossBaselineWindows || baseline.Lost < 0 || baseline.Lost > baseline.Sent {
		out.Verdict = "inconclusive"
		out.Reasons = []string{fmt.Sprintf("loss baseline too small or untrusted: sent=%d lost=%d windows=%d; need >=%d units, >=event sent=%d, and >=%d windows", baseline.Sent, baseline.Lost, windows, minimumLossBaselineUnits, event.Sent, minimumLossBaselineWindows)}
		return out
	}
	out.BoundAvailable = true
	out.BaselineUpperRate = binomialUpperRate(baseline.Lost, baseline.Sent, out.TailAlpha)
	out.UpperBound = binomialUpperCount(event.Sent, out.BaselineUpperRate, out.TailAlpha)
	if event.Lost > out.UpperBound {
		out.Verdict = "fail"
		out.Reasons = []string{fmt.Sprintf("event content loss %d exceeds network baseline upper bound %d (expected %.3f)", event.Lost, out.UpperBound, out.ExpectedCount)}
	}
	return out
}

// binomialCDF sums probabilities in log space to avoid underflow for zero
// losses and large denominators. Edge probabilities have exact answers.
func binomialCDF(k, n int, p float64) float64 {
	if k < 0 {
		return 0
	}
	if k >= n || p <= 0 {
		return 1
	}
	if p >= 1 {
		return 0
	}
	term := float64(n) * math.Log1p(-p)
	sum := term
	odds := math.Log(p) - math.Log1p(-p)
	for i := 0; i < k; i++ {
		term += math.Log(float64(n-i)/float64(i+1)) + odds
		hi, lo := max(sum, term), min(sum, term)
		sum = hi + math.Log1p(math.Exp(lo-hi))
	}
	return min(1, math.Exp(sum))
}

// Invert P_p[X <= lost] = alpha for a one-sided Clopper-Pearson upper
// confidence limit. A zero-loss baseline still has a positive upper rate.
func binomialUpperRate(lost, sent int, alpha float64) float64 {
	if lost == sent {
		return 1
	}
	lo, hi := 0.0, 1.0
	for i := 0; i < 64; i++ {
		mid := (lo + hi) / 2
		if binomialCDF(lost, sent, mid) > alpha {
			lo = mid
		} else {
			hi = mid
		}
	}
	return hi
}

func binomialUpperCount(sent int, rate, alpha float64) int {
	lo, hi := 0, sent
	for lo < hi {
		mid := lo + (hi-lo)/2
		if binomialCDF(mid, sent, rate) >= 1-alpha {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	return lo
}

func poolContentLoss(rows []profileRow, video bool) (callharness.ContentLossCounts, callharness.ContentLossCounts, int) {
	event, baseline := callharness.ContentLossCounts{Trusted: true}, callharness.ContentLossCounts{Trusted: true}
	windows := 0
	for _, row := range rows {
		e := row.Measurement.ContentLoss
		counts := e.Event.Audio
		verdict := row.Measurement.AudioLoss
		if video {
			counts, verdict = e.Event.Video, row.Measurement.VideoLoss
		}
		event.Sent += counts.Sent
		event.Lost += counts.Lost
		event.Trusted = event.Trusted && counts.Trusted && verdict.Trusted && counts.Sent > 0
		baseline.Trusted = baseline.Trusted && len(e.Baselines) > 0
		for _, window := range e.Baselines {
			counts = window.Audio
			if video {
				counts = window.Video
			}
			baseline.Sent += counts.Sent
			baseline.Lost += counts.Lost
			baseline.Trusted = baseline.Trusted && counts.Trusted && counts.Sent > 0
			windows++
		}
	}
	return event, baseline, windows
}
