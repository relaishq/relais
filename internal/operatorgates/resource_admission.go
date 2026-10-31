package operatorgates

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/relaishq/relais/internal/loadgen"
	"github.com/relaishq/relais/internal/operatorcaller"
	"github.com/relaishq/relais/internal/operatorgates/observe"
	"github.com/relaishq/relais/pkg/operator"
)

// AdmissionPlan retains the campaign's explicit durations and seven-minute
// combined preparation/stop reserve. Each manifest still has its own 20m cap.
type AdmissionPlan struct {
	SurvivorSeconds int `json:"survivorSeconds"`
	BoundarySeconds int `json:"boundarySeconds"`
}

func (p AdmissionPlan) minimum() time.Duration {
	return (time.Duration(p.SurvivorSeconds)+time.Duration(p.BoundarySeconds))*time.Second + 7*time.Minute
}
func (p AdmissionPlan) valid() bool {
	return p.SurvivorSeconds > 0 && p.SurvivorSeconds <= 1200 && p.BoundarySeconds > 0 && p.BoundarySeconds <= 1200 && p.minimum() <= GateMaximum
}

// resourceCallers routes the existing two-phase launcher interface to separate
// request reservations. No launch, stop or unknown outcome can be shared.
type resourceCallers []*ResourcePorts

func (p resourceCallers) Ready(ctx context.Context, end time.Time) operator.Certainty {
	for _, port := range p {
		if c := port.Ready(ctx, end); c != operator.Succeeded {
			return c
		}
	}
	return operator.Succeeded
}
func (p resourceCallers) Start(ctx context.Context, id operatorcaller.Identity, phase operatorcaller.Phase, end time.Time) operatorcaller.StartResult {
	for _, port := range p {
		if port.Manifest.Identity == id {
			return port.Start(ctx, id, phase, end)
		}
	}
	return operatorcaller.StartResult{State: operatorcaller.NotStarted, Certainty: operator.Failed}
}
func (p resourceCallers) Stop(ctx context.Context, id operatorcaller.Identity, end time.Time) operatorcaller.StopResult {
	for _, port := range p {
		if port.Manifest.Identity == id {
			return port.Stop(ctx, id, end)
		}
	}
	return operatorcaller.StopResult{Certainty: operator.Failed}
}
func (p resourceCallers) stopAll(ctx context.Context, end time.Time) bool {
	stopped := true
	for _, port := range p {
		if port.NeverStarted() {
			continue
		}
		r := port.Stop(ctx, port.Manifest.Identity, end)
		stopped = stopped && r.Inactive && r.Certainty == operator.Succeeded
	}
	return stopped
}
func (r *ProcessRuntime) measurePorts(caller *CallerRuntime, calls int) MeasurePorts {
	ports := MeasurePorts{ProcedurePorts: ProcedurePorts{Clock: r.Session.clock, Sleep: resourcePause, Step: r.Step, Inventory: r.Inventory, Broker: caller}, Calls: calls, Store: r.Session.OptionalStore(r.Peer), Observe: r.Observe, BeforeFault: func(ctx context.Context, end time.Duration) error {
		if err := r.Inventory(ctx, "from", end); err != nil {
			return err
		}
		return r.VerifyClocks(ctx, end)
	}, MarkFault: caller.MarkFault, Collect: caller.Collect}
	if r.commandPorts != nil {
		ports.ProcedurePorts = *r.commandPorts
		ports.Broker = caller
	}
	return ports
}
func runResourceAdmission(ctx context.Context, s *Session, runtime *ProcessRuntime, caller *CallerRuntime, boundary *operatorcaller.Broker, launchers resourceCallers, end time.Duration, token string, result *ResourceResult) ProcedureResult {
	wall := func(end time.Duration) time.Time { return s.clock.Wall().Add(max(0, end-s.clock.Elapsed())) }
	ports := runtime.measurePorts(caller, 1)
	admitted := RunAdmission(ctx, AdmissionPorts{
		Survivor: func(ctx context.Context, end time.Duration) MeasureResult {
			return RunMeasureGate(ctx, operator.Load, runtime.Config, ports, end)
		},
		StopSurvivor: func(ctx context.Context, end time.Duration) bool {
			stop := launchers.Stop(ctx, caller.Broker.Manifest.Identity, wall(end))
			return stop.Inactive && stop.Certainty == operator.Succeeded
		},
		Idle: func(ctx context.Context, end time.Duration) error {
			return WaitBoundaryIdle(ctx, ports.ProcedurePorts, end)
		},
		Boundary: func(ctx context.Context, end time.Duration) MeasureResult {
			if err := operatorcaller.BeginBoundary(ctx, launchers, caller.Broker, boundary, token, wall(end)); err != nil {
				return MeasureResult{Status: operator.Inconclusive, Reason: caller.callerError(err).Error()}
			}
			boundaryCaller := NewCallerRuntime(boundary, s.clock, end, nil, caller.Phase)
			boundaryPorts := runtime.measurePorts(boundaryCaller, 2)
			// Boundary callers have no trial population. Retain their raw evidence
			// independently and preserve known safety failures without pooling them or
			// introducing a new passing media/timing policy.
			boundaryPorts.Collect = func(ctx context.Context, end time.Duration) <-chan MeasurementCollection {
				done := make(chan MeasurementCollection, 1)
				go func() {
					raw, err := boundary.Collect(ctx, minTime(wall(end), time.UnixMilli(boundary.Manifest.Deadline)))
					result.BoundaryJSONL = raw
					media, decodeErr := admissionMedia(raw, boundary)
					done <- MeasurementCollection{Evidence: MeasurementEvidence{Media: media}, Err: errors.Join(err, decodeErr)}
					close(done)
				}()
				return done
			}
			return RunMeasureGate(ctx, operator.Load, runtime.Config, boundaryPorts, end)
		},
	}, end)
	result.Admission = &admitted
	return ProcedureResult{Status: admitted.Status, Reason: admitted.Reason, Media: admitted.Survivor.Media}
}
func admissionMedia(raw string, b *operatorcaller.Broker) (*MediaSummary, error) {
	summary := &MediaSummary{Status: operator.Inconclusive, ExpectedCalls: b.Manifest.Plan.Calls}
	seen := map[string]bool{}
	var err error
	for _, line := range strings.Split(raw, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var m loadgen.Message
		if e := observe.DecodeEvidenceLimit([]byte(line), &m, operator.MaxBlobBytes); e != nil {
			err = errors.Join(err, e)
			if !observe.IsIncompleteEvidence(e) {
				continue
			}
		}
		if m.Type != "call" {
			continue
		}
		if b.Registration == nil || m.PID != b.CallerPID || seen[m.Session] || !slices.Contains(b.Registration.Sessions, m.Session) || m.Call == nil {
			err = errors.Join(err, fault("precondition-failed:caller-evidence"))
			continue
		}
		seen[m.Session] = true
		if m.Call.DecryptionFailures.AuthTag > 0 || m.Call.DecryptionFailures.Replay > 0 || m.Call.DecryptionFailures.Other > 0 || m.Call.ICERestarts > 0 {
			summary.Status = operator.Failure
		}
		for _, move := range m.Call.Moves {
			if move.Lost || move.Measurement.Failed {
				summary.Status = operator.Failure
			}
		}
		if e := observe.Require([]byte(line), observe.Contract{"call.DecryptionFailures.AuthTag", "call.DecryptionFailures.Replay", "call.DecryptionFailures.Other", "call.ICERestarts", "call.HungUpAt"}); e != nil {
			err = errors.Join(err, e)
		}
		if m.Call.HungUpAt > 0 {
			summary.CompletedCalls++
		}
	}
	if summary.CompletedCalls != summary.ExpectedCalls {
		err = errors.Join(err, fault("precondition-failed:caller-evidence"))
	}
	return summary, err
}
