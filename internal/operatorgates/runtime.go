package operatorgates

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/relaishq/relais/internal/operatorgates/host"
	"github.com/relaishq/relais/internal/operatorgates/inventory"
	"github.com/relaishq/relais/internal/operatorgates/observe"
	"github.com/relaishq/relais/internal/releasecompat"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/operator"
	"github.com/relaishq/relais/pkg/relay"
)

// ProcessRuntime renders and classifies the fixed host procedures. Ordinary
// mutations receive inventory and clock admission before the fault hook;
// planned ordinary faults also wait out all host reads. Restoration bypasses
// clock admission and the gap, retaining post-rollback coverage in place.
type ProcessRuntime struct {
	commandPorts   *ProcedurePorts
	Session        *Session
	Peer           Peer
	Config         *ProcessConfig
	Architecture   string
	Pins           []inventory.Pin
	Leases         *LeaseGuard
	ClockObserved  func(clocks.Evidence)
	ClockSamples   []clocks.Evidence
	BeforeMutation func(context.Context) error
	FaultQuietGap  time.Duration
	FaultObserved  func(FaultQuietTiming)
}

func ValidateProcessParameters(p ProcessParameters) error {
	if p.ClockProbeFile != "" && (!filepath.IsAbs(p.ClockProbeFile) || strings.ContainsAny(p.ClockProbeFile, "\x00\r\n")) {
		return fault("invalid-clock-probe-file")
	}
	endpoint := func(s string) bool {
		u, err := url.Parse(s)
		if err != nil {
			return false
		}
		port, err := strconv.ParseUint(u.Port(), 10, 16)
		return err == nil && port > 0 && u.Scheme == "http" && u.User == nil && u.Path == "" && u.RawQuery == "" && u.Fragment == "" && u.Hostname() != ""
	}
	if !endpoint(p.Control) || !endpoint(p.Relay) || !regexp.MustCompile(`^[A-Za-z0-9.-]+$`).MatchString(p.Store) || p.StorePort < 1 || p.StorePort > 65535 || len(p.Workers) != 2 {
		return fault("invalid-process-endpoints")
	}
	for _, w := range []string{"w1", "w2"} {
		e, ok := p.Workers[w]
		if !ok || !endpoint(e.HTTP) || !e.Media.IsValid() || e.Media.Port() == 0 {
			return fault("invalid-process-endpoints")
		}
	}
	return nil
}
func (r *ProcessRuntime) VerifyClocks(ctx context.Context, end time.Duration) (refusal error) {
	defer func() {
		if r.Session != nil && isLeaseRefusal(refusal) {
			r.Session.observationFailure("hosts", "clock", refusal)
		}
	}()
	if r.Leases == nil {
		return fault("clock-lease-missing")
	}
	if err := r.Leases.Refresh(ctx, end); err != nil {
		return r.preflightFailure(err, "clocks")
	}
	e, err := r.Leases.Snapshot()
	if err == nil {
		r.ClockSamples = append(r.ClockSamples, e)
		if r.ClockObserved != nil {
			r.ClockObserved(e)
		}
	}
	return r.preflightFailure(err, "clocks")
}
func (r *ProcessRuntime) Inventory(ctx context.Context, target string, end time.Duration) error {
	i := 0
	if target == "to" {
		i = 1
	}
	if len(r.Pins) != 2 {
		return fault("invalid-release-pins")
	}
	_, err := r.Session.FreshInventory(ctx, r.Peer, r.Pins, end, r.Pins[i].Commit, true)
	return r.preflightFailure(err, "inventory")
}

// LeaseStep supplies the bound fault guard. Recovery permits the
// replace needed to restore a settled worker; rollback uses the fixed release
// restoration list. Neither bypasses inventory or the unknown-result latch.
type LeaseStep struct {
	Guard    *LeaseGuard
	Recovery bool
}

func (r *ProcessRuntime) Step(ctx context.Context, op, slot string, end time.Duration, extra ProcessExtra) (StepResult, error) {
	return r.StepWithLeases(ctx, op, slot, end, extra, LeaseStep{Guard: r.Leases})
}

func (r *ProcessRuntime) StepWithLeases(ctx context.Context, op, slot string, end time.Duration, extra ProcessExtra, lease LeaseStep) (StepResult, error) {
	return r.step(ctx, op, slot, end, extra, lease)
}

func (r *ProcessRuntime) step(ctx context.Context, op, slot string, end time.Duration, extra ProcessExtra, lease LeaseStep) (result StepResult, err error) {
	defer func() {
		observedSlot := slot
		if (op == "worker-status" || op == "worker-empty") && extra.Worker != "" {
			observedSlot = extra.Worker
		}
		r.Session.observationFailure(observedSlot, op, err)
	}()
	if r.Session.Latched() {
		return StepResult{}, fault("command-result-unknown")
	}
	if err := ValidateProcessParameters(r.Config.Parameters); err != nil {
		return StepResult{}, err
	}
	expected := "shared"
	switch op {
	case "worker-down", "worker-stop", "worker-start":
		expected = extra.Worker
	case "relay-status", "relay-inspect", "relay-stage", "relay-stop", "relay-replenish":
		expected = "relay"
	}
	if (op != "telemetry" && slot != expected) || slot != "shared" && slot != "relay" && slot != "w1" && slot != "w2" || extra.Worker != "" && extra.Worker != "w1" && extra.Worker != "w2" {
		return StepResult{}, fault("invalid-process-scope")
	}
	var stage *host.Stage
	rollbackMove := (op == "move" || op == "drain") && r.Config.Operation == "rollback" && r.Config.Target == "from"
	restore := rollbackMove || leaseRestoration(op, r.Config.Operation == "rollback" && r.Config.Target == "from", lease.Recovery)
	if !host.IsRead(op) {
		deployed := ""
		if !r.Config.Mixed {
			if len(r.Pins) != 2 {
				return StepResult{}, fault("invalid-release-pins")
			}
			deployed = r.Pins[0].Commit
		}
		pool, err := r.Session.FreshInventory(ctx, r.Peer, r.Pins, end, deployed, false)
		if err != nil {
			return StepResult{}, r.preflightFailure(err, "inventory")
		}
		if !restore {
			if lease.Guard == nil {
				return StepResult{}, fault("clock-lease-missing")
			}
			err = lease.Guard.Refresh(ctx, end)
		}
		if err != nil {
			return StepResult{}, r.preflightFailure(err, "clocks")
		}
		var packed bytes.Buffer
		z := gzip.NewWriter(&packed)
		if err = json.NewEncoder(z).Encode(pool); err != nil {
			return StepResult{}, err
		}
		if err = z.Close(); err != nil {
			return StepResult{}, err
		}
		i := 0
		if r.Config.Target == "to" {
			i = 1
		}
		pin := r.Pins[i]
		operation := r.Config.Operation
		if operation == "" {
			operation = "upgrade"
		}
		arch := r.Architecture
		if arch == "" {
			arch = "arm64"
		}
		stage = &host.Stage{Version: pin.Version, Arch: arch, ArchiveSHA256: pin.ArchiveSHA256, ManifestSHA256: pin.ManifestSHA256, Pool: base64.StdEncoding.EncodeToString(packed.Bytes()), Operation: operation}
	}
	parameters := struct {
		ProcessParameters
		ProcessExtra
		Slot      string `json:"slot"`
		TargetDir string `json:"targetDir"`
	}{r.Config.Parameters, extra, slot, ""}
	if len(r.Pins) == 2 {
		i := 0
		if r.Config.Target == "to" {
			i = 1
		}
		parameters.TargetDir = "/opt/relais/preview/" + r.Pins[i].Version
	}
	script, err := host.Process(op, parameters, stage)
	if err != nil {
		return StepResult{}, err
	}
	// All ordinary preflight host reads are protected. Restoration (including
	// stable-from rollback trials) and post-operation coverage keep their timing.
	plannedFault := !restore && (op == "move" || op == "drain" || op == "worker-down")
	quiet := FaultQuietTiming{Action: op, Slot: slot}
	if plannedFault {
		if r.FaultObserved != nil {
			defer func() { r.FaultObserved(quiet) }()
		}
		if err = lease.Guard.prepareFault(ctx, end-operator.HostSubmissionMinimum, r.FaultQuietGap, &quiet); err != nil {
			return StepResult{}, r.preflightFailure(err, "clocks")
		}
	}
	if !host.IsRead(op) && !restore && r.BeforeMutation != nil {
		if err = r.BeforeMutation(ctx); err != nil {
			return StepResult{}, err
		}
	}
	var hooks []ExchangeHooks
	if !host.IsRead(op) && !restore {
		if err = lease.Guard.Check(); err != nil {
			return StepResult{}, err
		}
		hooks = []ExchangeHooks{{BeforeSend: lease.Guard.Check}}
		if plannedFault {
			hooks[0].BeforeSend = func() error {
				quiet.BeforeSendNS = int64(r.Session.clock.Elapsed())
				return lease.Guard.checkFaultQuiet(r.FaultQuietGap, &quiet)
			}
		}
	}
	var observed StepResult
	var operationErr error
	// Positive product counters can survive incomplete operation evidence.
	// A refusal cannot become definite while this operation remains unknown.
	var productFailure error
	certainty, err := r.Session.Exchange(ctx, r.Peer, operator.HostExecute, slot, op, nil, []byte(script), end, func(reply operator.Reply) operator.Certainty {
		if reply.NotSubmitted {
			operationErr = fault("precondition-failed:" + reply.RefusalCode)
			return operator.Failed
		}
		var output operator.HostOutput
		if observe.Decode(reply.Payload, &output, observe.Contract{"stdout"}) != nil || len(output.Stdout) > operator.MaxHostOutputBytes {
			return operator.Unknown
		}
		data, e := host.Unpack(output.Stdout, 256000)
		if e != nil {
			return operator.Unknown
		}
		var envelope struct {
			Kind    string          `json:"kind"`
			Code    string          `json:"code"`
			Started time.Time       `json:"started"`
			Ended   time.Time       `json:"ended"`
			Data    json.RawMessage `json:"data"`
		}
		if observe.Decode(data, &envelope, observe.Contract{"kind"}) != nil {
			return operator.Unknown
		}
		if envelope.Kind == "refused" {
			operationErr = fault("compatibility-refused")
			return operator.Failed
		}
		if envelope.Kind == "failed" && host.IsPureRead(op) && regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`).MatchString(envelope.Code) {
			operationErr = fault("precondition-failed:" + envelope.Code)
			return operator.Failed
		}
		if envelope.Kind != "ok" || envelope.Started.IsZero() || envelope.Ended.IsZero() {
			return operator.Unknown
		}
		observed, e = DecodeStepResult(op, envelope.Data)
		observed.Started = envelope.Started
		observed.Ended = envelope.Ended
		if e != nil {
			productFailure = knownProductFailure(op, envelope.Data)
			if host.IsPureRead(op) {
				if productFailure == nil {
					operationErr = fault("precondition-failed:observation")
				}
				return operator.Failed
			}
			return operator.Unknown
		}
		if op == "worker-down" {
			var bracket struct {
				Before string `json:"signalBeforeUnixNs"`
				After  string `json:"signalAfterUnixNs"`
			}
			if observe.Decode(envelope.Data, &bracket, observe.Contract{"signalBeforeUnixNs", "signalAfterUnixNs"}) != nil {
				return operator.Unknown
			}
			var diagnostic struct {
				Signal *observe.SignalDiagnostics `json:"signalDiagnostics"`
			}
			// Optional diagnostics cannot change operation certainty.
			if json.Unmarshal(envelope.Data, &diagnostic) != nil {
				diagnostic.Signal = nil
			}
			workerError, bound := int64(0), int64(0)
			workerError, e = lease.Guard.workerError()
			if e != nil {
				return operator.Unknown
			}
			bound = lease.Guard.bounds.FrontendWorkerBoundNS
			midpoint, e := clocks.SignalMidpoint(bracket.Before, bracket.After, workerError, bound, envelope.Started, envelope.Ended)
			r.Session.retainClockBracket(slot, bracket.Before, bracket.After, midpoint, diagnostic.Signal)
			if errors.Is(e, clocks.ErrSignalBracketWide) {
				r.Session.mu.Lock()
				if n := len(r.Session.clockBrackets); n > 0 && r.Session.clockBrackets[n-1].Worker == slot && r.Session.clockBrackets[n-1].BeforeUnixNS == bracket.Before && r.Session.clockBrackets[n-1].AfterUnixNS == bracket.After {
					r.Session.clockBrackets[n-1].LeaseExcluded = true
					observed.KillExcluded = true
				}
				r.Session.mu.Unlock()
			}
			if e != nil && !errors.Is(e, clocks.ErrSignalBracketWide) {
				return operator.Unknown
			}
			ns, _ := strconv.ParseUint(midpoint, 10, 64)
			observed.Started = time.Unix(int64(ns/1_000_000_000), int64(ns%1_000_000_000))
		}
		return operator.Succeeded
	}, hooks...)
	observed.Certainty = certainty
	if rollbackMove && certainty == operator.Succeeded {
		start, finish := observed.Started, observed.Ended
		moves := observed.Drain.Moves
		if op == "move" {
			moves = []controlplane.MoveResult{observed.Move}
		}
		haveSpan := false
		for _, move := range moves {
			if move.Start.IsZero() || move.End.IsZero() || move.End.Before(move.Start) {
				continue
			}
			if !haveSpan || move.Start.Before(start) {
				start = move.Start
			}
			if !haveSpan || move.End.After(finish) {
				finish = move.End
			}
			haveSpan = true
		}
		coverageErr := error(fault("clock-lease-missing"))
		if lease.Guard != nil {
			coverageErr = lease.Guard.coverRestoration(ctx, end, start, finish)
		}
		if coverageErr != nil {
			r.Session.mu.Lock()
			if r.Session.clockMissingIntervals == nil {
				r.Session.clockMissingIntervals = map[string]observe.CoverageBreak{}
			}
			front, _ := inventory.ComponentSlot(r.Session.inventoryComponents, "signal")
			b := observe.CoverageBreak{FromUnixSeconds: start.Unix(), UnixSeconds: finish.Unix(), Kind: "clock-lease-missing"}
			if old, ok := r.Session.clockMissingIntervals[front]; ok {
				b.FromUnixSeconds = min(b.FromUnixSeconds, old.FromUnixSeconds)
				b.UnixSeconds = max(b.UnixSeconds, old.UnixSeconds)
			}
			if isLeaseRefusal(coverageErr) {
				b.Kind = coverageErr.Error()
			}
			r.Session.clockMissingIntervals[front] = b
			r.Session.mu.Unlock()
		}
		if r.Session.Latched() {
			err = fault("command-result-unknown")
		}
	}
	if err != nil {
		if certainty == operator.Failed && isLeaseRefusal(err) && !r.Session.Latched() {
			return observed, err
		}
		return observed, fault("command-result-unknown")
	}
	if certainty == operator.Unknown {
		if productFailure != nil {
			return observed, productFailure
		}
		return observed, fault("command-result-unknown")
	}
	if productFailure != nil {
		return observed, productFailure
	}
	if operationErr != nil {
		return observed, operationErr
	}
	if certainty != operator.Succeeded {
		return observed, fault("precondition-failed")
	}
	return observed, nil
}

// DecodeStepResult is the one product JSON ingress for fixed steps. Values are
// never reconstructed in the resource adapter. Missing required proof is an error.
func DecodeStepResult(op string, data json.RawMessage) (r StepResult, err error) {
	r.Raw = bytes.Clone(data)
	if op == "status" {
		var presence map[string]json.RawMessage
		_ = json.Unmarshal(data, &presence)
		value, ok := presence["lost_count"]
		var n uint64
		r.LossKnown = ok && json.Unmarshal(value, &n) == nil
	}
	switch op {
	case "status":
		r.Status, err = observe.Control(data)
		var h struct {
			Takeovers []json.RawMessage `json:"takeovers"`
		}
		if e := json.Unmarshal(data, &h); err == nil {
			err = e
		}
		r.History = h.Takeovers
	case "drain":
		r.Drain, err = observe.Drain(data)
		var d struct {
			Moves []json.RawMessage `json:"moves"`
		}
		if e := json.Unmarshal(data, &d); err == nil {
			err = e
		}
		r.Moves = d.Moves
	case "move":
		r.Move, err = observe.Move(data)
	case "relay-inspect":
		r.Inspection, err = observe.RelayInspection(data)
	case "relay-status":
		r.Inspection.Status, err = observe.Relay(data)
	case "worker-status":
		r.Local, err = observe.Worker(data)
	case "telemetry":
		r.Telemetry, err = DecodeHostTelemetry(data)
	case "store-status":
		r.Ownership, err = observe.StoreOwnership(data)
		r.OwnershipKnown = err == nil
	case "control-start":
		var ready struct {
			ReadyMS   int64 `json:"readyMs"`
			Completed bool  `json:"completed"`
		}
		err = observe.Decode(data, &ready, observe.Contract{"readyMs", "completed"})
		if !ready.Completed || ready.ReadyMS < 0 || ready.ReadyMS > maxSafeEpoch {
			err = fault("missing-readiness-evidence")
		}
		r.ReadyMS = &ready.ReadyMS
	case "worker-start", "worker-stop", "worker-down", "control-stop", "freeze", "unfreeze", "signal-stop", "signal-start", "relay-stage", "relay-stop", "relay-replenish", "worker-empty":
		var proof struct {
			Completed bool `json:"completed"`
		}
		err = observe.Decode(data, &proof, observe.Contract{"completed"})
		if err == nil && !proof.Completed {
			err = fault("missing-completion-proof")
		}
	default:
		err = observe.Decode(data, &struct{}{}, nil)
	}
	return r, err
}

func (r *ProcessRuntime) preflightFailure(err error, kind string) error {
	return r.Session.preflightFailure(err, kind)
}

func (s *Session) preflightFailure(err error, kind string) error {
	if err == nil {
		return nil
	}
	if s.Latched() {
		return fault("command-result-unknown")
	}
	if isLeaseRefusal(err) {
		return fault(err.Error())
	}
	var known *ProcedureError
	if errors.As(err, &known) {
		return err
	}
	var refused *releasecompat.Refusal
	if errors.As(err, &refused) {
		return fault("compatibility-refused")
	}
	var unavailable *inventory.Failure
	if errors.As(err, &unavailable) {
		return fault("precondition-failed:" + unavailable.Slot + "-" + unavailable.Code)
	}
	return fault("precondition-failed:" + kind)
}

// Missing unrelated evidence cannot erase a known product failure. These narrow
// decodes consume only the required positive counter, using the product type.
// They never authorize a pass or clear transport uncertainty.
func knownProductFailure(op string, data json.RawMessage) error {
	var fields map[string]json.RawMessage
	if observe.Decode(data, &fields, nil) != nil {
		return nil
	}
	project := func(key string) []byte { b, _ := json.Marshal(map[string]json.RawMessage{key: fields[key]}); return b }
	switch op {
	case "status":
		var s controlplane.Status
		if observe.Decode(project("lost_count"), &s, observe.Contract{"lost_count"}) == nil && s.LostCount > 0 {
			return fault("known-call-loss")
		}
	case "move":
		var m controlplane.MoveResult
		if observe.Decode(project("lost"), &m, observe.Contract{"lost"}) == nil && m.Lost {
			return fault("known-call-loss")
		}
	case "drain":
		var moves []json.RawMessage
		if json.Unmarshal(fields["moves"], &moves) == nil {
			for _, move := range moves {
				if err := knownProductFailure("move", move); err != nil {
					return err
				}
			}
		}
	case "relay-inspect":
		var status, stats map[string]json.RawMessage
		if json.Unmarshal(fields["status"], &status) != nil || json.Unmarshal(status["stats"], &stats) != nil {
			return nil
		}
		counter, _ := json.Marshal(map[string]json.RawMessage{"SelfFences": stats["SelfFences"]})
		projection, _ := json.Marshal(map[string]json.RawMessage{"stats": counter})
		var s relay.RelayStatus
		if observe.Decode(projection, &s, observe.Contract{"stats.SelfFences"}) == nil && s.Stats.SelfFences > 0 {
			return fault("known-relay-failure")
		}
	}
	return nil
}
