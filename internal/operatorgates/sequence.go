package operatorgates

import (
	"context"
	"strings"
	"time"

	"github.com/relaishq/relais/pkg/operator"
)

// SequencePorts supplies caller preparation and evidence integration. All gate
// order, cutoffs, stop decisions and final pooling remain public here.
type SequencePorts struct {
	Prepare        func(context.Context, operator.Gate, time.Duration, time.Duration) error
	ReleaseCallers func(context.Context, string, time.Duration) bool
	Minimum        func(operator.Gate) time.Duration
	Command        func(context.Context, operator.Gate, time.Duration) ProcedureResult
	Finalize       func(context.Context, time.Duration) (*MediaSummary, error)
}
type SequenceResult struct {
	Gates      []operator.GateResult
	Procedures []ProcedureResult
	Pool       *MediaSummary
	Status     operator.Verdict
}

func (s *Session) RunSequence(ctx context.Context, p SequencePorts) SequenceResult {
	out := SequenceResult{Status: operator.Inconclusive}
	awaiting := map[int]bool{}
	for _, gate := range s.gates {
		result := ProcedureResult{Status: operator.NotRun}
		release := false
		releaseReason := "gate-not-run"
		minimum := time.Duration(0)
		if p.Minimum != nil {
			minimum = p.Minimum(gate)
		}
		if ctx.Err() != nil {
			s.Halt("interrupted")
			result.Reason = "interrupted"
		} else if s.Latched() {
			result.Reason = "prior-gate-stopped"
		} else {
			latest := s.Budget.MutationEnd() - minimum
			if p.Prepare != nil && latest <= s.clock.Elapsed() {
				result.Reason = "gate-deadline"
			} else {
				var prep error
				var preparationEnd time.Duration
				if p.Prepare != nil {
					end := min(s.clock.Elapsed()+15*time.Minute, latest)
					preparationEnd = end
					prepCtx, cancel := context.WithTimeout(ctx, max(time.Duration(0), end-s.clock.Elapsed()))
					gateEnd := s.Budget.end
					if p.Finalize != nil {
						gateEnd = s.Budget.MutationEnd()
					}
					release = true
					prep = p.Prepare(prepCtx, gate, end, gateEnd)
					if prep == nil && s.clock.Elapsed() >= end {
						prep = fault("command-result-unknown")
					}
					cancel()
				}
				if prep != nil {
					switch reason := code(prep); {
					case reason == "caller-not-near", reason == "precondition-unhealthy", reason == "precondition-failed", strings.HasPrefix(reason, "precondition-failed:"):
						result.Status = operator.Inconclusive
						result.Reason = reason
						if classified, category := readFailure(prep); classified != "" {
							result.Reason, result.Category = classified, category
						}
					case reason == "gate-deadline":
						result.Reason = code(prep)
					default:
						if len(s.uncertainties) == 0 {
							s.unknownScope("gate.prepare", "session", string(gate), preparationEnd, transportRule(prep, "operation-evidence-unknown"))
						}
						result.Status = operator.Inconclusive
						result.Reason = "command-result-unknown"
						s.Halt(result.Reason)
					}
				} else {
					// Admit against the same instant that fixes the window. A full
					// 20-minute minimum must not lose to clock movement between reads.
					now := s.clock.Elapsed()
					end, ok := s.beginGate(now)
					if p.Finalize != nil {
						end = min(end, s.Budget.MutationEnd())
					}
					if !ok || end <= now || minimum > end-now {
						result.Reason = "gate-deadline"
						s.activeEnd = nil

					} else if p.Command != nil {
						releaseReason = "gate-ended"
						gateCtx, cancel := context.WithTimeout(ctx, end-s.clock.Elapsed())
						release = true
						result = p.Command(gateCtx, gate, end)
						cancel()
						if result.Media == nil {
							if result.Status == operator.Passed {
								result.Status = operator.Inconclusive
							}
							result.AwaitingPool = false
						}
						if result.Media != nil && result.Media.Status == operator.Failure {
							result.Status = operator.Failure
						} else if result.Media != nil && result.Media.Status == operator.Inconclusive && result.Status == operator.Passed {
							result.Status = operator.Inconclusive
						}
						complete := s.CompleteGate(result.GateResult(gate), end)
						result.Status = complete.Status
						result.Reason = complete.Reason
						if result.AwaitingPool && result.Status == operator.Inconclusive && result.Reason == "" {
							awaiting[len(out.Gates)] = true
						}
					}
				}
			}
		}
		releaseEnd := min(s.Budget.end, s.clock.Elapsed()+time.Minute)
		if release && p.ReleaseCallers != nil && !p.ReleaseCallers(ctx, releaseReason, releaseEnd) {
			if len(s.uncertainties) == 0 {
				s.unknownScope("caller.stop", "caller", "stop", releaseEnd, "unconfirmed-stop")
			}
			s.Halt("command-result-unknown")
			result = Stopped(false, result)
		}
		out.Gates = append(out.Gates, result.GateResult(gate))
		out.Procedures = append(out.Procedures, result)
	}
	// This callback is read-only evidence retention, allowed after the latch.
	// It cannot extend the original minute-105 budget or authorize another effect.
	if p.Finalize != nil && ctx.Err() == nil && s.Budget.Remaining() > 0 {
		end := s.Budget.PoolEnd()
		poolCtx, cancel := context.WithTimeout(ctx, end-s.clock.Elapsed())
		pool, err := p.Finalize(poolCtx, end)
		cancel()
		if pool != nil && (err == nil && s.clock.Elapsed() < end || pool.Status == operator.Failure) {
			out.Pool = pool
		}
	}
	for i := range awaiting {
		if out.Gates[i].Status != operator.Failure && out.Pool != nil {
			out.Gates[i].Status = out.Pool.Status
		}
	}
	out.Status = operator.Passed
	for _, g := range out.Gates {
		if g.Status == operator.Failure {
			out.Status = operator.Failure
			break
		}
		if g.Status != operator.Passed {
			out.Status = operator.Inconclusive
		}
	}
	if out.Pool != nil && out.Pool.Status == operator.Failure {
		out.Status = operator.Failure
	} else if p.Finalize != nil && (out.Pool == nil || out.Pool.Status != operator.Passed) && out.Status != operator.Failure {
		out.Status = operator.Inconclusive
	}
	if s.Latched() && out.Status != operator.Failure {
		out.Status = operator.Inconclusive
	}
	s.retainGateDiagnostics(&out, nil)
	return out
}

// RunAdmission preserves the two distinct call populations. The boundary is
// measured after the survivor has stopped and is never pooled with its media.
type AdmissionPorts struct {
	Survivor     func(context.Context, time.Duration) MeasureResult
	StopSurvivor func(context.Context, time.Duration) bool
	Idle         func(context.Context, time.Duration) error
	Boundary     func(context.Context, time.Duration) MeasureResult
}
type AdmissionResult struct {
	Survivor, Boundary MeasureResult
	Status             operator.Verdict
	Reason             string
}

func RunAdmission(ctx context.Context, p AdmissionPorts, end time.Duration) AdmissionResult {
	r := AdmissionResult{Survivor: p.Survivor(ctx, end), Status: operator.Inconclusive}
	if r.Survivor.Status == operator.Failure {
		r.Status = operator.Failure
	}
	if r.Survivor.Reason == "command-result-unknown" || r.Survivor.Reason == "gate-deadline" {
		r.Reason = r.Survivor.Reason
		return r
	}
	if !p.StopSurvivor(ctx, end) {
		r.Reason = "command-result-unknown"
		return r
	}
	if err := p.Idle(ctx, end); err != nil {
		r.Reason = code(err)
		if code(err) == "known-call-loss" {
			r.Status = operator.Failure
		}
		return r
	}
	r.Boundary = p.Boundary(ctx, end)
	if r.Boundary.Status == operator.Failure {
		r.Status = operator.Failure
	}
	if r.Boundary.Reason != "" {
		r.Reason = r.Boundary.Reason
	}
	return r
}
