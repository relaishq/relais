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
	Event     string    `json:"event"`
	RequestID string    `json:"requestId,omitempty"`
	Certainty Certainty `json:"certainty,omitempty"`
	Code      string    `json:"code,omitempty"`
	AtUnixMS  string    `json:"atUnixMs"`
}

type Result struct {
	Diagnostics    []GateDiagnostic `json:"diagnostics,omitempty"`
	Uncertainties  []Uncertainty    `json:"uncertainties,omitempty"`
	SchemaVersion  int              `json:"schemaVersion"`
	ScenarioDigest string           `json:"scenarioDigest"`
	Status         Verdict          `json:"status"`
	Complete       bool             `json:"complete"`
	Latched        bool             `json:"latched"`
	Pending        []string         `json:"pending"`
	Gates          []GateResult     `json:"gates"`
	Trace          []Trace          `json:"trace"`
	FirstError     *Trace           `json:"firstError,omitempty"`
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
type MediaDiagnostic struct {
	Status              Verdict `json:"status"`
	ExpectedCalls       int     `json:"expectedCalls"`
	CompletedCalls      int     `json:"completedCalls"`
	FailedMetrics       int     `json:"failedMetrics"`
	InconclusiveMetrics int     `json:"inconclusiveMetrics"`
}
