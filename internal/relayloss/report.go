// Package relayloss defines sanitized evidence for the operator host-loss drill.
package relayloss

import "time"

const Budget = 5 * time.Second
const CallCount = 8

// Call uses an ordinal rather than a session identifier.
type Call struct {
	Number    int           `json:"call"`
	Gap       time.Duration `json:"gapNs"`
	Recovered bool          `json:"recovered"`
}

type Report struct {
	Completed bool   `json:"completed"`
	Calls     []Call `json:"calls"`
}

// Passed requires every call and the entire fault/restore procedure. Missing
// recovery evidence cannot pass as a zero-length gap.
func (r Report) Passed() bool {
	if !r.Completed || len(r.Calls) != CallCount {
		return false
	}
	for i, c := range r.Calls {
		if c.Number != i+1 || !c.Recovered || c.Gap <= 0 || c.Gap > Budget {
			return false
		}
	}
	return true
}
