package operator

// Results and traces contain fixed codes and public identities, never raw
// adapter payloads, script bytes, resource identifiers or error messages.
type Verdict string

const (
	Passed       Verdict = "passed"
	Failure      Verdict = "failed"
	Inconclusive Verdict = "inconclusive"
	NotRun       Verdict = "not-run"
)

type GateResult struct {
	Gate   Gate    `json:"gate"`
	Status Verdict `json:"status"`
	Reason string  `json:"reason,omitempty"`
}

type Trace struct {
	Operation   Operation `json:"operation,omitempty"`
	Target      string    `json:"target,omitempty"`
	Action      string    `json:"action,omitempty"`
	WaitPurpose string    `json:"waitPurpose,omitempty"`
	Event       string    `json:"event"`
	RequestID   string    `json:"requestId,omitempty"`
	Certainty   Certainty `json:"certainty,omitempty"`
	Code        string    `json:"code,omitempty"`
	AtUnixMS    string    `json:"atUnixMs"`
}

type Result struct {
	OmittedRefusals int              `json:"omittedRefusals,omitempty"`
	Refusals        []Refusal        `json:"refusals,omitempty"`
	Diagnostics     []GateDiagnostic `json:"diagnostics,omitempty"`
	Uncertainties   []Uncertainty    `json:"uncertainties,omitempty"`
	SchemaVersion   int              `json:"schemaVersion"`
	ScenarioDigest  string           `json:"scenarioDigest"`
	Status          Verdict          `json:"status"`
	Complete        bool             `json:"complete"`
	Latched         bool             `json:"latched"`
	Pending         []string         `json:"pending"`
	Gates           []GateResult     `json:"gates"`
	Trace           []Trace          `json:"trace"`
	FirstError      *Trace           `json:"firstError,omitempty"`
}

// GateDiagnostic retains bounded evidence projections, never adapter output.
type GateDiagnostic struct {
	Reason        string              `json:"reason,omitempty"`
	Gate          Gate                `json:"gate"`
	Procedure     ProcedureDiagnostic `json:"procedure"`
	Media         *MediaDiagnostic    `json:"media,omitempty"`
	Pool          *MediaDiagnostic    `json:"pool,omitempty"`
	PoolPending   bool                `json:"poolPending"`
	CallerStopped bool                `json:"callerStopped"`
}
type ProcedureDiagnostic struct {
	Completed      bool   `json:"completed"`
	Reason         string `json:"reason,omitempty"`
	CompletedSteps int    `json:"completedSteps"`
}

// MetricDiagnostic identifies a yardstick row by indexes, never call identities.
type MetricDiagnostic struct {
	Phase  int      `json:"phase"`
	Event  int      `json:"event"`
	Metric string   `json:"metric"`
	Causes []string `json:"causes"`
}
type EvidenceIssue struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}
type PhaseDiagnostic struct {
	Gate               string        `json:"gate"`
	EventCount         int           `json:"eventCount"`
	Phase              int           `json:"phase"`
	Included           bool          `json:"included"`
	Cause              string        `json:"cause"`
	ExcludedEvents     int           `json:"excludedEvents"`
	SaturationOverlaps int           `json:"saturationOverlaps"`
	Issue              EvidenceIssue `json:"issue"`
}
type MediaDiagnostic struct {
	OmittedUndecidedMetrics int                `json:"omittedUndecidedMetrics"`
	OmittedPhaseDiagnostics int                `json:"omittedPhaseDiagnostics"`
	IncludedPhases          int                `json:"includedPhases"`
	DroppedPhases           int                `json:"droppedPhases"`
	ExcludedEvents          int                `json:"excludedEvents"`
	SaturationOverlaps      int                `json:"saturationOverlaps"`
	UndecidedMetrics        []MetricDiagnostic `json:"undecidedMetrics"`
	PhaseDiagnostics        []PhaseDiagnostic  `json:"phaseDiagnostics"`
	Status                  Verdict            `json:"status"`
	ExpectedCalls           int                `json:"expectedCalls"`
	CompletedCalls          int                `json:"completedCalls"`
	FailedMetrics           int                `json:"failedMetrics"`
	InconclusiveMetrics     int                `json:"inconclusiveMetrics"`
}

// Refusal preserves the identity and budget of a proved pre-submission refusal.
// It contains no arguments, scripts, resource identifiers or adapter text.
type Refusal struct {
	RequestID      string    `json:"requestId"`
	Operation      Operation `json:"operation"`
	Target         string    `json:"target"`
	Action         string    `json:"action,omitempty"`
	DeadlineUnixMS string    `json:"deadlineUnixMs"`
	RemainingMS    int64     `json:"remainingMs"`
	Code           string    `json:"code"`
}
