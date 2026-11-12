// Package operatorevidence joins raw caller evidence and invokes a judge built
// against an explicitly pinned public source snapshot. It owns no media policy.
package operatorevidence

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/relaishq/relais/internal/loadgen"
	"github.com/relaishq/relais/internal/operatorcaller"
	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/relaishq/relais/internal/operatorgates/observe"
	"github.com/relaishq/relais/internal/releasecompat"
	"github.com/relaishq/relais/pkg/operator"
)

type PhaseDiagnostic struct {
	Gate               string                `json:"gate"`
	EventCount         int                   `json:"eventCount"`
	Phase              int                   `json:"phase"`
	Included           bool                  `json:"included"`
	Cause              string                `json:"cause"`
	ExcludedEvents     int                   `json:"excludedEvents"`
	SaturationOverlaps int                   `json:"saturationOverlaps"`
	Issue              observe.EvidenceIssue `json:"issue"`
}

// Summary is the judge's pooled result. BoundaryMetrics lists takeover verdicts
// that pass for every kill time in the clock interval while their content
// label differs (#446). The judge trusts them and keeps the label here. The
// member is absent when there is none.
type Summary struct {
	CompleteMedia      bool              `json:"completeMedia"`
	LostCalls          int               `json:"lostCalls"`
	UndecidedMetrics   []UndecidedMetric `json:"undecidedMetrics"`
	IncludedPhases     int               `json:"includedPhases"`
	DroppedPhases      int               `json:"droppedPhases"`
	ExcludedEvents     int               `json:"excludedEvents"`
	ExcludedKills      int               `json:"excludedKills"`
	Excluded           *ExcludedPool     `json:"-"`
	BoundaryMetrics    []UndecidedMetric `json:"-"`
	SaturationOverlaps int               `json:"saturationOverlaps"`
	DuplicatePackets   uint64            `json:"duplicatePackets"`
	PhaseDiagnostics   []PhaseDiagnostic `json:"phaseDiagnostics"`

	Status              operator.Verdict `json:"status"`
	QualifiedMoves      int              `json:"qualifiedMoves"`
	QualifiedTakeovers  int              `json:"qualifiedTakeovers"`
	ExpectedCalls       int              `json:"expectedCalls"`
	CompletedCalls      int              `json:"completedCalls"`
	RelayCalls          int              `json:"relayCalls"`
	EventCount          int              `json:"eventCount"`
	MetricCount         int              `json:"metricCount"`
	FailedMetrics       int              `json:"failedMetrics"`
	InconclusiveMetrics int              `json:"inconclusiveMetrics"`
	MaxMediaGapNS       int64            `json:"maxMediaGapNs"`
	DecryptionFailures  int              `json:"decryptionFailures"`
	ICERestarts         int              `json:"iceRestarts"`
}

// ExcludedPool reports the takeovers left out of the pool for a wide kill
// bracket. CountedBackIn is the pooled status with them judged on their trusted
// brackets. OwnerSignOff marks a pass held back as inconclusive because an
// excluded takeover's non-timing checks did not all pass. The judge emits it
// only when excludedKills is nonzero; DecodeSummary then requires it.
type ExcludedPool struct {
	Takeovers     []ExcludedTakeover `json:"takeovers"`
	CountedBackIn operator.Verdict   `json:"countedBackIn"`
	OwnerSignOff  bool               `json:"ownerSignOff"`
}

// ExcludedTakeover is one excluded takeover's results when counted back in.
// Metrics lists each yardstick metric's outcome for that event.
type ExcludedTakeover struct {
	Phase           int              `json:"phase"`
	Event           int              `json:"event"`
	Calls           []ExcludedCall   `json:"calls"`
	Metrics         []ExcludedMetric `json:"metrics"`
	NonTimingPassed bool             `json:"nonTimingPassed"`
}
type ExcludedCall struct {
	WindowOutcome string         `json:"windowOutcome"`
	KillToLive    DurationBounds `json:"killToLive"`
	EventGaps     []TrackGap     `json:"eventGaps"`
}
type DurationBounds struct {
	LowerNS int64 `json:"lowerNs"`
	UpperNS int64 `json:"upperNs"`
}
type TrackGap struct {
	Kind string `json:"kind"`
	DurationBounds
}
type ExcludedMetric struct {
	Metric  string `json:"metric"`
	Outcome string `json:"outcome"`
}

// MarshalJSON preserves an explicit empty diagnostic inventory. Missing/null
// inventories still fail the recursive evidence decoder at the read boundary.
func (s Summary) MarshalJSON() ([]byte, error) {
	type wire Summary
	if s.UndecidedMetrics == nil {
		s.UndecidedMetrics = []UndecidedMetric{}
	}
	if s.PhaseDiagnostics == nil {
		s.PhaseDiagnostics = []PhaseDiagnostic{}
	}
	return json.Marshal(struct {
		wire
		Excluded        *ExcludedPool     `json:"excluded,omitempty"`
		BoundaryMetrics []UndecidedMetric `json:"boundaryMetrics,omitempty"`
	}{wire(s), s.Excluded, s.BoundaryMetrics})
}

// DecodeSummary validates presence once; missing counters never become zeros.
func DecodeSummary(raw []byte) (Summary, error) {
	var s Summary
	decodeErr := observe.DecodeEvidenceLimit(raw, &s, 256000)
	knownFailure := s.Status == operator.Failure || s.FailedMetrics > 0 || s.DecryptionFailures > 0 || s.ICERestarts > 0
	if decodeErr != nil || observe.Numbers(s) != nil {
		if knownFailure {
			return Summary{Status: operator.Failure}, nil
		}
		return s, errors.New("invalid-media-summary")
	}
	if s.Status != operator.Passed && s.Status != operator.Failure && s.Status != operator.Inconclusive {
		return s, errors.New("invalid-media-status")
	}
	if s.ExcludedKills > 0 {
		var excluded struct {
			Excluded ExcludedPool `json:"excluded"`
		}
		if observe.DecodeEvidenceLimit(raw, &excluded, 256000) != nil || observe.Numbers(excluded) != nil {
			if knownFailure {
				return Summary{Status: operator.Failure}, nil
			}
			return s, errors.New("invalid-media-summary")
		}
		s.Excluded = &excluded.Excluded
	}
	var optional struct {
		BoundaryMetrics json.RawMessage `json:"boundaryMetrics"`
	}
	if json.Unmarshal(raw, &optional) == nil && optional.BoundaryMetrics != nil {
		var boundary struct {
			BoundaryMetrics []UndecidedMetric `json:"boundaryMetrics"`
		}
		if observe.DecodeEvidenceLimit(raw, &boundary, 256000) != nil || observe.Numbers(boundary) != nil {
			if knownFailure {
				return Summary{Status: operator.Failure}, nil
			}
			return s, errors.New("invalid-media-summary")
		}
		s.BoundaryMetrics = boundary.BoundaryMetrics
	}
	trialsComplete := s.EventCount > 0 && s.MetricCount > 0
	if s.RelayCalls == s.CompletedCalls {
		trialsComplete = s.EventCount == 0 && s.MetricCount == 0 && s.QualifiedMoves == 0 && s.QualifiedTakeovers == 0
	}
	if s.FailedMetrics > 0 || s.DecryptionFailures > 0 || s.ICERestarts > 0 {
		s.Status = operator.Failure
	} else if s.Status == operator.Passed && (s.ExpectedCalls < 1 || s.CompletedCalls != s.ExpectedCalls || s.RelayCalls > s.CompletedCalls || !trialsComplete || s.InconclusiveMetrics > 0) {
		s.Status = operator.Inconclusive
	}
	return s, nil
}

type RawMedia struct {
	CallerJSONL string                   `json:"callerJsonl"`
	Events      []loadgen.EventResult    `json:"events"`
	Receipts    []operatorcaller.Receipt `json:"receipts"`
}
type Phase struct {
	Gate         operator.Gate
	Plan         operatorcaller.Plan
	Registration operatorcaller.Registration
	Raw          []byte
	FirstFaultAt time.Time
	DeployedSHA  string
	// Clocks and Inventory are successful results from Session.FreshClocks and
	// Session.FreshInventory. Their zero values mean evidence is unavailable.
	Clocks    clocks.Evidence
	Inventory releasecompat.Pool
}
type MediaInput struct {
	Gate     operator.Gate          `json:"gate,omitempty"`
	RunID    string                 `json:"runId"`
	Sessions []string               `json:"sessions"`
	Probes   []operatorcaller.Probe `json:"probes"`
	RawMedia
	Missing bool `json:"missing,omitempty"`
}
type Pool struct {
	ClockReceiptSHA256 string `json:"clockReceiptSha256,omitempty"`
	// PreCoverageEvidence opts historical archives into their frozen policy.
	// Live ReadPool never sets it. Coverage, when present, always wins.
	PreCoverageEvidence bool                     `json:"preCoverageEvidence,omitempty"`
	ClockCoverage       *clocks.CoverageEvidence `json:"clockCoverage,omitempty"`
	SourceCommit        string                   `json:"-"`
	RunID               string                   `json:"runId"`
	Phases              []MediaInput             `json:"phases"`
	Missing             bool                     `json:"missing"`
}

func ReadPool(run string, phases []Phase, missing bool) (Pool, string, error) {
	pool := Pool{RunID: run, Phases: []MediaInput{}, Missing: missing}
	sha := ""
	// Mixed pins are not one pool. Each release gets its own unchanged judge.
	for _, p := range phases {
		if sha != "" && sha != p.Plan.PublicSHA {
			return pool, "", errors.New("mixed-pool-source")
		}
		sha = p.Plan.PublicSHA
	}
	pool.SourceCommit = sha
	for _, p := range phases {
		r := p.Registration
		expected, err := operatorcaller.Register(p.Plan, r.RunID, r.Sessions, r.Burst, r.Host, time.UnixMilli(r.RegisteredAt), &p.FirstFaultAt)
		if err != nil || p.FirstFaultAt.IsZero() || p.DeployedSHA != p.Plan.PublicSHA || r.RunID != run || r.Phase != p.Plan.Phase || r.PublicSHA != p.Plan.PublicSHA || r.Near != expected.Near || !r.Near || p.Plan.Phase != operatorcaller.Survivor {
			pool.Missing = true
			continue
		}
		if p.Clocks.At.IsZero() || !p.Inventory.Complete {
			pool.Missing = true
		}
		var raw RawMedia
		phaseMissing := false
		if err := observe.DecodeEvidenceLimit(p.Raw, &raw, operator.MaxBlobBytes); err != nil {
			if !observe.IsIncompleteEvidence(err) {
				return pool, sha, errors.New("invalid-raw-media")
			}
			pool.Missing, phaseMissing = true, true
		}
		if observe.Numbers(raw.Events) != nil || observe.Numbers(raw.Receipts) != nil {
			pool.Missing = true
		}
		// Missing siblings cannot erase an independently scoped loss receipt or
		// caller failure. The unchanged judge receives every valid component.
		pool.Phases = append(pool.Phases, MediaInput{Gate: p.Gate, RunID: run, Sessions: append([]string{}, r.Sessions...), Probes: append([]operatorcaller.Probe{}, r.Burst.Probes...), RawMedia: raw, Missing: phaseMissing})
	}
	return pool, sha, nil
}
