package main

import (
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"
	"time"

	"github.com/relaishq/relais/internal/loadgen"
	"github.com/relaishq/relais/internal/operatorgates/observe"
	"github.com/relaishq/relais/pkg/callharness"
)

// Failure facts are tested individually, before any validation or addition.
// Invalid siblings cannot cancel a positive failure counter or trusted verdict.
func callFailure(c *callharness.Report) bool {
	if c == nil {
		return false
	}
	d := c.DecryptionFailures
	if d.AuthTag > 0 || d.Replay > 0 || d.Other > 0 || c.ICERestarts > 0 {
		return true
	}
	for _, m := range c.Moves {
		if m.Lost || m.DecryptionFailuresAfterResume > 0 {
			return true
		}
		v := m.Measurement
		// A missing VideoExpected flag cannot erase an explicitly trusted video
		// failure. Audio-only reports leave those verdicts untrusted.
		verdicts := []callharness.MetricVerdict{v.AudioLoss, v.Audio.Verdict, v.ResumeVerdict, v.VideoLoss, v.Video.Verdict, v.FirstContentVerdict, v.FirstNewContentVerdict, v.FirstLiveVerdict}
		for _, verdict := range verdicts {
			if verdict.Trusted && verdict.Failed {
				return true
			}
		}
		if d := m.Distributed; d != nil {
			if d.WindowOutcome == "fail" {
				return true
			}
		}
	}
	return false
}
func reportFailure(r *loadgen.RunReport) bool {
	var trials []callharness.MoveReport
	for _, p := range r.Processes {
		for _, c := range p.Calls {
			if callFailure(c.Call) {
				return true
			}
			trials = append(trials, failureTrials(c.Call, c.Session)...)
		}
	}
	if reportCollections(r) != nil {
		trials = append([]callharness.MoveReport{{}}, trials...)
	}
	return trialsFailed(trials)
}

// Keep gap observations separate from unrelated diagnostic counters for
// failure judgment only. This never qualifies an invalid phase.
func failureTrials(c *callharness.Report, session string) []callharness.MoveReport {
	if c == nil || c.Distributed == nil {
		return nil
	}
	var trials []callharness.MoveReport
	for _, m := range c.Moves {
		if m.Distributed != nil {
			identity := m.Identity
			if identity != nil && (identity.SessionID != session || identity.RunID != c.Distributed.RunID) {
				identity = nil
			}
			trials = append(trials, callharness.MoveReport{Kind: m.Kind, Identity: identity, Distributed: m.Distributed})
		}
	}
	return trials
}
func trialsFailed(trials []callharness.MoveReport) bool {
	// Never drop malformed gaps from a quantile population: removing low values
	// could fabricate a p95 failure. Independent absolute failures are retained
	// separately. All arithmetic operands are checked before the product judge.
	valid := true
	seen := map[[3]string]bool{}
	run := ""
	for _, m := range trials {
		if m.Identity == nil || !m.Identity.Valid() || m.Distributed == nil || m.Kind != "move" && m.Kind != "takeover" {
			valid = false
			continue
		}
		id := *m.Identity
		key := [3]string{id.RunID, id.OperationID, id.SessionID}
		if seen[key] || run != "" && run != id.RunID {
			valid = false
			continue
		}
		seen[key], run = true, id.RunID
		for _, gap := range m.Distributed.Gaps {
			if observe.Numbers(gap) != nil {
				valid = false
				continue
			}
			// The unchanged judge's absolute per-observation pause failure.
			if len(gap.BaselineCertain) >= 10 && gap.EventGap.Lower >= 2*time.Second {
				return true
			}
		}
		valid = valid && observe.Numbers(m.Distributed.Gaps) == nil && gapCollections(m.Distributed.Gaps)
	}
	if !valid {
		return false
	}
	outcome, _ := callharness.JudgeDistributedTrials(trials)
	return outcome == "fail"
}

// Failure quantiles use the same complete structural graph as qualification,
// independently of generator validity and unrelated diagnostic counters.
func failureInput(input *previewMediaInput) []callharness.MoveReport {
	var trials []callharness.MoveReport
	p := loadgen.ProcessReport{ExpectedCalls: len(input.Sessions)}
	valid := true
	decoder := json.NewDecoder(strings.NewReader(input.CallerJSONL))
	for {
		var raw json.RawMessage
		err := decoder.Decode(&raw)
		if err == io.EOF {
			break
		}
		if err != nil {
			valid = false
			break
		}
		var m loadgen.Message
		if err := observe.DecodeEvidenceLimit(raw, &m, 64<<20); err != nil {
			if !observe.IsIncompleteEvidence(err) {
				valid = false
				continue
			}
			// Stats and unrelated diagnostics cannot erase valid gap failures.
			// Each raw gap still has the full recursive contract; typed joins
			// below enforce the complete operation/population graph.
			if observe.EvidenceTypeFields(raw, reflect.TypeFor[loadgen.Message](), reflect.TypeFor[callharness.DistributedGap]()) != nil {
				valid = false
			}
		}
		if p.Complete {
			valid = false
		}
		if p.PID == 0 {
			p.PID = m.PID
		}
		if m.PID <= 0 || p.PID != m.PID {
			valid = false
		}
		switch m.Type {
		case "call":
			p.Calls = append(p.Calls, m)
			if m.Call != nil && m.Call.Distributed != nil && m.Call.Distributed.RunID == input.RunID {
				for _, s := range input.Sessions {
					if s == m.Session {
						trials = append(trials, failureTrials(m.Call, m.Session)...)
						break
					}
				}
			}
		case "dial":
			p.Dials = append(p.Dials, m)
		case "stats": // Generator counters cannot erase a media failure.
		case "done":
			p.Complete = true
		default:
			valid = false
		}
	}
	r := loadgen.RunReport{Callers: len(input.Sessions), Events: input.Events, Processes: []loadgen.ProcessReport{p}}
	mediaScope := *input
	mediaScope.Missing = false // Envelope validity cannot erase independently valid failures.
	if !valid || phaseCollections(&mediaScope, &r) != nil {
		// Keep independently scoped absolute failures, but do not judge a
		// quantile over an incomplete or inconsistent operation population.
		trials = append([]callharness.MoveReport{{}}, trials...)
	}
	return trials
}
func poolFailure(input *previewPoolInput) bool {
	var trials []callharness.MoveReport
	for i := range input.Phases {
		if input.Phases[i].RunID == input.RunID {
			trials = append(trials, failureInput(&input.Phases[i])...)
		}
	}
	return trialsFailed(trials)
}

func callNumbers(c *callharness.Report) error {
	if c == nil {
		return nil
	}
	if err := observe.Numbers(c); err != nil {
		return err
	}
	total := int64(0)
	for _, n := range []int{c.DecryptionFailures.AuthTag, c.DecryptionFailures.Replay, c.DecryptionFailures.Other} {
		sum, ok := observe.AddCount(total, int64(n))
		if !ok {
			return fmt.Errorf("decryption total overflow")
		}
		total = sum
	}
	return callCollections(c)
}
