package operatorgates

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/relaishq/relais/internal/operatorgates/observe"
	"github.com/relaishq/relais/pkg/callharness"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/operator"
)

const CalibrationWarmup = 30 * time.Second
const FaultCallAge = 190 * time.Second
const ConsentWindow = 60 * time.Second
const maxSafeEpoch = 1<<53 - 1

// These interfaces are the integration boundary for the caller and evidence
// owners. CallerRuntime implements the broker seam with operatorcaller.Broker.
// Neither may advance a gate, restore a host or retry an unknown effect.
type Registration struct {
	Near            bool     `json:"near"`
	Sessions        []string `json:"sessions"`
	RegisteredAt    *int64   `json:"registeredAt,omitempty"`
	ReadyObservedAt *int64   `json:"readyObservedAt,omitempty"`
	MediaStartedAt  *int64   `json:"mediaStartedAt,omitempty"`
}
type Identity = callharness.OperationIdentity

type Receipt struct {
	Identity  Identity        `json:"identity"`
	Kind      string          `json:"kind"`
	Start     time.Time       `json:"start"`
	End       time.Time       `json:"end"`
	Result    json.RawMessage `json:"result"`
	Uncertain bool            `json:"uncertain"`
}
type GateBroker interface {
	RunID() string
	Registration() *Registration
	Arm(context.Context, string, []Identity) error
	Feed(context.Context, string, []Receipt) error
	Events() []Receipt
}
type MediaSummary struct {
	Status              operator.Verdict `json:"status"`
	QualifiedMoves      *int             `json:"qualifiedMoves,omitempty"`
	QualifiedTakeovers  *int             `json:"qualifiedTakeovers,omitempty"`
	ExpectedCalls       int              `json:"expectedCalls"`
	CompletedCalls      int              `json:"completedCalls"`
	EventCount          int              `json:"eventCount"`
	MetricCount         int              `json:"metricCount"`
	FailedMetrics       int              `json:"failedMetrics"`
	InconclusiveMetrics int              `json:"inconclusiveMetrics"`
	MaxMediaGapNS       int64            `json:"maxMediaGapNs"`
	DecryptionFailures  int              `json:"decryptionFailures"`
	ICERestarts         int              `json:"iceRestarts"`
}
type ProcessExtra struct {
	Worker     string   `json:"worker,omitempty"`
	ActiveUnit string   `json:"activeUnit,omitempty"`
	Session    string   `json:"session,omitempty"`
	Owner      string   `json:"owner,omitempty"`
	Epoch      uint64   `json:"epoch,omitempty"`
	Sessions   []string `json:"sessions,omitempty"`
}
type WorkerEndpoint struct {
	HTTP  string         `json:"http"`
	Media netip.AddrPort `json:"media"`
}
type ProcessParameters struct {
	Control   string                    `json:"control"`
	Relay     string                    `json:"relay"`
	Store     string                    `json:"store"`
	StorePort int                       `json:"storePort"`
	Workers   map[string]WorkerEndpoint `json:"workers"`
}

// StepResult holds product-owned values. Raw moves retain field omission and
// timestamp spelling for receipt publication and stale-history comparison.
type StepResult struct {
	Started, Ended time.Time
	Certainty      operator.Certainty
	Status         controlplane.Status
	Drain          controlplane.DrainResult
	Move           controlplane.MoveResult
	Inspection     observe.Inspection
	Local          mediaworker.WorkerStatus
	Telemetry      HostTelemetry
	Ownership      observe.Ownership
	LossKnown      bool
	OwnershipKnown bool
	ReadyMS        *int64
	Raw            json.RawMessage
	Moves, History []json.RawMessage
}
type StepFunc func(context.Context, string, string, time.Duration, ProcessExtra) (StepResult, error)
type ProcedurePorts struct {
	Clock  Clock
	Sleep  func(context.Context, time.Duration) error
	Step   StepFunc
	Broker GateBroker
	// Media returns the evidence owner's validated, sanitized summary.
	Media      func(context.Context) (*MediaSummary, error)
	Duplicates func() *uint64
	// Inventory must read all hosts and prove the selected release is running.
	Inventory func(context.Context, string, time.Duration) error
}
type ProcessConfig struct {
	Parameters ProcessParameters
	ArmedAt    time.Time
	Trials     bool
	Pool       bool
	Target     string
	Operation  string
	Mixed      bool
}
type ProcessEvidence struct {
	CompletedSteps            int     `json:"completedSteps"`
	WorkerRollouts            int     `json:"workerRollouts"`
	Restarts                  int     `json:"restarts"`
	ReleaseTargetsVerified    *int    `json:"releaseTargetsVerified,omitempty"`
	ControlReadyMS            *int64  `json:"controlReadyMs,omitempty"`
	ControlReadyNormalMS      *int64  `json:"controlReadyNormalMs,omitempty"`
	ControlReadyDegradedMS    *int64  `json:"controlReadyDegradedMs,omitempty"`
	ControlReadyTargetMet     *int    `json:"controlReadyTargetMet,omitempty"`
	RelayEpochDelta           *uint64 `json:"relayEpochDelta,omitempty"`
	RoutesRestored            *uint64 `json:"routesRestored,omitempty"`
	DuplicatePackets          *uint64 `json:"duplicatePackets,omitempty"`
	RelayWindows              string  `json:"relayWindows,omitempty"`
	RelayLocalTiming          string  `json:"relayLocalTiming,omitempty"`
	RelayConnectionInvariants string  `json:"relayConnectionInvariants,omitempty"`
}
type ProcedureResult struct {
	// Completed records procedure completion independently of strict media qualification.
	Completed    bool             `json:"-"`
	Status       operator.Verdict `json:"status"`
	Reason       string           `json:"reason,omitempty"`
	Category     string           `json:"category,omitempty"`
	Media        *MediaSummary    `json:"media,omitempty"`
	Process      ProcessEvidence  `json:"process"`
	AwaitingPool bool             `json:"-"`
}

// awaiting-pool is internal only; the stable wire verdict stays inconclusive.
func (r ProcedureResult) GateResult(g operator.Gate) operator.GateResult {
	return operator.GateResult{Gate: g, Status: r.Status, Reason: r.Reason}
}

type procedure struct {
	c        *ProcessConfig
	p        ProcedurePorts
	end      time.Duration
	down     []string
	stopped  []string
	frozen   bool
	unknown  bool
	evidence ProcessEvidence
}

func newProcedure(c *ProcessConfig, p ProcedurePorts, end time.Duration) *procedure {
	return &procedure{c: c, p: p, end: min(end, p.Clock.Elapsed()+GateMaximum, p.Clock.Elapsed()+c.ArmedAt.Add(SessionMaximum).Sub(p.Clock.Wall()))}
}

// ProcedureError is a classified internal outcome. Unclassified errors from
// callbacks never prove a definite failure and cannot authorize restoration.
type ProcedureError struct{ Code, Category string }

func (e *ProcedureError) Error() string { return e.Code }
func code(err error) string {
	if err == nil {
		return ""
	}
	var classified *ProcedureError
	if errors.As(err, &classified) {
		return classified.Code
	}
	return "command-result-unknown"
}
func fault(s string) error { return &ProcedureError{Code: s} }
func (g *procedure) step(ctx context.Context, op, slot string, end time.Duration, extra ProcessExtra) (StepResult, error) {
	if g.unknown {
		return StepResult{}, fault("command-result-unknown")
	}
	if g.p.Clock.Elapsed() >= g.end {
		return StepResult{}, fault("gate-deadline")
	}
	// Record ownership before a signal can land. Clear only on confirmed start.
	if (op == "worker-down" || op == "worker-stop") && extra.Worker != "" && !slices.Contains(g.down, extra.Worker) {
		g.down = append(g.down, extra.Worker)
	}
	if op == "freeze" || op == "signal-stop" {
		g.frozen = true
	}
	r, err := g.p.Step(ctx, op, slot, min(end, g.end), extra)
	if code(err) == "command-result-unknown" || r.Certainty == operator.Unknown {
		g.unknown = true
	}
	if err != nil {
		return r, err
	}
	g.evidence.CompletedSteps++
	if (op == "worker-stop" || op == "worker-down") && !slices.Contains(g.stopped, extra.Worker) {
		g.stopped = append(g.stopped, extra.Worker)
	}
	if op == "worker-start" {
		g.down = slices.DeleteFunc(g.down, func(w string) bool { return w == extra.Worker })
		g.stopped = slices.DeleteFunc(g.stopped, func(w string) bool { return w == extra.Worker })
	}
	if op == "unfreeze" || op == "signal-start" {
		g.frozen = false
	}
	if op == "status" && r.Status.LostCount > 0 {
		return r, fault("known-call-loss")
	}
	return r, nil
}
func (g *procedure) do(ctx context.Context, op, slot string, extra ProcessExtra) (StepResult, error) {
	return g.step(ctx, op, slot, g.end, extra)
}
func (g *procedure) status(ctx context.Context) (controlplane.Status, error) {
	r, err := g.do(ctx, "status", "shared", ProcessExtra{})
	return r.Status, err
}
func (g *procedure) wait(ctx context.Context, test func(controlplane.Status) bool) (controlplane.Status, error) {
	r, err := g.waitStep(ctx, func(r StepResult) bool { return test(r.Status) })
	return r.Status, err
}
func (g *procedure) waitStep(ctx context.Context, test func(StepResult) bool) (StepResult, error) {
	for g.p.Clock.Elapsed() < g.end {
		r, err := g.do(ctx, "status", "shared", ProcessExtra{})
		if err != nil {
			return r, err
		}
		if g.p.Clock.Elapsed() >= g.end {
			break
		}
		if test(r) {
			return r, nil
		}
		if err = g.p.Sleep(ctx, min(500*time.Millisecond, g.end-g.p.Clock.Elapsed())); err != nil {
			return r, err
		}
	}
	return StepResult{}, fault("gate-deadline")
}

func alive(s controlplane.Status, w string, workers map[string]WorkerEndpoint) bool {
	return slices.ContainsFunc(s.Workers, func(x controlplane.WorkerStatus) bool {
		return x.Name == w && !x.Dead && !x.Recovering && x.Address == workers[w].Media
	})
}
func settled(s controlplane.Status, w string) bool {
	return slices.ContainsFunc(s.Workers, func(x controlplane.WorkerStatus) bool { return x.Name == w && x.Dead && !x.Recovering && x.Calls == 0 })
}
func findCall(s controlplane.Status, id string) (controlplane.CallStatus, bool) {
	for _, c := range s.Calls {
		if c.ID == id {
			return c, true
		}
	}
	return controlplane.CallStatus{}, false
}
func other(w string) string {
	if w == "w1" {
		return "w2"
	}
	return "w1"
}
func validCall(c controlplane.CallStatus) bool {
	return c.ID != "" && (c.Owner == "w1" || c.Owner == "w2") && c.Epoch > 0 && c.Epoch <= maxSafeEpoch
}
func (g *procedure) identity(c controlplane.CallStatus, op string) Identity {
	return Identity{RunID: g.p.Broker.RunID(), OperationID: op, SessionID: c.ID, Worker: c.Owner, Epoch: c.Epoch}
}
func (g *procedure) arm(ctx context.Context, id Identity) error {
	return g.armIdentities(ctx, id.OperationID, []Identity{id})
}
func (g *procedure) armIdentities(ctx context.Context, op string, ids []Identity) error {
	err := g.p.Broker.Arm(ctx, op, ids)
	if code(err) == "command-result-unknown" {
		g.unknown = true
	}
	return err
}
func (g *procedure) feed(ctx context.Context, r Receipt) error {
	err := g.p.Broker.Feed(ctx, r.Identity.OperationID, []Receipt{r})
	if code(err) == "command-result-unknown" {
		g.unknown = true
	}
	return err
}
func (g *procedure) consent(ctx context.Context) error {
	if g.p.Clock.Elapsed()+ConsentWindow >= g.end {
		return fault("gate-deadline")
	}
	return g.p.Sleep(ctx, ConsentWindow)
}
func DrainReceipt(id Identity, r StepResult, m *controlplane.MoveResult, raw json.RawMessage) Receipt {
	receipt := Receipt{Identity: id, Kind: "move", Start: r.Started, End: r.Ended, Result: raw, Uncertain: true}
	if len(raw) == 0 {
		receipt.Result = json.RawMessage(`{}`)
	}
	if m != nil {
		if !m.Start.IsZero() {
			receipt.Start = m.Start
		}
		if !m.End.IsZero() {
			receipt.End = m.End
		}
		receipt.Uncertain = m.Error != "" || m.ID != id.SessionID || m.From != id.Worker || m.Kind != "move" || m.Start.IsZero() || m.End.IsZero()
	}
	return receipt
}
func (g *procedure) drain(ctx context.Context, w string) (StepResult, error) {
	r, err := g.do(ctx, "drain", "shared", ProcessExtra{Worker: w})
	if err != nil {
		return r, err
	}
	for _, m := range r.Drain.Moves {
		if m.Lost {
			return r, fault("known-call-loss")
		}
	}
	if r.Drain.Error != "" {
		return r, fault("unknown-drain")
	}
	return r, nil
}
func (g *procedure) emitDrain(ctx context.Context, id Identity, r StepResult, strict bool) error {
	var m *controlplane.MoveResult
	var raw json.RawMessage
	for i := range r.Drain.Moves {
		if r.Drain.Moves[i].ID == id.SessionID {
			m = &r.Drain.Moves[i]
			if i < len(r.Moves) {
				raw = r.Moves[i]
			}
			break
		}
	}
	receipt := DrainReceipt(id, r, m, raw)
	if err := g.feed(ctx, receipt); err != nil {
		return err
	}
	if strict && receipt.Uncertain {
		return fault("missing-drain-receipt")
	}
	if g.c.Trials || strict {
		return g.consent(ctx)
	}
	return nil
}
func (g *procedure) replace(ctx context.Context, w string) error {
	if _, err := g.wait(ctx, func(s controlplane.Status) bool { return settled(s, w) }); err != nil {
		return err
	}
	if _, err := g.do(ctx, "replace", "shared", ProcessExtra{Worker: w}); err != nil {
		return err
	}
	if _, err := g.do(ctx, "worker-start", w, ProcessExtra{Worker: w}); err != nil {
		return err
	}
	_, err := g.wait(ctx, func(s controlplane.Status) bool { return alive(s, w, g.c.Parameters.Workers) })
	return err
}

var categoryPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}(?::[a-z0-9-]{1,40})?$`)

func readFailure(err error) (string, string) {
	var classified *ProcedureError
	if errors.As(err, &classified) && classified.Category != "" && categoryPattern.MatchString(classified.Category) {
		return classified.Code, classified.Category
	}
	if strings.HasPrefix(code(err), "precondition-failed:") {
		category := strings.TrimPrefix(code(err), "precondition-failed:")
		if categoryPattern.MatchString(category) {
			return "precondition-failed", category
		}
		return "precondition-failed", ""
	}
	return "", ""
}
