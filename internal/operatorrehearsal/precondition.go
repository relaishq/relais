package operatorrehearsal

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/relaishq/relais/internal/operatorgates"
	"github.com/relaishq/relais/internal/operatorgates/inventory"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/operator"
)

func (r *rehearsal) preconditionDiagnostic(slot, observation string, err error) {
	category := publicCode(err.Error())
	var unavailable *inventory.Failure
	if errors.As(err, &unavailable) {
		category = publicCode("precondition-failed:" + unavailable.Slot + "-" + unavailable.Code)
	}
	if category == "unclassified" {
		category = "precondition-failed:observation"
	}
	if !slices.Contains(Slots, slot) {
		slot = "unavailable"
	}
	observation = (operator.Uncertainty{Action: observation}).Public().Action
	_, _ = fmt.Fprintf(r.diagnostics, "precondition-check host=%s observation=%s category=%s\n", slot, observation, category)
}

// Verify cleanup with fresh product observations before any gate caller exists.
// Inactive systemd alone cannot prove that control and workers released a call.
func (r *rehearsal) verifyIdle(ctx context.Context, end time.Duration) error {
	if err := r.runtime.Inventory(ctx, "from", end); err != nil {
		return err
	}
	fail := func(slot, observation, category string) error {
		err := &operatorgates.ProcedureError{Code: "precondition-failed:" + category}
		r.preconditionDiagnostic(slot, observation, err)
		return err
	}
	step, err := r.runtime.Step(ctx, "status", "shared", end, operatorgates.ProcessExtra{})
	if err != nil {
		return err
	}
	s := step.Status
	if !s.Ready || s.LostCount != 0 {
		return fail("shared", "status", "control-not-settled")
	}
	if len(s.Calls) != 0 || s.Pool.Reservations != 0 {
		return fail("shared", "status", "control-not-empty")
	}
	for _, name := range []string{"w1", "w2"} {
		i := slices.IndexFunc(s.Workers, func(w controlplane.WorkerStatus) bool { return w.Name == name })
		if i < 0 || s.Workers[i].Dead || s.Workers[i].Recovering || s.Workers[i].LivenessUnknown || s.Workers[i].Address != r.process.Parameters.Workers[name].Media {
			return fail(name, "status", name+"-not-alive")
		}
		if s.Workers[i].Draining {
			return fail(name, "status", name+"-draining")
		}
		if s.Workers[i].Calls != 0 {
			return fail(name, "status", name+"-not-empty")
		}
		local, err := r.runtime.Step(ctx, "worker-status", "shared", end, operatorgates.ProcessExtra{Worker: name})
		if err != nil {
			return err
		}
		if !local.Local.InventoryKnown || local.Local.Truncated {
			return fail(name, "worker-status", name+"-inventory-unknown")
		}
		if local.Local.Sessions != 0 || len(local.Local.Inventory) != 0 {
			return fail(name, "worker-status", name+"-not-empty")
		}
	}
	_, _ = fmt.Fprintln(r.diagnostics, "calibration-idle-verified calls=0 reservations=0 workers=2 localSessions=0")
	return nil
}
