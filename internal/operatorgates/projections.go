package operatorgates

import (
	"encoding/json"

	"github.com/relaishq/relais/internal/operatorgates/observe"
	"github.com/relaishq/relais/pkg/operator"
)

// DecodeProcessEvidence publishes only the fixed numeric fields and the three
// declared inconclusive relay projections. Arbitrary report keys are discarded.
func DecodeProcessEvidence(data json.RawMessage) (e ProcessEvidence, err error) {
	err = observe.Decode(data, &e, observe.Contract{"completedSteps", "workerRollouts", "restarts"})
	if err != nil {
		return e, err
	}
	if e.CompletedSteps < 0 || e.WorkerRollouts < 0 || e.Restarts < 0 {
		return e, fault("invalid-process-measurement")
	}
	for _, v := range []*int64{e.ControlReadyMS, e.ControlReadyNormalMS, e.ControlReadyDegradedMS} {
		if v != nil && (*v < 0 || *v > maxSafeEpoch) {
			return e, fault("invalid-process-measurement")
		}
	}
	for _, v := range []*int{e.ReleaseTargetsVerified, e.ControlReadyTargetMet} {
		if v != nil && (*v < 0 || uint64(*v) > maxSafeEpoch) {
			return e, fault("invalid-process-measurement")
		}
	}
	for _, v := range []*uint64{e.RelayEpochDelta, e.RoutesRestored, e.DuplicatePackets} {
		if v != nil && *v > maxSafeEpoch {
			return e, fault("invalid-process-measurement")
		}
	}
	for _, v := range []string{e.RelayWindows, e.RelayLocalTiming, e.RelayConnectionInvariants} {
		if v != "" && v != "inconclusive" {
			return e, fault("unsupported-relay-criterion")
		}
	}
	return e, nil
}
func Stopped(confirmed bool, r ProcedureResult) ProcedureResult {
	if confirmed {
		return r
	}
	if r.Status != operator.Failure {
		r.Status = operator.Inconclusive
	}
	r.AwaitingPool = false
	if r.Reason != "gate-deadline" {
		r.Reason = "command-result-unknown"
	}
	return r
}
