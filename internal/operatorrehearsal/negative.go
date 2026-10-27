package operatorrehearsal

import (
	"context"
	"errors"
)

// NegativeControls must run on a disposable booted deployment before any gate.
// Each control demands a specific observed failure, then checks the original
// template state again. Failure stops setup; no fallback can qualify a gate.
func (d *Deployment) NegativeControls(ctx context.Context) error {
	if !d.aclNegative {
		return errors.New("wrong-acl-control-not-run")
	}
	// The real signal unit must refuse a missing LoadCredential source.
	missingCredential := `systemctl stop relais-signal.service
mv /run/relais/signal.token /run/relais/signal.token.saved
trap 'mv /run/relais/signal.token.saved /run/relais/signal.token; systemctl reset-failed relais-signal.service; systemctl start relais-signal.service' EXIT
if systemctl start relais-signal.service; then exit 1; fi
systemctl is-failed --quiet relais-signal.service
`
	if err := d.shell(ctx, "shared", "negative-missing-credential", missingCredential); err != nil {
		return err
	}
	// The real unit name must fail to load, rather than treating absence as idle.
	missingUnit := `systemctl stop relais-worker.service
mv /etc/systemd/system/relais-worker.service /etc/relais/relais-worker.service.saved
trap 'mv /etc/relais/relais-worker.service.saved /etc/systemd/system/relais-worker.service; systemctl daemon-reload; systemctl reset-failed relais-worker.service; systemctl start relais-worker.service' EXIT
systemctl daemon-reload
if systemctl start relais-worker.service; then exit 1; fi
test "$(systemctl show -p LoadState --value relais-worker.service)" = not-found
`
	if err := d.shell(ctx, "w1", "negative-missing-unit", missingUnit); err != nil {
		return err
	}
	// Swap the configured source, restart the running daemon, and prove the same
	// fast source check used by setup rejects it before the long sync wait.
	wrongSource := `cp /etc/chrony.conf /etc/chrony.conf.saved
trap 'mv /etc/chrony.conf.saved /etc/chrony.conf; systemctl restart chronyd; chronyc -n waitsync 240 0 20 1' EXIT
printf 'server 192.0.2.123 iburst\ndriftfile /var/lib/chrony/drift\n' > /etc/chrony.conf
systemctl restart chronyd
chronyc -n tracking >/dev/null
set +e
(
` + ClockCheck() + `)
status=$?
set -e
test "$status" -eq 42
`
	if err := d.shell(ctx, "shared", "negative-wrong-time-source", wrongSource); err != nil {
		return err
	}
	for _, slot := range Slots {
		script, x := d.Topology.VerifyScript(slot)
		if x != nil {
			return x
		}
		if x = d.shell(ctx, slot, "verify-after-controls-"+slot, script); x != nil {
			return x
		}
	}
	return nil
}
