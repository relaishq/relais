package operatorgates

import (
	"context"
	"slices"
	"time"

	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/operator"
)

// StoreObservation is adapter-owned evidence, not a copy of product status.
// An absent capability or unknown durability cannot authorize a failover.
type StoreObservation struct {
	Primary    string `json:"primary"`
	Available  bool   `json:"available"`
	Durability string `json:"durability"`
}
type StoreCapability interface {
	Observe(context.Context, time.Duration) (StoreObservation, error)
	Failover(context.Context, time.Duration) error
}
type MeasurementObservation struct {
	At     time.Time
	Hosts  map[string]HostTelemetry
	Status *controlplane.Status
	Local  map[string]mediaworker.WorkerStatus
	Store  *StoreObservation
}
type MeasurementEvidence struct {
	Media                   *MediaSummary
	KnownAllowanceViolation bool
}
type MeasurePorts struct {
	ProcedurePorts
	Calls       int
	Store       StoreCapability
	Observe     func(context.Context, time.Duration) (MeasurementObservation, error)
	BeforeFault func(context.Context, time.Duration) error
	MarkFault   func(context.Context) error
	// Collect starts one evidence collection. It returns a terminal channel,
	// allowing this gate to retain telemetry while the caller is still running.
	// Cancellation must terminate collection and publish/close the channel only
	// after its evidence has been added; the gate joins it before returning.
	Collect func(context.Context, time.Duration) <-chan MeasurementCollection
	Persist func(context.Context, []MeasurementObservation) error
}
type MeasurementCollection struct {
	Evidence MeasurementEvidence
	Err      error
}
type Measurement struct {
	ENAAllowanceDelta         *uint64  `json:"enaAllowanceDelta,omitempty"`
	ENAKnownAllowanceDelta    *uint64  `json:"enaKnownAllowanceDelta,omitempty"`
	RedisWriteP99UpperSeconds *float64 `json:"redisWriteP99UpperSeconds,omitempty"`
	CheckpointP99UpperSeconds *float64 `json:"checkpointP99UpperSeconds,omitempty"`
	SnapshotAgeMaxMS          *float64 `json:"snapshotAgeMaxMs,omitempty"`
	CheckpointAgeMaxMS        *float64 `json:"checkpointAgeMaxMs,omitempty"`
	StoreCommandsPerSecond    *float64 `json:"storeCommandsPerSecond,omitempty"`
	StoreBytesPerSecond       *float64 `json:"storeBytesPerSecond,omitempty"`
	PublicUDPBytesPerSecond   *float64 `json:"publicUdpBytesPerSecond,omitempty"`
	Samples                   int      `json:"samples"`
	MissingSamples            int      `json:"missingSamples"`
	RecoveredMS               *int64   `json:"recoveredMs,omitempty"`
	PrimaryChanged            *int     `json:"primaryChanged,omitempty"`
	SynchronousDurability     *int     `json:"synchronousDurability,omitempty"`
	AdmittedCalls             *int     `json:"admittedCalls,omitempty"`
}
type MeasureResult struct {
	Status      operator.Verdict `json:"status"`
	Reason      string           `json:"reason,omitempty"`
	Media       *MediaSummary    `json:"media,omitempty"`
	Measurement Measurement      `json:"measurement"`
}

func liveMeasurement(s *controlplane.Status, reg *Registration, c *ProcessConfig, calls int) error {
	if s != nil && s.LostCount > 0 {
		return fault("known-call-loss")
	}
	if s == nil || reg == nil || !s.Ready || s.Pool.Cap != 2 || s.Pool.Reservations != 0 || len(s.Calls) != calls || !alive(*s, "w1", c.Parameters.Workers) || !alive(*s, "w2", c.Parameters.Workers) {
		return fault("missing-live-inventory")
	}
	for _, id := range reg.Sessions {
		call, ok := findCall(*s, id)
		if !ok || !validCall(call) {
			return fault("missing-live-inventory")
		}
	}
	return nil
}
func WaitBoundaryIdle(ctx context.Context, p ProcedurePorts, end time.Duration) error {
	for p.Clock.Elapsed() < end {
		r, err := p.Step(ctx, "status", "shared", end, ProcessExtra{})
		if err != nil {
			return err
		}
		s := r.Status
		if s.LostCount > 0 {
			return fault("known-call-loss")
		}
		idle := s.Ready && s.Pool.Cap == 2 && s.Pool.Reservations == 0 && len(s.Calls) == 0
		for _, w := range []string{"w1", "w2"} {
			r, err = p.Step(ctx, "worker-status", "shared", end, ProcessExtra{Worker: w})
			if err != nil {
				return err
			}
			local := r.Local
			idle = idle && local.InventoryKnown && !local.Truncated && local.Sessions == 0 && local.Inventory != nil && len(local.Inventory) == 0
		}
		r, err = p.Step(ctx, "store-status", "shared", end, ProcessExtra{})
		if err != nil {
			return err
		}
		_, first := r.Ownership.Owners["w1"]
		_, second := r.Ownership.Owners["w2"]
		idle = idle && r.OwnershipKnown && first && second && r.Ownership.Owners["w1"] == 0 && r.Ownership.Owners["w2"] == 0 && r.Ownership.Transfers == 0
		if idle {
			return nil
		}
		if err = p.Sleep(ctx, min(500*time.Millisecond, end-p.Clock.Elapsed())); err != nil {
			return err
		}
	}
	return fault("gate-deadline")
}
func RunMeasureGate(ctx context.Context, gate operator.Gate, c *ProcessConfig, p MeasurePorts, end time.Duration) MeasureResult {
	g := newProcedure(c, p.ProcedurePorts, end)
	r := MeasureResult{Status: operator.Inconclusive}
	rows := []MeasurementObservation{}
	failed := false
	observe := func() (*MeasurementObservation, error) {
		if p.Clock.Elapsed() >= g.end {
			return nil, fault("gate-deadline")
		}
		o, err := p.Observe(ctx, g.end)
		if o.Status != nil && o.Status.LostCount > 0 {
			failed = true
		}
		if code(err) == "command-result-unknown" {
			g.unknown = true
			r.Reason = code(err)
		}
		if err == nil {
			rows = append(rows, o)
		}
		if err != nil {
			return nil, err
		}
		return &rows[len(rows)-1], nil
	}
	err := g.measureDefinition(ctx, gate, p, observe, &r.Measurement)
	switch code(err) {
	case "known-call-loss", "compatibility-refused":
		failed = true
	case "command-result-unknown":
		g.unknown = true
		r.Reason = code(err)
	case "gate-deadline", "unsupported-capability":
		r.Reason = code(err)
	default:
		if err != nil {
			r.Measurement.MissingSamples++
		}
	}
	if g.unknown {
		r.Reason = "command-result-unknown"
	}
	if len(g.down) > 0 && !g.unknown {
		if err = g.restoreWorkers(ctx); err != nil {
			if code(err) == "known-call-loss" {
				failed = true
			}
			if code(err) == "command-result-unknown" {
				r.Reason = code(err)
			} else {
				r.Reason = "restore-failed"
			}
		}
	}
	procedureLate := p.Clock.Elapsed() >= g.end
	collectionEnd, collectionParent := g.end, ctx
	if p.CollectionContext != nil {
		collectionEnd, collectionParent = max(g.end, p.CollectionEnd), p.CollectionContext
	}
	if p.Collect != nil && (p.CollectionContext == nil || p.Clock.Elapsed() < collectionEnd && collectionParent.Err() == nil) {
		collectionCtx, cancelCollection := context.WithCancel(collectionParent)
		collecting := p.Collect(collectionCtx, collectionEnd)
		var collection MeasurementCollection
		received := false
	collect:
		for {
			select {
			case result, ok := <-collecting:
				collection, received = result, true
				if !ok {
					collection.Err = context.Canceled
				}
				break collect
			default:
			}
			if p.Clock.Elapsed() >= g.end {
				r.Measurement.MissingSamples++
				break
			}
			if !g.unknown {
				if _, err = observe(); err != nil {
					r.Measurement.MissingSamples++
				}
			}
			// The collection channel may finish during the sample interval. The sleep
			// is bounded by the original gate deadline, never a renewed wait budget.
			if err = p.Sleep(ctx, min(15*time.Second, max(time.Duration(0), g.end-p.Clock.Elapsed()))); err != nil {
				break
			}
		}
		// Procedure telemetry stops at its own cutoff. The already-started
		// collector may finish within the reserved evidence window.
		if !received && p.CollectionContext != nil {
			select {
			case result, ok := <-collecting:
				collection, received = result, true
				if !ok {
					collection.Err = context.Canceled
				}
			case <-collectionCtx.Done():
			}
		}
		// Return collection ownership only after its worker has stopped and added
		// readable evidence to the pool. Cancellation never discards its result.
		cancelCollection()
		if !received {
			var ok bool
			collection, ok = <-collecting
			if !ok {
				collection.Err = context.Canceled
			}
		}
		if collection.Err != nil {
			r.Measurement.MissingSamples++
		}
		r.Media = collection.Evidence.Media
		if collection.Evidence.KnownAllowanceViolation {
			failed = true
		}
	}
	if p.Persist != nil {
		if err = p.Persist(ctx, rows); err != nil {
			r.Measurement.MissingSamples++
		}
	}
	summary := SummarizeObservations(rows)
	summary.MissingSamples += r.Measurement.MissingSamples
	summary.RecoveredMS = r.Measurement.RecoveredMS
	summary.PrimaryChanged = r.Measurement.PrimaryChanged
	summary.SynchronousDurability = r.Measurement.SynchronousDurability
	summary.AdmittedCalls = r.Measurement.AdmittedCalls
	r.Measurement = summary
	if summary.ENAKnownAllowanceDelta != nil && *summary.ENAKnownAllowanceDelta > 0 {
		failed = true
	}
	if r.Media != nil && r.Media.Status == operator.Failure {
		failed = true
	}
	if failed {
		r.Status = operator.Failure
	} else if r.Reason == "" && (procedureLate || p.Clock.Elapsed() >= collectionEnd) {
		r.Reason = "gate-deadline"
	}
	return r
}

// measureDefinition owns the general admission/failover procedure. Evidence
// collection and resource transport implement separate, typed seams.
func (g *procedure) measureDefinition(ctx context.Context, gate operator.Gate, p MeasurePorts, observe func() (*MeasurementObservation, error), measurement *Measurement) error {
	c := g.c

	reg := p.Broker.Registration()
	if reg == nil || !reg.Near || p.Clock.Elapsed() >= g.end {
		return fault("gate-deadline")
	}
	initial, err := observe()
	if err != nil {
		return err
	}
	settle := min(g.end, p.Clock.Elapsed()+time.Minute)
	for initial.Status != nil && len(initial.Status.Calls) > p.Calls && p.Clock.Elapsed() < settle {
		if err = p.Sleep(ctx, 5*time.Second); err != nil {
			return err
		}
		initial, err = observe()
		if err != nil {
			return err
		}
	}
	if err = liveMeasurement(initial.Status, reg, c, p.Calls); err != nil {
		return err
	}
	switch gate {
	case operator.Failover:
		if p.Store == nil {
			return fault("unsupported-capability")
		}
		if err = TrialWarmup(ctx, p.ProcedurePorts, g.end); err != nil {
			return err
		}
		before, err := p.Store.Observe(ctx, g.end)
		if err != nil {
			return err
		}
		if before.Durability != "sync" || !before.Available || before.Primary == "" {
			return fault("missing-synchronous-primary")
		}
		ready, err := observe()
		if err != nil {
			return err
		}
		ready.Store = &before
		if err = liveMeasurement(ready.Status, reg, c, p.Calls); err != nil {
			return err
		}
		if p.BeforeFault == nil || p.MarkFault == nil {
			return fault("missing-fault-proof")
		}
		if err = p.BeforeFault(ctx, g.end); err != nil {
			return err
		}
		if err = p.MarkFault(ctx); err != nil {
			return err
		}
		at := p.Clock.Elapsed()
		if p.Clock.Elapsed() >= g.end {
			return fault("gate-deadline")
		}
		if err = p.Store.Failover(ctx, g.end); err != nil {
			return err
		}
		for p.Clock.Elapsed() < g.end {
			o, err := observe()
			if err != nil {
				return err
			}
			current, err := p.Store.Observe(ctx, g.end)
			if err != nil {
				if code(err) == "command-result-unknown" {
					return err
				}
				measurement.MissingSamples++
			} else {
				o.Store = &current
			}
			if err == nil && current.Available && current.Primary != "" && current.Primary != before.Primary && current.Durability == "sync" {
				if err = liveMeasurement(o.Status, reg, c, p.Calls); err != nil {
					return err
				}
				ms := (p.Clock.Elapsed() - at).Milliseconds()
				one := 1
				measurement.RecoveredMS = &ms
				measurement.PrimaryChanged = &one
				measurement.SynchronousDurability = &one
				break
			}
			if err = p.Sleep(ctx, min(5*time.Second, g.end-p.Clock.Elapsed())); err != nil {
				return err
			}
		}
		if measurement.RecoveredMS == nil {
			return fault("gate-deadline")
		}
		// Resource failover is never fabricated as a worker takeover receipt.
		return g.trials(ctx, TrialTarget{1, 1}, "failover-recovered")
	case operator.Load:
		for _, w := range []string{"w1", "w2"} {
			local, ok := initial.Local[w]
			owned := []controlplane.CallStatus{}
			for _, call := range initial.Status.Calls {
				if call.Owner == w {
					owned = append(owned, call)
				}
			}
			if !ok || !local.InventoryKnown || local.Truncated || local.Sessions != len(owned) || local.Inventory == nil || len(local.Inventory) != len(owned) {
				return fault("missing-independent-admission-inventory")
			}
			for _, call := range owned {
				if !slices.ContainsFunc(local.Inventory, func(i mediaworker.InstalledSession) bool {
					return i.ID == call.ID && i.Epoch == call.Epoch && i.Owner == c.Parameters.Workers[w].Media
				}) {
					return fault("missing-independent-admission-inventory")
				}
			}
		}
		measurement.AdmittedCalls = &p.Calls
	default:
		return fault("invalid-gate")
	}
	return nil
}
