package operatorgates

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/relaishq/relais/internal/loadgen"
	"github.com/relaishq/relais/internal/operatorcaller"
	"github.com/relaishq/relais/internal/operatorevidence"
	"github.com/relaishq/relais/internal/operatorgates/observe"
	"github.com/relaishq/relais/pkg/controlplane"
)

// MediaJudge is implemented by operatorevidence.PinnedJudge. Build and verify
// the pinned binary before starting callers; this seam never builds or retries.
type MediaJudge interface {
	Judge(context.Context, operatorevidence.Pool) (operatorevidence.Summary, error)
}

// EvidenceRuntime retains survivor phases for one release pin. Collection can
// append while a measurement retains telemetry; finalization only reads evidence.
type EvidenceRuntime struct {
	RunID   string
	Judge   MediaJudge
	mu      sync.Mutex
	phases  []operatorevidence.Phase
	missing bool
}

func (r *EvidenceRuntime) Add(phase operatorevidence.Phase, missing bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	phase.Raw = append([]byte{}, phase.Raw...)
	phase.Registration.Sessions = append([]string{}, phase.Registration.Sessions...)
	phase.Registration.Burst.Probes = append([]operatorcaller.Probe{}, phase.Registration.Burst.Probes...)
	r.phases = append(r.phases, phase)
	r.missing = r.missing || missing
}
func (r *EvidenceRuntime) Media(ctx context.Context) (*MediaSummary, error) {
	r.mu.Lock()
	phases, missing := append([]operatorevidence.Phase{}, r.phases...), r.missing
	r.mu.Unlock()
	pool, _, err := operatorevidence.ReadPool(r.RunID, phases, missing)
	if err != nil {
		return nil, fault("precondition-failed:media-pool")
	}
	summary, err := r.Judge.Judge(ctx, pool)
	return procedureMedia(summary), err
}
func procedureMedia(s operatorevidence.Summary) *MediaSummary {
	return &MediaSummary{IncludedPhases: s.IncludedPhases, DroppedPhases: s.DroppedPhases,
		ExcludedEvents: s.ExcludedEvents, SaturationOverlaps: s.SaturationOverlaps,
		UndecidedMetrics: s.UndecidedMetrics, PhaseDiagnostics: s.PhaseDiagnostics, Status: s.Status, QualifiedMoves: &s.QualifiedMoves, QualifiedTakeovers: &s.QualifiedTakeovers,
		ExpectedCalls: s.ExpectedCalls, CompletedCalls: s.CompletedCalls, EventCount: s.EventCount, MetricCount: s.MetricCount,
		FailedMetrics: s.FailedMetrics, InconclusiveMetrics: s.InconclusiveMetrics, MaxMediaGapNS: s.MaxMediaGapNS,
		DecryptionFailures: s.DecryptionFailures, ICERestarts: s.ICERestarts}
}

// CallerRuntime binds procedure callbacks to the real caller broker. End uses
// the procedure's elapsed clock; every mailbox deadline is clamped to it and the
// immutable caller manifest. Set Phase metadata before collecting. Wire MarkFault
// to ProcessRuntime.BeforeMutation / MeasurePorts.MarkFault. Media is the process
// callback; Collect is the asynchronous measurement callback. Both share one
// collection reservation. Pool.Media is the sequence's final read-only callback.
type CallerRuntime struct {
	Broker     *operatorcaller.Broker
	Clock      Clock
	End        time.Duration
	Pool       *EvidenceRuntime
	Phase      operatorevidence.Phase
	receipts   []Receipt
	incomplete bool
	once       sync.Once
	done       chan struct{}
	duplicates *uint64
	summary    *MediaSummary
	err        error
}

func NewCallerRuntime(b *operatorcaller.Broker, c Clock, end time.Duration, pool *EvidenceRuntime, phase operatorevidence.Phase) *CallerRuntime {
	return &CallerRuntime{Broker: b, Clock: c, End: end, Pool: pool, Phase: phase, done: make(chan struct{})}
}
func (r *CallerRuntime) deadline() time.Time {
	return minTime(r.Clock.Wall().Add(max(0, r.End-r.Clock.Elapsed())), time.UnixMilli(r.Broker.Manifest.Deadline))
}
func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
func (r *CallerRuntime) RunID() string { return r.Broker.Manifest.RunID }
func (r *CallerRuntime) Registration() *Registration {
	b := r.Broker
	if b.Registration == nil {
		return nil
	}
	registered := b.Registration.RegisteredAt
	out := &Registration{Near: b.Registration.Near, Sessions: append([]string{}, b.Registration.Sessions...), RegisteredAt: &registered}
	if !b.ReadyObservedAt.IsZero() {
		n := b.ReadyObservedAt.UnixMilli()
		out.ReadyObservedAt = &n
	}
	if !b.MediaStartedAt.IsZero() {
		n := b.MediaStartedAt.UnixMilli()
		out.MediaStartedAt = &n
	}
	return out
}
func (r *CallerRuntime) callerError(err error) error {
	if err == nil {
		return nil
	}
	if r.Broker.Fence.Latched() {
		return fault("command-result-unknown")
	}
	if err.Error() == "caller-deadline" {
		return fault("gate-deadline")
	}
	return fault("precondition-failed:caller")
}
func (r *CallerRuntime) Arm(ctx context.Context, operation string, ids []Identity) error {
	if ctx.Err() != nil || r.Clock.Elapsed() >= r.End {
		return fault("gate-deadline")
	}
	return r.callerError(r.Broker.Arm(ctx, operation, ids, r.deadline()))
}
func (r *CallerRuntime) Feed(ctx context.Context, operation string, receipts []Receipt) error {
	typed := make([]operatorcaller.Receipt, 0, len(receipts))
	for _, receipt := range receipts {
		var result controlplane.MoveResult
		// Validate original bytes before conversion can fill omitted members with
		// zero values. Keep failure facts, but never qualify an incomplete receipt.
		err := observe.DecodeEvidenceLimit(receipt.Result, &result, 256000)
		if observe.IsIncompleteEvidence(err) {
			r.incomplete = true
			receipt.Uncertain = true
			err = nil
		}
		if err != nil || r.Broker.Registration == nil || !slices.Contains(r.Broker.Registration.Sessions, receipt.Identity.SessionID) || receipt.Identity.OperationID != operation || !receipt.Identity.Valid() || receipt.Identity.RunID != r.RunID() || receipt.Kind != "move" && receipt.Kind != "takeover" || receipt.Start.IsZero() || receipt.End.Before(receipt.Start) {
			return fault("precondition-failed:caller-receipt")
		}
		typed = append(typed, operatorcaller.Receipt{Identity: receipt.Identity, Kind: receipt.Kind, Start: receipt.Start, End: receipt.End, Result: result, Uncertain: receipt.Uncertain})
	}
	if ctx.Err() != nil || r.Clock.Elapsed() >= r.End {
		return fault("gate-deadline")
	}
	_, err := r.Broker.Feed(ctx, operation, operatorcaller.Payload{Kind: "receipt", Receipts: typed}, r.deadline())
	if err == nil {
		for _, v := range receipts {
			v.Result = append(json.RawMessage{}, v.Result...)
			r.receipts = append(r.receipts, v)
		}
	}
	return r.callerError(err)
}
func (r *CallerRuntime) Events() []Receipt {
	out := append([]Receipt{}, r.receipts...)
	for i := range out {
		out[i].Result = append(json.RawMessage{}, out[i].Result...)
	}
	return out
}
func (r *CallerRuntime) MarkFault(ctx context.Context) error {
	return r.callerError(r.Broker.MarkFault(ctx))
}
func (r *CallerRuntime) startCollection(ctx context.Context, deadline time.Time) {
	r.once.Do(func() {
		go func() {
			defer close(r.done)
			caller, err := r.Broker.Collect(ctx, deadline)
			if r.Broker.Registration != nil {
				r.duplicates, _ = operatorevidence.JoinedDuplicatePackets(caller, r.Broker.Registration.Sessions, r.Broker.CallerPID)
			}
			phase := r.Phase
			phase.Gate = r.Broker.Manifest.Gate
			phase.Plan = r.Broker.Manifest.Plan
			if r.Broker.Registration != nil {
				phase.Registration = *r.Broker.Registration
			}
			if r.Broker.FirstFaultAt != nil {
				phase.FirstFaultAt = *r.Broker.FirstFaultAt
			}
			events, eventErr := receiptEvents(r.Broker.Events)
			var marshalErr error
			phase.Raw, marshalErr = json.Marshal(operatorevidence.RawMedia{CallerJSONL: caller, Events: events, Receipts: append([]operatorcaller.Receipt{}, r.Broker.Events...)})
			err = errors.Join(err, eventErr, marshalErr)
			r.Pool.Add(phase, err != nil || r.incomplete)
			retained, cancel := operatorcaller.EvidenceRetentionContext(ctx)
			defer cancel()
			r.summary, r.err = r.Pool.Media(retained)
			if err != nil && r.err == nil {
				r.err = fault("precondition-failed:caller-evidence")
			}
		}()
	})
}
func (r *CallerRuntime) awaitCollection(ctx context.Context) (*MediaSummary, error) {
	// Rejoin the collection owner before a stop or boundary can touch the broker.
	// Post-collection reads and judgment use bounded read-only retention contexts.
	<-r.done
	if ctx.Err() != nil {
		return r.summary, ctx.Err()
	}
	return r.summary, r.err
}
func (r *CallerRuntime) Media(ctx context.Context) (*MediaSummary, error) {
	r.startCollection(ctx, r.deadline())
	return r.awaitCollection(ctx)
}
func (r *CallerRuntime) Collect(ctx context.Context, end time.Duration) <-chan MeasurementCollection {
	ch := make(chan MeasurementCollection, 1)
	bounded, cancel := context.WithTimeout(ctx, max(0, min(end, r.End)-r.Clock.Elapsed()))
	r.startCollection(bounded, r.deadline())
	go func() {
		defer cancel()
		summary, err := r.awaitCollection(bounded)
		ch <- MeasurementCollection{Evidence: MeasurementEvidence{Media: summary}, Err: err}
		close(ch)
	}()
	return ch
}

func receiptEvents(receipts []operatorcaller.Receipt) ([]loadgen.EventResult, error) {
	if len(receipts) > maxSafeEpoch {
		return nil, fault("invalid-receipt-count")
	}
	out := []loadgen.EventResult{}
	groups := map[string]int{}
	for _, receipt := range receipts {
		key := receipt.Identity.OperationID
		index, ok := groups[key]
		if !ok {
			index = len(out)
			groups[key] = index
			kind := receipt.Kind
			if kind == "takeover" {
				kind = "kill"
			}
			worker := 1
			if receipt.Identity.Worker == "w2" {
				worker = 2
			}
			out = append(out, loadgen.EventResult{Event: loadgen.Event{Kind: kind, Worker: worker}, At: receipt.Start, FinishedAt: receipt.End})
		}
		event := &out[index]
		event.At = minTime(event.At, receipt.Start)
		if receipt.End.After(event.FinishedAt) {
			event.FinishedAt = receipt.End
		}
		event.Identities = append(event.Identities, receipt.Identity)
		event.Sessions = append(event.Sessions, receipt.Identity.SessionID)
		event.Affected = len(event.Identities)
		event.Event.Count = event.Affected
	}
	return out, nil
}

var _ GateBroker = (*CallerRuntime)(nil)
var _ MediaJudge = operatorevidence.PinnedJudge{}

// Duplicates returns only terminal, joined caller evidence.
func (r *CallerRuntime) Duplicates() *uint64 {
	select {
	case <-r.done:
		return r.duplicates
	default:
		return nil
	}
}
