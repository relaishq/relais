package callharness

import (
	"slices"
	"time"
)

// MaxTimingProbesPerEndpoint bounds mapping diagnostics, including binding probes.
const MaxTimingProbesPerEndpoint = 8

// MaxTimingCandidateStates is shared by first_content and first_new_content.
const MaxTimingCandidateStates = 16

// TimingEvidence is a bounded, optional explanation of the existing interval
// calculation. Coordinates in candidate rows are nanoseconds since call start.
// No session, worker, packet or content identities are exported here.
type TimingEvidence struct {
	Version           int                       `json:"version"`
	Start             ClockMappingEvidence      `json:"start"`
	End               ClockMappingEvidence      `json:"end"`
	PlannedSpan       *PlannedSpanEvidence      `json:"planned_span,omitempty"`
	Candidates        []ClassificationCandidate `json:"candidates"`
	OmittedCandidates int                       `json:"omitted_candidate_observations"`
}

type ClockMappingEvidence struct {
	Server        time.Time      `json:"server"`
	Host          OffsetInterval `json:"host_bound"`
	DriftPPM      float64        `json:"drift_ppm"`
	Mapped        TimeInterval   `json:"mapped"`
	Probes        []MappingProbe `json:"probes"`
	LowerProbe    int            `json:"lower_probe"`
	UpperProbe    int            `json:"upper_probe"`
	OmittedProbes int            `json:"omitted_probes"`
}

type MappingProbe struct {
	Index          int            `json:"index"`
	Probe          ClockProbe     `json:"timestamps"`
	Residual       OffsetInterval `json:"residual"`
	ResidualWidth  time.Duration  `json:"residual_width_ns"`
	AnchorDistance time.Duration  `json:"anchor_distance_ns"`
	Drift          time.Duration  `json:"drift_per_side_ns"`
	// Age brackets the event minus probe-send time, on the caller timeline.
	Age OffsetInterval `json:"age_ns"`
}

type PlannedSpanEvidence struct {
	Raw           time.Duration    `json:"raw_ns"`
	HostAllowance time.Duration    `json:"host_allowance_per_side_ns"`
	DriftPPM      float64          `json:"drift_ppm"`
	Bounds        DurationInterval `json:"bounds"`
}

type BoundaryChanges struct {
	PacketResume    bool `json:"packet_resume"`
	KeyframeRequest bool `json:"keyframe_request"`
	PriorReceipt    bool `json:"prior_receipt"`
	DecodedFrame    bool `json:"decoded_frame_selection"`
	ContentWindow   bool `json:"content_window"`
	LatencyBaseline bool `json:"latency_baseline"`
}

// ClassificationCandidate preserves one representative of each distinct label
// and trust/failure state. Comparing rows exposes packet-resume changes, source
// receipt/request boundary crossings, and decoded-frame selection changes.
type ClassificationCandidate struct {
	// Changed compares this representative with the first row for its metric.
	Changed                 BoundaryChanges   `json:"changed_boundary_witnesses"`
	Metric                  string            `json:"metric"`
	Label                   string            `json:"label"`
	Trusted                 bool              `json:"trusted"`
	Failed                  bool              `json:"failed"`
	StartRange              *DurationInterval `json:"start_range,omitempty"`
	EndRange                *DurationInterval `json:"end_range,omitempty"`
	Start                   time.Duration     `json:"start_ns"`
	End                     time.Duration     `json:"end_ns"`
	BaselineFloor           time.Duration     `json:"baseline_floor_ns"`
	Resume                  time.Duration     `json:"packet_resume_ns"`
	FramePresent            bool              `json:"frame_present"`
	FrameSent               time.Duration     `json:"frame_sent_ns"`
	FrameDecoded            time.Duration     `json:"frame_decoded_ns"`
	PriorReturn             time.Duration     `json:"prior_return_ns"`
	Requested               time.Duration     `json:"keyframe_request_ns"`
	PriorReceiptBeforeStart bool              `json:"prior_receipt_before_start"`
	RequestedAfterStart     bool              `json:"keyframe_requested_after_start"`
	SourceInWindow          bool              `json:"source_in_window"`
	LatencyAboveBaseline    bool              `json:"latency_above_baseline"`
}

func (r *recorder) retainClassificationCandidate(d *TimingEvidence, metric string, verdict MetricVerdict, m EventMeasurement, p MoveReport, floor, limit time.Duration) {
	if d == nil || !m.VideoExpected {
		return
	}
	label, sent := m.FirstContent, m.FirstContentSentAt
	if metric == "first_new_content" {
		label, sent = m.FirstNewContent, m.FirstNewContentSentAt
	}
	if index := slices.IndexFunc(d.Candidates, func(c ClassificationCandidate) bool {
		return c.Metric == metric && c.Label == label && c.Trusted == verdict.Trusted && c.Failed == verdict.Failed
	}); index >= 0 {
		row := &d.Candidates[index]
		row.StartRange.Lower, row.StartRange.Upper = min(row.StartRange.Lower, p.Start), max(row.StartRange.Upper, p.Start)
		row.EndRange.Lower, row.EndRange.Upper = min(row.EndRange.Lower, p.End), max(row.EndRange.Upper, p.End)
		return
	}
	if len(d.Candidates) == MaxTimingCandidateStates {
		d.OmittedCandidates++
		return
	}
	row := ClassificationCandidate{StartRange: &DurationInterval{Lower: p.Start, Upper: p.Start}, EndRange: &DurationInterval{Lower: p.End, Upper: p.End}, Metric: metric, Label: label, Trusted: verdict.Trusted, Failed: verdict.Failed, Start: p.Start, End: p.End, BaselineFloor: floor, Resume: m.MediaResumedAt}
	selectFrame := func(frames []frameMark) {
		for _, f := range frames {
			if !f.decodable || f.at < m.MediaResumedAt || f.at >= limit || f.source.at != sent || row.FramePresent && f.at >= row.FrameDecoded {
				continue
			}
			if metric == "first_new_content" && (f.repeated || f.at != p.Start+m.FirstNewFrame) {
				continue
			}
			row.FramePresent, row.FrameSent, row.FrameDecoded = true, f.source.at, f.at
			row.PriorReturn, row.Requested = f.source.returnedAt, f.source.requestedAt
			row.PriorReceiptBeforeStart = f.source.returnedAt > 0 && f.source.returnedAt >= f.source.at && f.source.returnedAt < p.Start
			row.RequestedAfterStart = f.source.pli && f.source.requestedAt >= p.Start
			row.SourceInWindow = f.source.at >= m.WindowStart && f.source.at < m.MediaResumedAt
			arrived := f.receivedAt
			if arrived == 0 {
				arrived = f.at
				if f.source.returnedAt >= m.MediaResumedAt && f.source.returnedAt <= f.at {
					arrived = f.source.returnedAt
				}
			}
			row.LatencyAboveBaseline = arrived-f.source.at > m.Video.BaselineP95+m.Video.Tolerance
		}
	}
	if label != "" {
		if r.prepared != nil {
			selectFrame(r.prepared.frames)
		} else {
			for _, track := range r.tracks {
				if track.video != nil {
					selectFrame(track.video.frames)
				}
			}
		}
	}
	for _, first := range d.Candidates {
		if first.Metric == metric {
			row.Changed = BoundaryChanges{
				PacketResume:    row.Resume != first.Resume,
				KeyframeRequest: row.RequestedAfterStart != first.RequestedAfterStart,
				PriorReceipt:    row.PriorReceiptBeforeStart != first.PriorReceiptBeforeStart,
				DecodedFrame:    row.FramePresent != first.FramePresent || row.FrameDecoded != first.FrameDecoded || row.FrameSent != first.FrameSent,
				ContentWindow:   row.SourceInWindow != first.SourceInWindow,
				LatencyBaseline: row.LatencyAboveBaseline != first.LatencyAboveBaseline,
			}
			break
		}
	}
	d.Candidates = append(d.Candidates, row)
}
