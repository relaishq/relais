package main

import (
	"encoding/json"
	"io"
	"math"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/relaishq/relais/internal/loadgen"
	"github.com/relaishq/relais/internal/operatorgates/observe"
)

// This is an operator diagnostic contract, not a second product report type.
// The rehearsal decodes this projection; all policy runs in this pinned binary.
type generatorCheck struct {
	Rule     string  `json:"rule"`
	Observed float64 `json:"observed"`
	Limit    float64 `json:"limit"`
}
type generatorCalibration struct {
	Checks               []generatorCheck `json:"checks"`
	Passed               bool             `json:"passed"`
	Samples              int              `json:"samples"`
	MediaNS              int64            `json:"mediaNs"`
	FlaggedNS            int64            `json:"flaggedNs"`
	FlaggedPercent       float64          `json:"flaggedPercent"`
	PeakP99NS            int64            `json:"peakP99Ns"`
	PeakMaxNS            int64            `json:"peakMaxNs"`
	PeakCPUPerGOMAXPROCS float64          `json:"peakCpuPerGomaxprocs"`
	DecoderDropped       int              `json:"decoderDropped"`
	DecoderErrors        int              `json:"decoderErrors"`
	CompletedCalls       int              `json:"completedCalls"`
	DuplicatePackets     uint64           `json:"duplicatePackets"`
	Causes               []string         `json:"causes"`

	MediaWindow generatorMediaWindow `json:"mediaWindow"`
}

// Offsets use the product call's StartedAt as zero; no wall-clock identity leaves
// the pinned judge. These are the budget spans, without event-overlap expansion.
type generatorMediaWindow struct {
	FirstVideoSendOffsetMS float64                    `json:"firstVideoSendOffsetMs"`
	HangUpOffsetMS         float64                    `json:"hangUpOffsetMs"`
	FlaggedIntervals       []generatorFlaggedInterval `json:"flaggedIntervals"`
	ClippedFlaggedMS       float64                    `json:"clippedFlaggedMs"`
	MediaWindowMS          float64                    `json:"mediaWindowMs"`
}

type generatorFlaggedInterval struct {
	StartOffsetMS float64 `json:"startOffsetMs"`
	EndOffsetMS   float64 `json:"endOffsetMs"`
}

func (c *generatorCalibration) cause(code string) {
	if !slices.Contains(c.Causes, code) {
		c.Causes = append(c.Causes, code)
	}
}

// calibrateGenerator is an alternate entry into the SAME generator policy used
// by previewGeneratorDetails. It neither infers a media window from sample
// durations nor imposes a minimum packet count, CPU mask or sample population.
func calibrateGenerator(raw string) (c generatorCalibration) {
	c.MediaWindow.FlaggedIntervals = []generatorFlaggedInterval{}
	c.Checks = []generatorCheck{}
	c.Causes = []string{}
	defer func() {
		c.Passed = len(c.Causes) == 0
		if !c.Passed {
			for _, check := range c.Checks {
				c.cause(check.Rule)
			}
		}
	}()
	var checked strings.Builder
	var samples []json.RawMessage
	var pid int
	d := json.NewDecoder(strings.NewReader(raw))
	for {
		var line json.RawMessage
		err := d.Decode(&line)
		if err == io.EOF {
			break
		}
		if err != nil {
			c.cause("invalid-calibration-jsonl")
			break
		}
		var m loadgen.Message
		if observe.DecodeLimit(line, &m, observe.Contract{"type", "pid"}, 64<<20) != nil || observe.Numbers(m) != nil {
			c.cause("incomplete-calibration-evidence")
			break
		}
		if pid == 0 {
			pid = m.PID
		}
		if pid <= 0 || m.PID != pid {
			c.cause("calibration-process-differs")
			break
		}
		if m.Error != "" {
			c.cause("caller-error")
		}
		if m.Type == "stats" {
			if _, err := readSample(line, pid); err != nil {
				c.cause("missing-calibration-stats")
				break
			}
			samples = append(samples, line)
		}
		if m.Type == "call" {
			if m.Call == nil || observe.Require(line, observe.Contract{"call.StartedAt", "call.FirstVideoSentAt", "call.HungUpAt", "call.SentVideo.Frames", "call.DecryptionFailures.AuthTag", "call.DecryptionFailures.Replay", "call.DecryptionFailures.Other", "call.ICERestarts", "call.Tracks", "call.Tracks[].DuplicatePackets", "call.Tracks[].Video?.OnlineDecode.Dropped", "call.Tracks[].Video?.OnlineDecode.Errors"}) != nil {
				c.cause("incomplete-calibration-call")
				break
			}
			if callFailure(m.Call) {
				c.cause("known-media-failure")
			}
			for _, track := range m.Call.Tracks {
				c.DuplicatePackets += uint64(track.DuplicatePackets)
				if track.Video != nil {
					c.DecoderErrors += track.Video.OnlineDecode.Errors
					c.DecoderDropped += track.Video.OnlineDecode.Dropped
				}
			}
		}
		checked.Write(line)
		checked.WriteByte('\n')
	}
	p, err := loadgen.ReadSoakProcess(strings.NewReader(checked.String()), pid, 1, loadgen.DefaultLimits())
	c.Samples = len(p.Samples)
	c.CompletedCalls = len(p.Calls)
	c.PeakP99NS = int64(p.PeakIntervalP99)
	c.PeakCPUPerGOMAXPROCS = p.PeakCPUPerGOMAXPROCS
	for _, sample := range p.Samples {
		c.PeakMaxNS = max(c.PeakMaxNS, int64(sample.IntervalLateness.Max))
		for _, check := range generatorSampleChecks(sample) {
			if !slices.Contains(c.Checks, check) {
				c.Checks = append(c.Checks, check)
			}
		}
	}
	if err != nil || !p.Complete || len(p.Causes) > 0 {
		c.cause("incomplete-calibration-evidence")
	}
	dir, err := collectGeneratorSamples(samples)
	if err != nil {
		c.cause("generator-evidence-rejected")
		return c
	}
	defer os.RemoveAll(dir)
	r := loadgen.RunReport{Processes: []loadgen.ProcessReport{p}}
	budgets, excluded := applySaturation(dir, &r, nil, []string{"caller-1"})
	if len(budgets) != 1 {
		c.cause("generator-evidence-rejected")
		return c
	}
	b := budgets[0]
	c.Checks = append(c.Checks, b.Checks...)
	for _, check := range b.Checks {
		c.cause(check.Rule)
	}
	c.MediaNS = int64(math.Round(b.MediaSeconds * 1e9))
	c.FlaggedNS = int64(math.Round(b.FlaggedSeconds * 1e9))
	c.FlaggedPercent = b.Percent
	start, end := callerWindow(p)
	if len(p.Calls) == 1 && p.Calls[0].Call != nil && !start.IsZero() && end.After(start) {
		origin := p.Calls[0].Call.StartedAt
		offset := func(at time.Time) float64 { return float64(at.Sub(origin)) / float64(time.Millisecond) }
		c.MediaWindow = generatorMediaWindow{
			FirstVideoSendOffsetMS: offset(start), HangUpOffsetMS: offset(end),
			FlaggedIntervals: []generatorFlaggedInterval{},
			ClippedFlaggedMS: b.FlaggedSeconds * 1000, MediaWindowMS: b.MediaSeconds * 1000,
		}
		for _, f := range b.clipped {
			c.MediaWindow.FlaggedIntervals = append(c.MediaWindow.FlaggedIntervals, generatorFlaggedInterval{offset(f.Start), offset(f.End)})
		}
	}
	if b.Outcome != "pass" {
		if b.Percent > saturationPercentLimit {
			c.cause("generator-saturation-budget")
			c.Checks = append(c.Checks, generatorCheck{"flagged-percent-max", b.Percent, saturationPercentLimit})
		}
		if b.Percent <= saturationPercentLimit || len(b.Causes) > 1 {
			c.cause("generator-evidence-rejected")
		}
	}
	if len(excluded) > 0 {
		c.cause("generator-event-exclusion")
	}
	return c
}
