package operatorgates

import (
	"context"
	"encoding/json"
	"time"

	"github.com/relaishq/relais/internal/operatorcaller"
	"github.com/relaishq/relais/internal/operatorevidence"
	"github.com/relaishq/relais/internal/operatorevidence/relayjudge"
)

func runResourceRelayLoss(ctx context.Context, s *Session, runtime *ProcessRuntime, caller *CallerRuntime, p ProcedurePorts, end time.Duration, binary string) ProcedureResult {
	b := caller.Broker
	input := relayjudge.Input{Version: 1, RunID: b.Manifest.RunID, Scenario: s.scenario, PID: b.CallerPID}
	if b.Registration != nil {
		input.Sessions = append([]string{}, b.Registration.Sessions...)
	}
	judge := operatorevidence.OpenRelayLossJudge(s.scenario.Judge.SourceCommit, s.scenario.Judge.SHA256, binary)
	collect := func(ctx context.Context, end time.Duration) (string, []json.RawMessage, error) {
		collectCtx, cancel := context.WithTimeout(ctx, max(0, end-caller.Clock.Elapsed()))
		defer cancel()
		ctx = collectCtx
		deadline := caller.Clock.Wall().Add(max(0, end-caller.Clock.Elapsed()))
		if _, err := b.Feed(ctx, "relay-loss-finish", operatorcaller.Payload{Kind: "relay-loss-finish"}, deadline); err != nil {
			return "", nil, caller.callerError(err)
		}
		raw, err := b.Collect(ctx, deadline)
		if err != nil || b.Fence.Latched() {
			return raw, nil, fault("command-result-unknown")
		}
		bundle, readErr := b.Mailbox.Read(ctx, b.Manifest.Job()+"/relay-loss-stats.json")
		var samples []json.RawMessage
		if readErr != nil || !bundle.Found || json.Unmarshal(bundle.Data, &samples) != nil {
			return raw, nil, fault("relay-loss-missing")
		}
		return raw, samples, nil
	}
	runtime.commandPorts = &p
	defer func() { runtime.commandPorts = nil }()
	return RunRelayLossGate(ctx, runtime.Config, RelayLossPorts{ProcedurePorts: p, Input: input, Collect: collect, Judge: judge.Judge}, end)
}
