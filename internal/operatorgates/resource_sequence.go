package operatorgates

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/relaishq/relais/internal/operatorcaller"
	"github.com/relaishq/relais/internal/operatorevidence"
	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/relaishq/relais/internal/operatorgates/inventory"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/operator"
)

// ResourceConfig is private local configuration, never a protocol artifact.
// Resource names and launch paths stay with the resource server. The judge is
// the hash-verified local public executable, not a peer-supplied verdict.
type ResourceConfig struct {
	Parameters   ProcessParameters        `json:"parameters"`
	Pins         []inventory.Pin          `json:"pins"`
	Bounds       clocks.Bounds            `json:"bounds"`
	Architecture string                   `json:"architecture"`
	Endpoints    operatorcaller.Endpoints `json:"endpoints"`
	Token        string                   `json:"token"`
	CallerIDBase uint64                   `json:"callerIdBase,omitempty"`
	JudgeBinary  string                   `json:"judgeBinary"`
	Admission    *AdmissionPlan           `json:"admission,omitempty"`
}
type ResourceResult struct {
	SequenceResult
	CallerStopped map[operator.Gate]bool
	Clocks        []clocks.Evidence
	PoolError     bool
	Admission     *AdmissionResult
	BoundaryJSONL string
}
type ResourceRunner struct {
	Config ResourceConfig
	// These hooks retain local diagnostics and evidence for the rehearsal. They
	// never supply resource ports or choose gate outcomes.
	Judge    MediaJudge
	Started  func(*Session)
	Finished func(*Session, ResourceResult)
}

func (r ResourceRunner) validate(s *Session) error {
	c := r.Config
	if ValidateProcessParameters(c.Parameters) != nil || inventory.ValidatePins(c.Pins, s.scenario) != nil {
		return errors.New("invalid-resource-config")
	}
	if c.Architecture != "x86_64" && c.Architecture != "arm64" {
		return errors.New("invalid-resource-config")
	}
	if c.CallerIDBase > 1000000 || c.Endpoints.RunID == "" || c.Token == "" || !filepath.IsAbs(c.JudgeBinary) {
		return errors.New("invalid-resource-config")
	}
	data, err := os.ReadFile(c.JudgeBinary)
	sum := sha256.Sum256(data)
	if err != nil || hex.EncodeToString(sum[:]) != s.scenario.Judge.SHA256 || s.scenario.Judge.SourceCommit != s.scenario.Caller.SourceCommit || s.scenario.Judge.SourceCommit != c.Pins[0].Commit {
		return errors.New("invalid-resource-config")
	}
	return nil
}
func CampaignDuration(g operator.Gate) time.Duration {
	if g == operator.RelayDeploy {
		return 600 * time.Second
	}
	return 1020 * time.Second
}

const callerCollectionReserve = time.Minute
const callerStopReserve = time.Minute

func CampaignMinimum(g operator.Gate) time.Duration {
	return CampaignDuration(g) + time.Minute + callerCollectionReserve + callerStopReserve
}
func CallerWindow(g operator.Gate, now, gateEnd time.Duration) (time.Duration, bool) {
	end := min(now+GateMaximum, gateEnd)
	return end, end-now >= CampaignMinimum(g)
}
func resourcePause(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(max(0, d))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
func (r ResourceRunner) Run(ctx context.Context, s *Session, peer Peer) []operator.GateResult {
	if r.Started != nil {
		r.Started(s)
	}
	result := ResourceResult{CallerStopped: map[operator.Gate]bool{}}
	if r.validate(s) != nil {
		for _, gate := range s.gates {
			result.Gates = append(result.Gates, operator.GateResult{Gate: gate, Status: operator.NotRun, Reason: "precondition-failed"})
			result.Procedures = append(result.Procedures, ProcedureResult{Status: operator.NotRun, Reason: "precondition-failed"})
		}
		s.retainGateDiagnostics(&result.SequenceResult, result.CallerStopped)
		if r.Finished != nil {
			r.Finished(s, result)
		}
		return result.Gates
	}
	c := r.Config
	judge := r.Judge
	if judge == nil {
		judge = operatorevidence.PinnedJudge{Commit: s.scenario.Judge.SourceCommit, SHA256: s.scenario.Judge.SHA256, Binary: c.JudgeBinary}
	}
	process := &ProcessConfig{Parameters: c.Parameters, ArmedAt: time.UnixMilli(s.Budget.armed), Trials: true, Pool: true}
	runtime := &ProcessRuntime{Session: s, Peer: peer, Config: process, Architecture: c.Architecture, Pins: c.Pins, Bounds: &c.Bounds}
	pool := &EvidenceRuntime{RunID: c.Endpoints.RunID, Judge: judge}
	var launchers resourceCallers
	var boundary *operatorcaller.Broker
	var caller *CallerRuntime
	var current operator.Gate
	next := max(uint64(1), c.CallerIDBase)
	wall := func(end time.Duration) time.Time { return s.clock.Wall().Add(max(0, end-s.clock.Elapsed())) }
	procedures := func() ProcedurePorts {
		return ProcedurePorts{Clock: s.clock, Sleep: resourcePause, Step: runtime.Step, Inventory: runtime.Inventory, Broker: caller}
	}
	makeCaller := func(gate operator.Gate, phase operatorcaller.Phase, seconds int, callerEnd time.Duration) (*ResourcePorts, *operatorcaller.Broker, error) {
		identity := operatorcaller.Identity{RunID: c.Endpoints.RunID, Gate: gate, Phase: phase, CallerID: strconv.FormatUint(next, 10), Attempt: "1", PublicSHA: s.scenario.Caller.SourceCommit}
		next++
		manifest := operatorcaller.Manifest{Version: 1, Identity: identity, ArmedAt: s.Budget.armed, CreatedAt: s.clock.Wall().UnixMilli(), Deadline: wall(callerEnd).UnixMilli(), Plan: operatorcaller.NewPlan(phase, seconds, identity.PublicSHA), Endpoints: c.Endpoints}
		port := &ResourcePorts{Session: s, Peer: peer, End: callerEnd, Manifest: manifest}
		broker, err := operatorcaller.NewBroker(manifest, operatorcaller.NewMailbox(port), operatorcaller.SystemClock{}, s)
		return port, broker, err
	}
	stop := func(ctx context.Context, end time.Duration) bool { return launchers.stopAll(ctx, wall(end)) }

	result.SequenceResult = s.RunSequence(ctx, SequencePorts{
		Minimum: func(gate operator.Gate) time.Duration {
			if gate == operator.Load && c.Admission != nil && c.Admission.valid() {
				return c.Admission.minimum()
			}
			return CampaignMinimum(gate)
		},
		Prepare: func(ctx context.Context, gate operator.Gate, end, gateEnd time.Duration) error {
			current = gate
			// Absence is a definite refusal before any submission, not a fabricated
			// operation or an unknown effect. No partial gate is started.
			required := []operator.Operation{operator.HostExecute, operator.CallerReady, operator.CallerStart, operator.CallerStop, operator.MailboxRead, operator.MailboxCreate}
			if gate == operator.Failover {
				required = append(required, operator.StoreObserve, operator.StoreFailover)
			}
			if gate == operator.Load {
				required = append(required, operator.CallerAdmission)
			}
			for _, op := range required {
				if !s.capabilities[op] {
					return fault("precondition-failed:unsupported-capability")
				}
			}
			seconds := int(CampaignDuration(gate) / time.Second)
			if gate == operator.Load {
				if c.Admission == nil || !c.Admission.valid() {
					return fault("precondition-failed")
				}
				seconds = c.Admission.SurvivorSeconds
			}
			process.Target = ""
			process.Operation = ""
			process.Mixed = false
			runtime.BeforeMutation = nil
			if err := GatePrecondition(ctx, process, procedures(), end); err != nil {
				return err
			}
			now := s.clock.Elapsed()
			callerEnd, fits := CallerWindow(gate, now, gateEnd)
			if gate == operator.Load {
				callerEnd = min(now+GateMaximum, gateEnd)
				fits = callerEnd-now >= c.Admission.minimum()
			}
			if !fits {
				return fault("gate-deadline")
			}
			port, broker, err := makeCaller(gate, operatorcaller.Survivor, seconds, callerEnd)
			if err != nil {
				return fault("precondition-failed:caller")
			}
			launchers = resourceCallers{port}
			boundary = nil
			if gate == operator.Load {
				var boundaryPort *ResourcePorts
				boundaryPort, boundary, err = makeCaller(gate, operatorcaller.AdmissionBoundary, c.Admission.BoundarySeconds, callerEnd)
				if err != nil {
					return fault("precondition-failed:caller")
				}
				launchers = append(launchers, boundaryPort)
			}
			if err = operatorcaller.PrepareCallers(ctx, launchers, broker, boundary, c.Token, wall(end), func(context.Context) error { return nil }); err != nil {
				if !s.Latched() {
					if err.Error() == "caller-not-near" {
						return fault("caller-not-near")
					}
					if port.NeverStarted() {
						return fault("precondition-failed:caller")
					}
				}
				return err
			}
			observed, err := s.FreshInventory(ctx, peer, c.Pins, end, c.Pins[0].Commit, true)
			if err != nil {
				return runtime.preflightFailure(err, "inventory")
			}
			if err = runtime.VerifyClocks(ctx, end); err != nil {
				return err
			}
			windowRequired := CampaignDuration(gate) + callerCollectionReserve + callerStopReserve
			if gate == operator.Load {
				windowRequired = time.Duration(c.Admission.SurvivorSeconds+c.Admission.BoundarySeconds)*time.Second + callerCollectionReserve + callerStopReserve
			}
			if callerEnd-s.clock.Elapsed() < windowRequired {
				return fault("gate-deadline")
			}
			phase := operatorevidence.Phase{DeployedSHA: c.Pins[0].Commit, Clocks: runtime.ClockSamples[len(runtime.ClockSamples)-1], Inventory: observed}
			caller = NewCallerRuntime(broker, s.clock, callerEnd, pool, phase)
			runtime.BeforeMutation = caller.MarkFault
			return nil
		},
		ReleaseCallers: func(ctx context.Context, _ string, end time.Duration) bool {
			result.CallerStopped[current] = stop(ctx, end)
			return result.CallerStopped[current]
		},
		Command: func(ctx context.Context, gate operator.Gate, end time.Duration) ProcedureResult {
			p := procedures()
			p.Media = caller.Media
			p.Duplicates = caller.Duplicates
			return runResourceCommand(ctx, s.clock, caller, end, p, func(ctx context.Context, p ProcedurePorts, end time.Duration) ProcedureResult {
				runtime.commandPorts = &p
				defer func() { runtime.commandPorts = nil }()
				var out ProcedureResult
				switch gate {
				case operator.UpgradeRollback:
					out = RunReleaseGate(ctx, process, p, end)
				case operator.Load:
					out = runResourceAdmission(ctx, s, runtime, caller, boundary, launchers, end, c.Token, &result)
				case operator.Failover:
					m := RunMeasureGate(ctx, gate, process, runtime.measurePorts(caller, 1), end)
					out = ProcedureResult{Status: m.Status, Reason: m.Reason, Media: m.Media}
				default:
					out = RunProcessGate(ctx, gate, process, p, end)
				}
				return out
			}, stop)
		},
		Finalize: func(ctx context.Context, _ time.Duration) (*MediaSummary, error) {
			summary, err := pool.Media(ctx)
			result.PoolError = err != nil
			return summary, err
		},
	})
	s.retainGateDiagnostics(&result.SequenceResult, result.CallerStopped)
	result.Clocks = runtime.ClockSamples
	if r.Finished != nil {
		r.Finished(s, result)
	}
	return result.Gates
}

// Observe retains the product's own status and independent local inventories.
func (r *ProcessRuntime) Observe(ctx context.Context, end time.Duration) (MeasurementObservation, error) {
	out := MeasurementObservation{At: r.Session.clock.Wall(), Hosts: map[string]HostTelemetry{}, Local: map[string]mediaworker.WorkerStatus{}}
	status, err := r.Step(ctx, "status", "shared", end, ProcessExtra{})
	if err != nil {
		return out, err
	}
	out.Status = &status.Status
	for _, slot := range inventory.Slots {
		row, err := r.Step(ctx, "telemetry", slot, end, ProcessExtra{})
		if err != nil {
			return out, err
		}
		out.Hosts[slot] = row.Telemetry
	}
	for _, worker := range []string{"w1", "w2"} {
		row, err := r.Step(ctx, "worker-status", "shared", end, ProcessExtra{Worker: worker})
		if err != nil {
			return out, err
		}
		out.Local[worker] = row.Local
	}
	return out, nil
}

// runResourceCommand owns the procedure, collection and stop windows together.
func runResourceCommand(ctx context.Context, clock Clock, caller *CallerRuntime, end time.Duration, p ProcedurePorts, run func(context.Context, ProcedurePorts, time.Duration) ProcedureResult, stop func(context.Context, time.Duration) bool) ProcedureResult {
	// Preparation consumed part of this immutable window. Neither gate
	// admission nor restoration may renew the caller manifest's cutoff.
	stopEnd := min(end, caller.End)
	collectionEnd := stopEnd - callerStopReserve
	procedureEnd := collectionEnd - callerCollectionReserve
	caller.End = collectionEnd
	out := ProcedureResult{Status: operator.Inconclusive, Reason: "gate-deadline"}
	if clock.Elapsed() < procedureEnd {
		collectionCtx, cancelCollection := context.WithTimeout(ctx, max(0, collectionEnd-clock.Elapsed()))
		defer cancelCollection()
		p.CollectionEnd = collectionEnd
		p.CollectionContext = collectionCtx
		if step := p.Step; step != nil {
			p.PostMediaStatus = func(context.Context) (controlplane.Status, error) {
				if clock.Elapsed() >= collectionEnd || collectionCtx.Err() != nil {
					return controlplane.Status{}, fault("gate-deadline")
				}
				row, err := step(collectionCtx, "status", "shared", collectionEnd, ProcessExtra{})
				if err == nil && row.Status.LostCount > 0 {
					err = fault("known-call-loss")
				}
				return row.Status, err
			}
		}
		if media := p.Media; media != nil {
			p.Media = func(context.Context) (*MediaSummary, error) {
				if clock.Elapsed() >= collectionEnd || collectionCtx.Err() != nil {
					return nil, fault("gate-deadline")
				}
				return media(collectionCtx)
			}
		}
		procedureCtx, cancelProcedure := context.WithTimeout(ctx, procedureEnd-clock.Elapsed())
		out = run(procedureCtx, p, procedureEnd)
		cancelProcedure()
	}
	if clock.Elapsed() >= stopEnd {
		if out.Status != operator.Failure {
			out.Status = operator.Inconclusive
			out.Reason = "gate-deadline"
			out.AwaitingPool = false
		}
		return out
	}
	return Stopped(stop(ctx, min(clock.Elapsed()+callerStopReserve, stopEnd)), out)
}
