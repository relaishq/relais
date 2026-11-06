// Package clocks checks independent-host evidence against approved bounds.
package clocks

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"time"

	"github.com/relaishq/relais/internal/operatorgates/observe"
)

type Sample struct {
	ProbeLog       json.RawMessage `json:"-"`
	TrackingLog    *TrackingLog    `json:"trackingLog,omitempty"`
	Tracking       string          `json:"tracking,omitempty"`
	ErrorBoundNS   int64           `json:"errorBoundNs"`
	RateErrorPPM   float64         `json:"rateErrorPpm"`
	MeasuredUnixNS string          `json:"measuredUnixNs"`
}

// EvidenceVersion covers recorded clock diagnostics, including the optional
// falsification evidence added in the next slice. It does not version verdicts.
const EvidenceVersion = 2

type TrackingRow struct {
	BeforeRun                  bool    `json:"beforeRun"`
	CombinedSources            int     `json:"combinedSources"`
	OffsetSDSeconds            float64 `json:"offsetSdSeconds"`
	RemainingCorrectionSeconds float64 `json:"remainingCorrectionSeconds"`
	UnixSeconds                int64   `json:"unixSeconds"`
	Reference                  string  `json:"reference,omitempty"`
	Stratum                    int     `json:"stratum"`
	FrequencyPPM               float64 `json:"frequencyPpm"`
	SkewPPM                    float64 `json:"skewPpm"`
	OffsetSeconds              float64 `json:"offsetSeconds"`
	RootDelaySeconds           float64 `json:"rootDelaySeconds"`
	RootDispersionSeconds      float64 `json:"rootDispersionSeconds"`
	MaxErrorSeconds            float64 `json:"maxErrorSeconds"`
	LeapStatus                 string  `json:"leapStatus"`
}
type TrackingBreak struct {
	BeforeRun       bool   `json:"beforeRun"`
	FromUnixSeconds int64  `json:"fromUnixSeconds"`
	Kind            string `json:"kind"`
	UnixSeconds     int64  `json:"unixSeconds"`
}
type TrackingLog struct {
	Cursor         json.RawMessage `json:"cursor"`
	Rows           []TrackingRow   `json:"rows"`
	Breaks         []TrackingBreak `json:"breaks"`
	RunStartUnixMS int64           `json:"runStartUnixMs"`
}

// PublicSample retains numeric tracking evidence without exposing source identities.
func PublicSample(s Sample) Sample {
	s.ProbeLog = nil
	s.Tracking = ""
	if s.TrackingLog == nil {
		return s
	}
	log := *s.TrackingLog
	log.Rows = append([]TrackingRow{}, log.Rows...)
	for i := range log.Rows {
		log.Rows[i].Reference = ""
		switch log.Rows[i].LeapStatus {
		case "N", "+", "-", "?":
		default:
			log.Rows[i].LeapStatus = "unavailable"
		}
	}
	log.Breaks = append([]TrackingBreak{}, log.Breaks...)
	for i := range log.Breaks {
		switch log.Breaks[i].Kind {
		case "cursor-gap", "log-rotation", "log-loss", "clock-step", "daemon-restart", "source-change", "leap-status", "self-consistency":
		default:
			log.Breaks[i].Kind = "unavailable"
		}
	}
	// The live cursor stays in the session. Public evidence retains only its file
	// position and content anchor, never an opaque host-supplied JSON object.
	var cursor struct {
		Position int64  `json:"position"`
		Anchor   string `json:"anchor"`
	}
	if len(log.Cursor) > 0 && string(log.Cursor) != "null" && json.Unmarshal(log.Cursor, &cursor) == nil {
		cursor.Anchor = fmt.Sprintf("%x", sha256.Sum256([]byte(cursor.Anchor)))
		log.Cursor, _ = json.Marshal(cursor)
	} else {
		log.Cursor = nil
	}
	s.TrackingLog = &log
	return s
}

type Bounds struct {
	FrontendControlBoundNS int64   `json:"frontendControlBoundNs"`
	FrontendWorkerBoundNS  int64   `json:"frontendWorkerBoundNs"`
	DriftPPM               float64 `json:"driftPpm"`
}
type Evidence struct {
	Version         int `json:"version"`
	At              time.Time
	Clocks          map[string]Sample
	Control, Worker int64
	Drift           float64
	Falsification   *Falsification `json:"falsification,omitempty"`
}

const maxSafeInteger = 1<<53 - 1

var nsPattern = regexp.MustCompile(`^[0-9]{19}$`)

func Parse(data []byte) (s Sample, err error) {
	baseline, probe, err := stripProbeLog(data)
	if err != nil {
		return s, err
	}
	err = observe.Decode(baseline, &s, observe.Contract{"errorBoundNs", "rateErrorPpm", "measuredUnixNs"})
	s.ProbeLog = probe
	if err == nil && (s.ErrorBoundNS < 0 || s.ErrorBoundNS > maxSafeInteger || !finite(s.RateErrorPPM) || s.RateErrorPPM < 0 || !nsPattern.MatchString(s.MeasuredUnixNS)) {
		err = fmt.Errorf("missing independent clock evidence")
	}
	return
}
func finite(x float64) bool { return !math.IsNaN(x) && !math.IsInf(x, 0) }

func Check(rows map[string]Sample, bounds Bounds, frontendSlot, controlSlot string) (Evidence, error) {
	e := Evidence{Version: EvidenceVersion, Clocks: rows}
	if bounds.FrontendControlBoundNS < 0 || bounds.FrontendWorkerBoundNS < 0 || !finite(bounds.DriftPPM) || bounds.DriftPPM < 0 {
		return e, fmt.Errorf("invalid clock assumptions")
	}
	for _, slot := range []string{"relay", "w1", "w2", "shared"} {
		if _, ok := rows[slot]; !ok {
			return e, fmt.Errorf("missing independent clock evidence")
		}
	}
	frontend, frontendOK := rows[frontendSlot]
	control, controlOK := rows[controlSlot]
	if !frontendOK || !controlOK {
		return e, fmt.Errorf("missing independent clock evidence")
	}
	e.Control = frontend.ErrorBoundNS + control.ErrorBoundNS
	e.Worker = frontend.ErrorBoundNS + max(rows["w1"].ErrorBoundNS, rows["w2"].ErrorBoundNS)
	e.Drift = frontend.RateErrorPPM + max(control.RateErrorPPM, rows["w1"].RateErrorPPM, rows["w2"].RateErrorPPM)
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
