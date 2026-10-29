package operatorrehearsal

import (
	"context"
	"errors"

	"github.com/relaishq/relais/internal/operatorevidence"
	"github.com/relaishq/relais/internal/operatorgates/observe"
)

type Calibration struct {
	Checks               []CalibrationCheck `json:"checks"`
	Passed               bool               `json:"passed"`
	Samples              int                `json:"samples"`
	MediaNS              int64              `json:"mediaNs"`
	FlaggedNS            int64              `json:"flaggedNs"`
	FlaggedPercent       float64            `json:"flaggedPercent"`
	PeakP99NS            int64              `json:"peakP99Ns"`
	PeakMaxNS            int64              `json:"peakMaxNs"`
	PeakCPUPerGOMAXPROCS float64            `json:"peakCpuPerGomaxprocs"`
	DecoderDropped       int                `json:"decoderDropped"`
	DecoderErrors        int                `json:"decoderErrors"`
	CompletedCalls       int                `json:"completedCalls"`
	DuplicatePackets     uint64             `json:"duplicatePackets"`
	Causes               []string           `json:"causes"`

	MediaWindow CalibrationMediaWindow `json:"mediaWindow"`
}

// CalibrationMediaWindow uses Call.StartedAt as zero and contains only the
// judge's clipped, merged budget intervals, in fractional milliseconds.
type CalibrationMediaWindow struct {
	FirstVideoSendOffsetMS float64                      `json:"firstVideoSendOffsetMs"`
	HangUpOffsetMS         float64                      `json:"hangUpOffsetMs"`
	FlaggedIntervals       []CalibrationFlaggedInterval `json:"flaggedIntervals"`
	ClippedFlaggedMS       float64                      `json:"clippedFlaggedMs"`
	MediaWindowMS          float64                      `json:"mediaWindowMs"`
}

type CalibrationFlaggedInterval struct {
	StartOffsetMS float64 `json:"startOffsetMs"`
	EndOffsetMS   float64 `json:"endOffsetMs"`
}

// Calibrate delegates all sample selection, cadence, window and saturation
// decisions to the exact pinned judge used by the gates.
func Calibrate(ctx context.Context, judge operatorevidence.PinnedJudge, raw string) (Calibration, error) {
	var c Calibration
	result, err := judge.Calibrate(ctx, raw)
	if err != nil {
		c.cause("calibration-judge-unavailable")
		return c, err
	}
	if observe.DecodeEvidenceLimit(result, &c, 64<<20) != nil || observe.Numbers(c) != nil {
		c = Calibration{}
		c.cause("incomplete-calibration-evidence")
		return c, errors.New("incomplete-calibration-evidence")
	}
	return cleanCalibration(c), nil
}
