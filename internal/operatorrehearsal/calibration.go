package operatorrehearsal

import (
	"encoding/json"
	"errors"
	"io"
	"math"
	"strings"
	"time"

	"github.com/relaishq/relais/internal/loadgen"
	"github.com/relaishq/relais/internal/operatorgates/observe"
)

type Calibration struct {
	Passed               bool     `json:"passed"`
	Samples              int      `json:"samples"`
	MediaNS              int64    `json:"mediaNs"`
	FlaggedNS            int64    `json:"flaggedNs"`
	FlaggedPercent       float64  `json:"flaggedPercent"`
	PeakP99NS            int64    `json:"peakP99Ns"`
	PeakMaxNS            int64    `json:"peakMaxNs"`
	PeakCPUPerGOMAXPROCS float64  `json:"peakCpuPerGomaxprocs"`
	DecoderDropped       int      `json:"decoderDropped"`
	DecoderErrors        int      `json:"decoderErrors"`
	CompletedCalls       int      `json:"completedCalls"`
	DuplicatePackets     uint64   `json:"duplicatePackets"`
	Causes               []string `json:"causes"`
}

// Calibration measures the actual unchanged caller with no operator faults.
// Intervals are weighted by elapsed time, never by tick count or mean p99.
// This is an admission check, not a replacement for the pinned event guards.
func Calibrate(raw string) (Calibration, error) {
	c := Calibration{Causes: []string{}}
	d := json.NewDecoder(strings.NewReader(raw))
	var previous time.Time
	var pid int
	for {
		var line json.RawMessage
		e := d.Decode(&line)
		if e == io.EOF {
			break
		}
		if e != nil {
			return c, errors.New("invalid-calibration-jsonl")
		}
		var m loadgen.Message
		if observe.DecodeLimit(line, &m, observe.Contract{"type", "pid"}, 64<<20) != nil || observe.Numbers(m) != nil {
			return c, errors.New("incomplete-calibration-evidence")
		}
		if pid == 0 {
			pid = m.PID
		}
		if m.PID <= 0 || m.PID != pid {
			return c, errors.New("calibration-process-differs")
		}
		if m.Error != "" {
			c.Causes = append(c.Causes, "caller-error")
		}
		if m.Type == "stats" {
			if observe.Require(line, observe.Contract{"stats.at", "stats.interval_ns", "stats.gomaxprocs", "stats.cpu_percent_per_gomaxprocs", "stats.lateness.packets", "stats.interval_lateness.packets", "stats.interval_lateness.p99_ns", "stats.interval_lateness.max_ns", "stats.interval_pacing.sent_frames", "stats.interval_pacing.skipped_frames"}) != nil || m.Stats == nil {
				return c, errors.New("missing-calibration-stats")
			}
			s := *m.Stats
			if s.Interval <= 0 || s.Interval > time.Minute || s.GOMAXPROCS != 2 || s.IntervalLateness.Packets == 0 || s.IntervalLateness.P99 < 0 || s.IntervalLateness.Max < s.IntervalLateness.P99 || math.IsNaN(s.CPUPerGOMAXPROCS) || math.IsInf(s.CPUPerGOMAXPROCS, 0) || s.CPUPerGOMAXPROCS < 0 || s.At.IsZero() || !previous.IsZero() && !s.At.After(previous) {
				return c, errors.New("invalid-calibration-sample")
			}
			if !previous.IsZero() {
				delta := s.At.Sub(previous) - s.Interval
				if delta < -25*time.Millisecond || delta > 25*time.Millisecond {
					return c, errors.New("calibration-clock-discontinuity")
				}
			}
			previous = s.At
			if c.MediaNS > math.MaxInt64-int64(s.Interval) {
				return c, errors.New("calibration-overflow")
			}
			c.Samples++
			c.MediaNS += int64(s.Interval)
			c.PeakP99NS = max(c.PeakP99NS, int64(s.IntervalLateness.P99))
			c.PeakMaxNS = max(c.PeakMaxNS, int64(s.IntervalLateness.Max))
			c.PeakCPUPerGOMAXPROCS = max(c.PeakCPUPerGOMAXPROCS, s.CPUPerGOMAXPROCS)
			limits := loadgen.DefaultLimits()
			if len(limits.Causes(s)) > 0 || s.IntervalLateness.Max > 500*time.Millisecond || s.Interval > time.Second+25*time.Millisecond {
				c.FlaggedNS += int64(s.Interval)
			}
		}
		if m.Type == "call" {
			if m.Call == nil || observe.Require(line, observe.Contract{"call.DecryptionFailures.AuthTag", "call.DecryptionFailures.Replay", "call.DecryptionFailures.Other", "call.ICERestarts", "call.Tracks[]", "call.Tracks[].DuplicatePackets"}) != nil {
				return c, errors.New("incomplete-calibration-call")
			}
			c.CompletedCalls++
			if m.Call.DecryptionFailures.AuthTag+m.Call.DecryptionFailures.Replay+m.Call.DecryptionFailures.Other > 0 || m.Call.ICERestarts > 0 {
				c.Causes = append(c.Causes, "known-media-failure")
			}
			for _, t := range m.Call.Tracks {
				c.DuplicatePackets += uint64(t.DuplicatePackets)
				if t.Video != nil {
					c.DecoderDropped += t.Video.OnlineDecode.Dropped
					c.DecoderErrors += t.Video.OnlineDecode.Errors
				}
			}
		}
	}
	if c.MediaNS > 0 {
		c.FlaggedPercent = 100 * float64(c.FlaggedNS) / float64(c.MediaNS)
	}
	if c.Samples < 30 || c.MediaNS < int64(30*time.Second) {
		c.Causes = append(c.Causes, "insufficient-calibration-samples")
	}
	if c.CompletedCalls != 1 {
		c.Causes = append(c.Causes, "incomplete-calibration-call")
	}
	if c.FlaggedPercent > 1 {
		c.Causes = append(c.Causes, "generator-saturation-budget")
	}
	if c.DuplicatePackets > 0 || c.DecoderErrors > 0 {
		c.Causes = append(c.Causes, "known-media-failure")
	}
	c.Passed = len(c.Causes) == 0
	return c, nil
}
