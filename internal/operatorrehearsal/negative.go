package operatorrehearsal

import (
	"context"
	"errors"
	"fmt"
	"strconv"
)

type negativeControl struct {
	slot, name, unit, path, credential, expected string
}

func negativeScript(c negativeControl) (string, error) {
	script, err := assets.ReadFile("assets/negative-control.py")
	if err != nil {
		return "", err
	}
	return "python3 - <<'PY'\ncontrol = " + strconv.Quote(c.name) + "\nunit = " + strconv.Quote(c.unit) + "\ncontrol_path = " + strconv.Quote(c.path) + "\ncredential = " + strconv.Quote(c.credential) + "\nexpected = " + strconv.Quote(c.expected) + "\nclock_check = " + strconv.Quote(ClockCheck()) + "\n" + string(script) + "PY\n", nil
}

// NegativeControls must run on a disposable booted deployment before any gate.
// Each control demands a specific observed failure and verifies its restoration.
// Failure stops setup; no fallback can qualify a gate.
func (d *Deployment) NegativeControls(ctx context.Context, afterRestore func(context.Context, string) error) error {
	if !d.aclNegative {
		return errors.New("wrong-acl-control-not-run")
	}
	if afterRestore == nil {
		return errors.New("negative-control-verification-failed")
	}
	verify := func(control string) error {
		if err := afterRestore(ctx, control); err != nil {
			return fmt.Errorf("negative-control-verification-failed: %w", err)
		}
		for _, slot := range Slots {
			script, err := d.Topology.VerifyScript(slot)
			if err != nil {
				return err
			}
			if err = d.shell(ctx, slot, "verify-after-controls-"+slot, script); err != nil {
				return errors.New("negative-control-verification-failed")
			}
		}
		d.controlNote("control=" + control + " restore_health_verified=true")
		return nil
	}
	// The ACL control runs before any host exists, while setup's store admin
	// still exists. Finish its deployment health proof once all hosts are booted.
	if err := verify("wrong-acl"); err != nil {
		return err
	}
	for _, row := range []negativeControl{
		{"shared", "missing-credential", "relais-signal.service", "/run/relais/signal.token", "/run/relais/signal.token", "failed-with-EXIT_CREDENTIALS-243-and-no-process"},
		{"w1", "missing-unit", "relais-worker.service", "/etc/systemd/system/relais-worker.service", "none", "nonzero-start-and-LoadState-not-found"},
		{"shared", "wrong-time-source", "chronyd", "/etc/chrony.conf", "none", "clock-check-exit-42-wrong-time-source"},
	} {
		d.controlNote(fmt.Sprintf("control=%s host=%s unit=%s credential=%s path=%s expected=%s stage=dispatch", row.name, row.slot, row.unit, row.credential, row.path, row.expected))
		script, err := negativeScript(row)
		if err != nil {
			return err
		}
		args, err := d.execArgs(row.slot, []string{"-i"}, "sh", "-eu")
		if err != nil {
			return err
		}
		result, err := d.command(ctx, "negative-"+row.name, args, []byte(script))
		if err != nil {
			if result.Code != nil && result.Err == nil && !result.Truncated {
				switch *result.Code {
				case 70:
					return errors.New("negative-control-setup-failed")
				case 71:
					return errors.New("negative-control-refusal-failed")
				case 72:
					return errors.New("negative-control-restore-failed")
				}
			}
			return errors.New("negative-control-command-unconfirmed")
		}
		if err = verify(row.name); err != nil {
			return err
		}
	}
	return nil
}

func (d *Deployment) controlNote(message string) {
	d.diagnosticMu.Lock()
	defer d.diagnosticMu.Unlock()
	if d.Diagnostics != nil {
		_, _ = fmt.Fprintln(d.Diagnostics, d.Topology.Redact(message))
	}
}
