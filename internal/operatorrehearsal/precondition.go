package operatorrehearsal

import (
	"context"
	"encoding/json"
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
	return r.verifyIdleAt(ctx, end, "calibration")
}

func (r *rehearsal) verifyIdleAt(ctx context.Context, end time.Duration, stage string) error {
	if err := r.runtime.Inventory(ctx, "from", end); err != nil {
		return err
	}
	fail := func(slot, observation, category string) error {
		err := &operatorgates.ProcedureError{Code: "precondition-failed:" + category}
		r.preconditionDiagnostic(slot, observation, err)
		return err
	}
	s, err := r.controlStatus(ctx, stage, end)
	if err != nil {
		return err
	}
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
	_, _ = fmt.Fprintf(r.diagnostics, "deployment-idle-verified stage=%s calls=0 reservations=0 workers=2 localSessions=0\n", stage)
	return nil
}

// Keep only logical worker names, flags and counts, never addresses or call IDs.
func (r *rehearsal) controlStatus(ctx context.Context, stage string, end time.Duration) (controlplane.Status, error) {
	step, err := r.runtime.Step(ctx, "status", "shared", end, operatorgates.ProcessExtra{})
	if err != nil {
		return step.Status, err
	}
	type worker struct {
		Name       string `json:"name"`
		Dead       bool   `json:"dead"`
		Recovering bool   `json:"recovering"`
		Draining   bool   `json:"draining"`
		Calls      int    `json:"calls"`
	}
	s := step.Status
	view := struct {
		Ready        bool     `json:"ready"`
		Calls        int      `json:"calls"`
		Reservations int      `json:"reservations"`
		Workers      []worker `json:"workers"`
	}{Ready: s.Ready, Calls: len(s.Calls), Reservations: s.Pool.Reservations, Workers: []worker{}}
	for _, name := range []string{"w1", "w2"} {
		for _, w := range s.Workers {
			if w.Name == name {
				view.Workers = append(view.Workers, worker{name, w.Dead, w.Recovering, w.Draining, w.Calls})
			}
		}
	}
	raw, _ := json.Marshal(view)
	_, _ = fmt.Fprintf(r.diagnostics, "control-status stage=%s %s\n", stage, raw)
	return s, nil
}

func (r *rehearsal) restoreControl(ctx context.Context, control string) error {
	end := r.clock.Elapsed() + 5*time.Minute
	if _, err := r.controlStatus(ctx, control+"-restore", end); err != nil {
		return err
	}
	if control == "missing-unit" {
		// SIGTERM marks draining in control. Restoring the file and process is
		// followed by P3's empty/stop/replace/start repair, not a flag reset.
		if err := operatorgates.GatePrecondition(ctx, r.process, r.ports(), end); err != nil {
			return err
		}
	}
	return r.verifyIdleAt(ctx, end, control+"-verified")
}
