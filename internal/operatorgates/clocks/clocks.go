// Package clocks checks independent-host evidence against approved bounds.
package clocks

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"time"

	"github.com/relaishq/relais/internal/operatorgates/observe"
)

type Sample struct {
	Tracking       string  `json:"tracking,omitempty"`
	ErrorBoundNS   int64   `json:"errorBoundNs"`
	RateErrorPPM   float64 `json:"rateErrorPpm"`
	MeasuredUnixNS string  `json:"measuredUnixNs"`
}
type Bounds struct {
	FrontendControlBoundNS int64   `json:"frontendControlBoundNs"`
	FrontendWorkerBoundNS  int64   `json:"frontendWorkerBoundNs"`
	DriftPPM               float64 `json:"driftPpm"`
}
type Evidence struct {
	At              time.Time
	Clocks          map[string]Sample
	Control, Worker int64
	Drift           float64
}

const maxSafeInteger = 1<<53 - 1

var nsPattern = regexp.MustCompile(`^[0-9]{19}$`)

func Parse(data []byte) (s Sample, err error) {
	err = observe.Decode(data, &s, observe.Contract{"errorBoundNs", "rateErrorPpm", "measuredUnixNs"})
	if err == nil && (s.ErrorBoundNS < 0 || s.ErrorBoundNS > maxSafeInteger || !finite(s.RateErrorPPM) || s.RateErrorPPM < 0 || !nsPattern.MatchString(s.MeasuredUnixNS)) {
		err = fmt.Errorf("missing independent clock evidence")
	}
	return
}
func finite(x float64) bool { return !math.IsNaN(x) && !math.IsInf(x, 0) }

func Check(rows map[string]Sample, bounds Bounds) (Evidence, error) {
	e := Evidence{Clocks: rows}
	if bounds.FrontendControlBoundNS < 0 || bounds.FrontendWorkerBoundNS < 0 || !finite(bounds.DriftPPM) || bounds.DriftPPM < 0 {
		return e, fmt.Errorf("invalid clock assumptions")
	}
	for _, slot := range []string{"relay", "w1", "w2", "shared"} {
		if _, ok := rows[slot]; !ok {
			return e, fmt.Errorf("missing independent clock evidence")
		}
	}
	e.Control = rows["relay"].ErrorBoundNS + rows["shared"].ErrorBoundNS
	e.Worker = rows["relay"].ErrorBoundNS + max(rows["w1"].ErrorBoundNS, rows["w2"].ErrorBoundNS)
	e.Drift = rows["relay"].RateErrorPPM + max(rows["shared"].RateErrorPPM, rows["w1"].RateErrorPPM, rows["w2"].RateErrorPPM)
	if e.Control > bounds.FrontendControlBoundNS || e.Worker > bounds.FrontendWorkerBoundNS || e.Drift > bounds.DriftPPM {
		return e, fmt.Errorf("measured clocks exceed approved bounds")
	}
	return e, nil
}

// SignalMidpoint preserves decimal nanoseconds without a float conversion.
func SignalMidpoint(before, after string, workerBound, assumedBound int64) (string, error) {
	if !nsPattern.MatchString(before) || !nsPattern.MatchString(after) || workerBound < 0 || assumedBound < 0 {
		return "", fmt.Errorf("missing signal bracket")
	}
	b, _ := strconv.ParseUint(before, 10, 64)
	a, _ := strconv.ParseUint(after, 10, 64)
	if a < b || (a-b+1)/2 > uint64(assumedBound) || workerBound > assumedBound-int64((a-b+1)/2) {
		return "", fmt.Errorf("signal bracket exceeds approved mapping")
	}
	return strconv.FormatUint(b+(a-b)/2, 10), nil
}
