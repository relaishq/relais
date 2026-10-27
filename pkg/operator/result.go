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
	SchemaVersion  int          `json:"schemaVersion"`
	ScenarioDigest string       `json:"scenarioDigest"`
	Status         Verdict      `json:"status"`
	Complete       bool         `json:"complete"`
	Latched        bool         `json:"latched"`
	Pending        []string     `json:"pending"`
	Gates          []GateResult `json:"gates"`
	Trace          []Trace      `json:"trace"`
	FirstError     *Trace       `json:"firstError,omitempty"`
}
