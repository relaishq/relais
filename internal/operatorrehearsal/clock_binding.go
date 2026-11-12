package operatorrehearsal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/relaishq/relais/internal/operatorcaller"
	"github.com/relaishq/relais/internal/operatorgates"
	"github.com/relaishq/relais/pkg/operator"
)

func (r *rehearsal) leaseProbeFiles() map[string]string {
	files := map[string]string{}
	for slot, h := range r.topology.Hosts {
		var node struct {
			Components []string `json:"components"`
		}
		if json.Unmarshal([]byte(h.Metadata.Files["/etc/relais/node.json"].Content), &node) == nil && slices.Contains(node.Components, "signal") {
			files[slot] = clockProbePath
		}
	}
	return files
}
func (r *rehearsal) clockConfig(judge string) operatorgates.ResourceConfig {
	return operatorgates.ResourceConfig{ClockPolicy: rehearsalClockPolicy(), ClockLeapMode: "unsmeared", Parameters: r.parameters(), Pins: r.pins, Bounds: r.report.Bounds, Architecture: "x86_64", Token: r.topology.Secrets["SignalToken"], JudgeBinary: judge, Endpoints: operatorcaller.Endpoints{RunID: r.run, Signaling: SignalingURL + "/calls", Relay: r.topology.Hosts["relay"].IP + ":24780", FrontendControlBoundNS: r.report.Bounds.FrontendControlBoundNS, FrontendWorkerBoundNS: r.report.Bounds.FrontendWorkerBoundNS, DriftPPM: int(r.report.Bounds.DriftPPM)}}
}
func (r *rehearsal) bindAdmission(ctx context.Context, scenario operator.Scenario) error {
	clean := func() bool { return r.session != nil && !r.session.Latched() && len(r.session.Uncertainties()) == 0 }
	if !clean() {
		return errors.New("command-result-unknown")
	}
	binding, selection, err := BindClockAdmission(r.c.Output, r.report.ClockSelection, scenario)
	if err != nil {
		return err
	}
	init := r.boundInit
	init.Scenario, init.ClockBinding = binding.Scenario, &binding.ClockBinding
	r.report.Bounds = selection.Receipt.Bounds
	adapter := &Adapter{Topology: r.topology, Containers: r.deployment.ContainerIDs(), Runner: r.runner, Diagnostics: r.diagnostics, LeaseProbeFiles: r.leaseProbeFiles()}
	init.Capabilities = adapter.Capabilities()
	session, err := operatorgates.NewSession(r.run, init, r.clock)
	if err != nil {
		return err
	}
	peer := NewLocalPeer(adapter, r.run, init)
	if err = session.BindClock(ctx, peer, r.clockConfig(r.judge.pinned.Binary), min(session.Budget.PoolEnd(), r.clock.Elapsed()+2*time.Minute)); err != nil {
		_ = peer.Close()
		return err
	}
	guard, err := session.BoundLeaseGuard(peer, pause)
	if err != nil {
		_ = peer.Close()
		return err
	}
	if !clean() {
		_ = peer.Close()
		return errors.New("command-result-unknown")
	}
	_ = r.peer.Close()
	r.peer = peer
	r.session = session
	r.runtime.Session = session
	r.runtime.Peer = peer
	r.runtime.Leases = guard
	r.boundInit = init
	r.report.ScenarioSHA256 = init.Scenario.Digest
	session.SetObservationFailureSink(r.preconditionDiagnostic)
	session.SetUncertaintySink(func(u operator.Uncertainty) {
		raw, _ := json.Marshal(u)
		_, _ = fmt.Fprintf(r.diagnostics, "command-result-unknown %s\n", raw)
	})
	r.judge.note = session.RecordUncertainty
	return nil
}
