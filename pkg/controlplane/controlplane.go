// Package controlplane coordinates moves and crash takeovers without changing
// the caller's connection. Run detects heartbeat timeouts and resumes dead
// workers' calls from stored snapshots with counter margins. Planned moves
// freeze the old worker, transfer a fenced lease,
// reroute the relay and resume the new worker from the final snapshot.
// A bounded relay hold and a private drain barrier bracket the sequence so
// caller packets wait in order instead of disappearing between owners.
// A per-call lock prevents overlapping moves; a failed move resumes the old
// worker after restoring ownership, even if reverse notification fails.
// Workers are reached through a small interface so a later process boundary
// can use the same coordination rules.
package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/relaishq/relais/internal/movediag"
	"github.com/relaishq/relais/internal/privateapi"
	"github.com/relaishq/relais/pkg/agent"
	agentv2 "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/metrics"
	"github.com/relaishq/relais/pkg/relay"
	"github.com/relaishq/relais/pkg/sessionstore"
)

var (
	// ErrMoveInProgress refuses overlapping coordination of the same call.
	ErrMoveInProgress = errors.New("controlplane: move in progress")
	// ErrUnknownCall means this control plane no longer tracks the call.
	ErrUnknownCall = errors.New("controlplane: unknown call")
	// ErrNoTarget means no non-draining destination is available.
	ErrNoTarget = errors.New("controlplane: no non-draining target worker")
	// ErrAtCapacity is a definite, retryable admission refusal.
	ErrAtCapacity = errors.New("controlplane: workers at capacity")
)

// Worker is the media boundary, local or remote. Resume must either adopt the
// supplied lease and run the session, or fail without running it.
type Worker interface {
	CreateSession(ctx context.Context, offer string) (string, string, error)
	EndSession(sessionID string) error
	ExportSession(sessionID string) ([]byte, error)
	ResumeSession(state []byte, opts mediaworker.ResumeOptions) (string, error)
}

// ReplayRecoveryWorker can request fresh video after a lossy relay replay.
// Targets without this capability retain ordinary frame-cache/PLI recovery.
type ReplayRecoveryWorker interface {
	RequestKeyframe(context.Context, string) error
}

// Relay is the trusted routing boundary, satisfied by *relay.Relay.
type Relay interface {
	HoldSession(ctx context.Context, sessionID string, from netip.AddrPort) error
	MoveSession(sessionID string, from, to netip.AddrPort) error
	ReleaseSession(sessionID string, to netip.AddrPort) (int, error)
	ForgetSession(sessionID string)
}

// TenureRelay identifies the transferred lease when a receiver address is reused.
// Older/fake routing implementations can keep the original Relay interface.
type TenureRelay interface {
	MoveSessionAtEpoch(string, netip.AddrPort, netip.AddrPort, uint64) error
}

// ExportReleaseRelay releases a planned hold to an owner that resumed from
// an exact, margin-zero export, replaying retained input that export did not
// consume (ADR 0009). *relay.Relay and RemoteRelay implement it.
type ExportReleaseRelay interface {
	ReleaseAfterExport(string, netip.AddrPort, map[uint32]relay.ConsumedInput) (relay.ReleaseReply, error)
}

// ExportMoveRelay moves a planned hold's route only after the old owner's
// output up to its export fence has been forwarded (ADR 0009).
type ExportMoveRelay interface {
	MoveSessionAfterExport(string, netip.AddrPort, netip.AddrPort, uint64) error
}

// moveAfterExport keeps the existing route call when the capability is absent.
// A retry whose previous route call failed no longer knows the routed source,
// so it cannot drain that source's output: it uses the existing route repair.
func moveAfterExport(r Relay, id string, from, to netip.AddrPort, epoch uint64) error {
	if mover, ok := r.(ExportMoveRelay); ok && from.IsValid() {
		return mover.MoveSessionAfterExport(id, from, to, epoch)
	}
	if tenure, ok := r.(TenureRelay); ok && epoch != 0 {
		return tenure.MoveSessionAtEpoch(id, from, to, epoch)
	}
	return r.MoveSession(id, from, to)
}

// releaseHold releases with the export's consumed input when both it and the
// relay capability are available, and otherwise exactly as before.
func releaseHold(r Relay, id string, to netip.AddrPort, input map[uint32]relay.ConsumedInput, result *mediaworker.HandoverResult) error {
	if exporter, ok := r.(ExportReleaseRelay); ok && input != nil {
		reply, err := exporter.ReleaseAfterExport(id, to, input)
		result.HeldPackets, result.RecoveredPackets, result.ReleaseLockHeld = reply.Packets, reply.Recovered, reply.LockHeld
		return err
	}
	held, err := r.ReleaseSession(id, to)
	result.HeldPackets = held
	return err
}

// ReplayRelay is the optional phase-2 caller-media cache capability. Both
// *relay.Relay and RemoteRelay implement it; phase-1/fake relays remain valid.
type ReplayRelay interface {
	BeginReplay(context.Context, string, netip.AddrPort, map[uint32]uint64) (relay.ReplayPlan, error)
	ReplaySession(context.Context, string, netip.AddrPort) (relay.ReplayResult, error)
}

type registration struct {
	createMu      sync.RWMutex // isolate create observation cuts to this worker
	name          string
	addr          netip.AddrPort
	worker        Worker
	observation   workerObservation
	reserved      int // incoming creates/moves; counted when balancing
	rollbackSlots int // source protection, used only by placement load
	retryingMoves bool
	pending       map[string]*takeoverState // unfinished transfers, retained for retry
}

type call struct {
	intent         *sessionstore.ControlIntent // guarded by mu
	terminal       bool                        // guarded by mu; terminal loss is accounted once
	lease          sessionstore.Lease          // guarded by stateMu; last observed placed tenure
	lossLease      sessionstore.Lease          // guarded by mu; immutable token for terminal cleanup
	cleanupPending bool                        // guarded by the plane registry lock
	cleanupLeases  []sessionstore.Lease        // guarded by mu; retained routed tenures
	cleanupLegacy  bool
	cleanupPlanned bool
	cleanupAfter   time.Time
	cleanupDelay   time.Duration
	// hungUp records a completed End; retained retries keep this call.
	hungUp atomic.Bool
	mu     sync.Mutex // serializes moves, takeovers, End and terminal reconciliation
	id     string
	// controlMu guards the routing holder and operator priority. Operators retain
	// the ordinary fail-fast rule against other operators, but wait for Control.
	controlMu      sync.Mutex
	controlActive  bool
	controlCancel  context.CancelFunc
	controlContext context.Context // immutable while the Control holder owns mu
	controlWaiters int
	// stateMu protects the completed-move metadata below. Never hold it
	// across I/O or while acquiring mu or the plane's registry lock.
	stateMu       sync.Mutex
	lastMove      *time.Time
	moveCount     uint64
	takeoverCount uint64
	lastMoveKind  string
}

// Plane owns the registry and call metadata. The store remains authoritative
// for ownership. New and Register must be called before accepting calls.
type Plane struct {
	restart         *restartState
	inventory       keyInventoryCache
	metrics         planeMetrics
	mu              sync.Mutex
	relay           Relay
	store           sessionstore.Store
	workers         map[string]*registration
	calls           map[string]*call
	ttl             time.Duration
	incomingChanged chan struct{}
	config          Config
	events          [recentTakeoverLimit]MoveResult
	eventNext       int
	eventCount      int
	lostCount       uint64
	lossesByCause   map[string]uint64
	running         bool
	loadVersion     uint64 // invalidates lease listings when reservations settle
	pool            *PoolSample
	poolFailed      bool
	pendingWorkers  map[string]bool
	bootstrap       bool
	relayReady      bool
	refusals        map[string]uint64
}

// New returns an empty control plane. Drive Run with the
// application lifetime context to enable automatic crash detection.
func New(r Relay, store sessionstore.Store) *Plane { return NewWithConfig(r, store, Config{}) }

// NewWithConfig changes the death-detection intervals and takeover bounds.
// Run must be driven with an application-owned lifetime context.
func NewWithConfig(r Relay, store sessionstore.Store, config Config) *Plane {
	config = config.defaults()
	return &Plane{
		relay:           r,
		config:          config,
		store:           store,
		workers:         make(map[string]*registration),
		calls:           make(map[string]*call),
		lossesByCause:   make(map[string]uint64),
		pendingWorkers:  make(map[string]bool),
		refusals:        map[string]uint64{"no_target": 0, "at_capacity": 0},
		ttl:             config.LeaseTTL,
		incomingChanged: make(chan struct{}),
	}
}

// Register names a worker by its unique private relay-leg address.
func (p *Plane) Register(name string, addr netip.AddrPort, worker Worker) error {
	if name == "" || !addr.IsValid() || worker == nil {
		return errors.New("controlplane: worker needs name, address and interface")
	}

	p.mu.Lock()

	if _, ok := p.workers[name]; ok {
		p.mu.Unlock()
		return errors.New("controlplane: duplicate worker name")
	}

	for _, w := range p.workers {
		if w.addr == addr {
			p.mu.Unlock()
			return errors.New("controlplane: duplicate worker address")
		}
	}

	p.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	err := p.saveRegistration(ctx, name, addr, worker)
	cancel()
	if err != nil {
		return err
	}
	p.mu.Lock()
	if _, exists := p.workers[name]; exists {
		p.mu.Unlock()
		return errors.New("controlplane: duplicate worker name")
	}
	for _, w := range p.workers {
		if w.addr == addr {
			p.mu.Unlock()
			return errors.New("controlplane: duplicate worker address")
		}
	}
	p.workers[name] = &registration{name: name, addr: addr, worker: worker, observation: newWorkerObservation(time.Now(), p.bootstrap)}
	delete(p.pendingWorkers, name)
	p.loadVersion++
	p.mu.Unlock()
	if p.restart != nil {
		select {
		case p.restart.authority <- struct{}{}:
		default:
		}
	}
	if w, ok := worker.(interface{ StartHeartbeats(mediaworker.Heartbeats) }); ok {
		w.StartHeartbeats(p)
	}
	return nil
}

// SetRelay replaces the relay after a harness restart. In-flight moves keep
// their routing boundary; the caller of SetRelay must serialize the restart.
func (p *Plane) SetRelay(r Relay) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.relay = r
}

// pick reserves capacity under mu, but never holds mu across store I/O.
func (p *Plane) pick(ctx context.Context, name string, exclude netip.AddrPort) (*registration, error) {
	return p.pickExcluding(ctx, name, map[netip.AddrPort]bool{exclude: true})
}

func (p *Plane) pickExcluding(ctx context.Context, name string, excluded map[netip.AddrPort]bool) (*registration, error) {
	return p.pickCapacity(ctx, name, excluded, false, "")
}

// A lease may appear and its reservation disappear while ListByWorker is in
// flight. Re-read after settlement; otherwise an old list can admit too much.
// No registry lock spans store I/O. Retries have a fixed bound.
func (p *Plane) pickCapacity(ctx context.Context, name string, excluded map[netip.AddrPort]bool, overflow bool, existingID string) (*registration, error) {
	for range 32 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		p.mu.Lock()
		version := p.loadVersion
		candidates := make([]*registration, 0, len(p.workers))
		for _, w := range p.workers {
			if (name == "" || w.name == name) && w.observation.knowledge().eligible && !excluded[w.addr] {
				candidates = append(candidates, w)
			}
		}
		p.mu.Unlock()
		loads := make(map[*registration]int, len(candidates))
		for _, w := range candidates {
			leases, err := p.store.ListByWorker(ctx, w.addr)
			if err != nil {
				return nil, err
			}
			loads[w] = len(leases)
			// Recovery on the already-owned tenure consumes the same slot. The
			// reservation represents that call until resume finishes.
			for _, lease := range leases {
				if p.config.WorkerCallCap > 0 && lease.SessionID == existingID {
					loads[w]--
					break
				}
			}
		}
		p.mu.Lock()
		if err := ctx.Err(); err != nil {
			p.mu.Unlock()
			return nil, err
		}
		if p.config.WorkerCallCap > 0 && version != p.loadVersion {
			p.mu.Unlock()
			continue
		}
		var best *registration
		load, ready := int(^uint(0)>>1), false
		for _, w := range candidates {
			if p.workers[w.name] != w || !w.observation.knowledge().eligible {
				continue
			}
			ready = true
			n := loads[w] + w.reserved + w.rollbackSlots
			if !overflow && p.config.WorkerCallCap > 0 && n >= p.config.WorkerCallCap {
				continue
			}
			if n < load || n == load && (best == nil || w.name < best.name) {
				best, load = w, n
			}
		}
		if best == nil {
			p.mu.Unlock()
			if ready {
				return nil, ErrAtCapacity
			}
			return nil, ErrNoTarget
		}
		best.reserved++
		p.loadVersion++
		p.mu.Unlock()
		return best, nil
	}
	return nil, fmt.Errorf("controlplane: placement changed during sampling: %w", sessionstore.ErrTransient)
}

func (p *Plane) unreserve(w *registration) {
	p.mu.Lock()
	defer p.mu.Unlock()

	w.reserved--
	p.loadVersion++
	close(p.incomingChanged)
	p.incomingChanged = make(chan struct{})
}

func (p *Plane) forget(c *call) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.calls[c.id] == c {
		delete(p.calls, c.id)
	}
}

func (p *Plane) lookup(id string) (*call, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	c := p.calls[id]
	if c == nil {
		return nil, ErrUnknownCall
	}

	return c, nil
}

func (p *Plane) owner(addr netip.AddrPort) *registration {
	for _, w := range p.workers {
		if w.addr == addr {
			return w
		}
	}

	return nil
}

// Create starts a call on the least-loaded non-draining worker. name can
// pin a worker for a test, but cannot bypass draining.
func (p *Plane) Create(ctx context.Context, offer, name string) (idResult, answerResult string, createErr error) {
	if err := p.requireMutationReady(); err != nil {
		return "", "", err
	}
	defer func() {
		if createErr != nil {
			p.metrics.callErrors.Add(1)
			p.mu.Lock()
			if errors.Is(createErr, ErrAtCapacity) {
				p.refusals["at_capacity"]++
			} else if errors.Is(createErr, ErrNoTarget) {
				p.refusals["no_target"]++
			}
			p.mu.Unlock()
		} else {
			p.metrics.calls.Add(1)
		}
	}()
	p.mu.Lock()
	attempts := len(p.workers)
	ready := p.readyLocked()
	p.mu.Unlock()
	if !ready {
		return "", "", ErrNoTarget
	}
	excluded := make(map[netip.AddrPort]bool, attempts)
	var w *registration
	var id, answer string
	var placed sessionstore.Lease
	for range attempts {
		var err error
		w, err = p.pickExcluding(ctx, name, excluded)
		if err != nil {
			return "", "", err
		}
		createKey, recordErr := p.startCreate(ctx, w)
		if recordErr != nil {
			p.unreserve(w)
			return "", "", recordErr
		}
		placed = sessionstore.Lease{}
		if worker, ok := w.worker.(interface {
			CreateSessionWithLease(context.Context, string) (string, string, sessionstore.Lease, error)
		}); ok {
			id, answer, placed, err = worker.CreateSessionWithLease(ctx, offer)
		} else {
			id, answer, err = w.worker.CreateSession(ctx, offer)
		}
		if finishErr := p.finishCreate(ctx, createKey, w, id, err); finishErr != nil {
			return "", "", finishErr
		}
		if err == nil {
			break
		}
		p.unreserve(w)
		// Only the worker's definite draining refusal proves nothing was
		// accepted. Never replay uncertain, canceled, timed-out or unknown
		// create outcomes, even if they also wrap the draining sentinel.
		if !errors.Is(err, mediaworker.ErrDraining) || errors.Is(err, privateapi.ErrUncertain) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return "", "", err
		}
		excluded[w.addr] = true
		name = "" // A raced pinned create may use a different live worker.
		w = nil
	}
	if w == nil {
		return "", "", ErrNoTarget
	}
	defer p.unreserve(w)

	p.mu.Lock()
	if placed.Epoch == 0 {
		placed = sessionstore.Lease{SessionID: id, Worker: w.addr}
	}
	c := &call{id: id, lease: placed}
	p.calls[id] = c
	p.mu.Unlock()
	if placed.Epoch == 0 {
		if lease, err := p.store.Get(ctx, id); err == nil {
			c.rememberLease(lease)
		}
	}
	return id, answer, nil
}

// End waits for a move already in progress, then hangs up the current owner.
func (p *Plane) End(ctx context.Context, id string) error {
	if err := p.requireMutationReady(); err != nil {
		return err
	}
	return p.end(ctx, id)
}
func (p *Plane) end(ctx context.Context, id string) (endErr error) {
	c, err := p.lookup(id)
	if err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.terminal {
		if c.hungUp.Load() {
			return nil
		}
		return sessionstore.ErrNotFound
	}

	lease, err := p.store.Get(ctx, id)
	if err != nil {
		if errors.Is(err, sessionstore.ErrEnded) {
			p.expiredLocked(ctx, c)
			return nil
		}
		if errors.Is(err, sessionstore.ErrNotFound) {
			p.expiredLocked(ctx, c)
		}

		return err
	}

	p.mu.Lock()
	w := p.owner(lease.Worker)
	p.mu.Unlock()
	if w == nil {
		return ErrUnknownCall
	}

	if err := p.beginIntent(ctx, c, "end", lease); err != nil {
		return err
	}
	defer func() {
		if endErr == nil {
			p.finishIntent(c)
		}
	}()
	if err := w.worker.EndSession(id); err != nil {
		if errors.Is(err, mediaworker.ErrUnknownSession) {
			// Fencing may remove ownership after End's first read. A worker's
			// missing session does not prove that an explicit hangup ended it.
			current, readErr := p.store.Get(ctx, id)
			if errors.Is(readErr, sessionstore.ErrEnded) {
				p.expiredLocked(ctx, c)
				return nil
			}
			if errors.Is(readErr, sessionstore.ErrNotFound) {
				p.expiredLocked(ctx, c)
				return err
			}
			if readErr != nil {
				return readErr
			}
			if current.Worker != lease.Worker || current.Epoch != lease.Epoch {
				c.rememberLease(current)
				return sessionstore.ErrLeaseLost
			}
			if p.store.Release(ctx, lease) == nil {
				// Release is a successful no-op for a stale or expired token. A
				// committed marker, not a nil reply alone, proves a normal end.
				_, releaseReadErr := p.store.Get(ctx, id)
				if errors.Is(releaseReadErr, sessionstore.ErrEnded) {
					c.hungUp.Store(true)
					p.forget(c)
					p.deleteFrames(id)
				} else if errors.Is(releaseReadErr, sessionstore.ErrNotFound) {
					p.expiredLocked(ctx, c)
				}
			}
		}

		return err
	}
	// EndSession closes the worker even when its best-effort store release
	// fails. Commit the explicit hangup ourselves with the retained fenced token;
	// a stale release cannot remove a successor's ownership.
	releaseErr := p.store.Release(ctx, lease)
	current, readErr := p.store.Get(ctx, id)
	if errors.Is(readErr, sessionstore.ErrEnded) {
		c.hungUp.Store(true)
		p.forget(c)
		p.deleteFrames(id)
		return nil
	}
	if errors.Is(readErr, sessionstore.ErrNotFound) {
		p.expiredLocked(ctx, c)
		return readErr
	}
	if readErr != nil {
		return readErr
	}
	c.rememberLease(current)
	if !sameTenure(current, lease) {
		return sessionstore.ErrLeaseLost
	}
	if releaseErr != nil {
		return releaseErr
	}
	return fmt.Errorf("controlplane: end release unconfirmed: %w", sessionstore.ErrTransient)
}

// MoveResult records coordination timings; the harness measures media gaps
// separately at the caller.
type MoveResult struct {
	SnapshotAge        time.Duration               `json:"snapshot_age,omitempty"`
	CheckpointStoredAt time.Time                   `json:"checkpoint_stored_at"`
	CheckpointAge      time.Duration               `json:"checkpoint_age,omitempty"`
	Checkpoint         mediaworker.CheckpointState `json:"checkpoint"`
	CheckpointPolicy   string                      `json:"checkpoint_policy,omitempty"`
	SequenceMargin     uint16                      `json:"sequence_margin,omitempty"`
	SRTCPIndexMargin   uint32                      `json:"srtcp_index_margin,omitempty"`
	Kind               string                      `json:"kind"`
	DetectedAt         time.Time                   `json:"detected_at,omitempty"`
	LastHeartbeat      time.Time                   `json:"last_heartbeat,omitempty"`
	LossCause          string                      `json:"loss_cause,omitempty"`
	Lost               bool                        `json:"lost,omitempty"`
	ID                 string                      `json:"id"`
	From               string                      `json:"from"`
	To                 string                      `json:"to"`
	Start              time.Time                   `json:"start"`
	End                time.Time                   `json:"end"`
	Result             mediaworker.HandoverResult  `json:"result"`
	// Error is empty on success, otherwise the per-call drain/move failure.
	Error string `json:"error,omitempty"`
}

// Move waits within a bounded budget for Control and fails fast against
// another move or drain. Empty to picks
// the least-loaded non-draining worker other than the current owner.
func (p *Plane) Move(ctx context.Context, id, to string) (res MoveResult, moveErr error) {
	if err := p.requireMutationReady(); err != nil {
		return res, err
	}
	if !movediag.Enabled() {
		return p.moveRequest(ctx, id, to)
	}
	ctx = movediag.Begin(ctx)
	event := movediag.NewEvent(ctx, "move", "planned_move")
	defer func() { res.Result.MoveDiagnosticID = movediag.ID(ctx); movediag.Default.Finish(event, moveErr != nil) }()
	return p.moveRequest(ctx, id, to)
}

func (p *Plane) moveRequest(ctx context.Context, id, to string) (MoveResult, error) {
	c, err := p.lookup(id)
	if err != nil {
		return MoveResult{}, err
	}

	if err := c.lockOperator(ctx, false); err != nil {
		return MoveResult{}, err
	}
	defer c.mu.Unlock()
	if c.terminal {
		return MoveResult{}, ErrUnknownCall
	}

	lease, err := p.store.Get(ctx, id)
	if err != nil {
		return MoveResult{}, err
	}

	target, err := p.pick(ctx, to, lease.Worker)
	if err != nil {
		return MoveResult{}, err
	}
	defer p.unreserve(target)

	return p.move(ctx, c, lease, target)
}

// move holds the call lock and an incoming reservation on target.
func (p *Plane) move(ctx context.Context, c *call, lease sessionstore.Lease, target *registration) (res MoveResult, err error) {
	moved, recoveryMetrics := false, false
	defer func() {
		if recoveryMetrics {
			return // Pending recovery owns the eventual metric outcome.
		}
		if err != nil {
			p.metrics.moveErrors.Add(1)
		} else if moved {
			p.metrics.moves.Add(1)
		}
	}()
	p.mu.Lock()
	source, r := p.owner(lease.Worker), p.relay
	// Keep a slot for synchronous rollback after Transfer releases the source
	// lease. Otherwise a concurrent create could fill it before rollback.
	rollbackReserved := source != nil && p.config.WorkerCallCap > 0
	if rollbackReserved {
		source.rollbackSlots++
		p.loadVersion++
	}
	p.mu.Unlock()
	if rollbackReserved {
		defer func() {
			p.mu.Lock()
			source.rollbackSlots--
			p.loadVersion++
			p.mu.Unlock()
		}()
	}
	if source == nil {
		return res, ErrUnknownCall
	}

	if err = p.beginIntent(ctx, c, "move", lease); err != nil {
		return res, err
	}
	defer p.finishIntent(c)
	res = MoveResult{Kind: "move", ID: c.id, From: source.name, To: target.name, Start: time.Now(), Result: mediaworker.HandoverResult{SessionID: c.id}}
	defer func() {
		res.End = time.Now()
		res.Result.Duration = res.End.Sub(res.Start)
		if err != nil {
			res.Error = err.Error()
		}
	}()
	started := time.Now()
	// Missing acknowledgement aborts before export; the relay replays to A.
	if err = diagnosticRelay(r, movediag.Phase(ctx, "barrier")).HoldSession(movediag.Phase(ctx, "barrier"), c.id, source.addr); err != nil {
		if errors.Is(err, privateapi.ErrUncertain) {
			_, _ = r.ReleaseSession(c.id, source.addr)
		}
		return res, err
	}

	res.Result.Drain = time.Since(started)
	// Successful/rolled-back resume releases to its owner. An uncertain final
	// export retains the hold until adoption, bounded by the relay backstop.
	// exported is set only while the releasing owner resumed from the exact,
	// margin-zero export: the hold then also replays input it never consumed.
	releaseTo := source.addr
	released := false
	var exported map[uint32]relay.ConsumedInput
	defer func() {
		if released {
			return
		}
		p.mu.Lock()
		pending := source.pending[c.id]
		retained := pending != nil
		lost := p.calls[c.id] != c
		p.mu.Unlock()
		if lost {
			// Terminal cleanup already released the hold and forgot the call.
			res.Lost = true
			return
		}
		if retained {
			return
		}
		releaseErr := releaseHold(diagnosticRelay(r, movediag.Phase(ctx, "release")), c.id, releaseTo, exported, &res.Result)
		if errors.Is(releaseErr, relay.ErrHoldExpired) {
			res.Result.HoldExpired = true
			if err == nil {
				// Resume completed; expiry affected packet holding, not ownership.
				// The relay already counted it in HoldTimeouts. Do not invite a retry.
				return
			}
		}
		if releaseErr != nil {
			err = errors.Join(err, fmt.Errorf("controlplane: release caller hold: %w", releaseErr))
		}
	}()
	started = time.Now()
	state, err := diagnosticWorker(source.worker, movediag.Phase(ctx, "export")).ExportSession(c.id)
	res.Result.Export = time.Since(started)
	if err != nil {
		// A generic/transport error cannot prove export did not flush A.
		// Bump its epoch either way and recover the latest store snapshot.
		if uncertainExport(err) {
			recoveryMetrics = true
			p.mu.Lock()
			if source.pending == nil {
				source.pending = make(map[string]*takeoverState)
			}
			source.pending[c.id] = &takeoverState{call: c, lease: lease, routed: source.addr, routedLease: lease,
				excluded: map[netip.AddrPort]bool{source.addr: true}, attemptLimit: maxResumeAttempts, held: true, outageStarted: res.Start}
			p.mu.Unlock()
			recovery, cancel := context.WithTimeout(context.Background(), takeoverBudget)
			defer cancel()
			outcome := p.takeoverLocked(recovery, source, c, lease, res.Start)
			p.mu.Lock()
			pending := source.pending[c.id]
			p.mu.Unlock()
			if outcome != nil {
				res = *outcome
				if !res.Lost {
					releaseTo = target.addr
					err = nil
				} else {
					err = errors.New(res.Error)
				}
			}
			if pending != nil {
				return res, fmt.Errorf("controlplane: export uncertain; recovery pending: %w", err)
			}
			if outcome == nil {
				// A concurrent transfer ended recovery without a terminal outcome.
				// Let move's defer count the failed coordination attempt.
				recoveryMetrics = false
			}
			released = true
			return res, err
		}
		if errors.Is(err, mediaworker.ErrUnknownSession) {
			// A hard-killed source has no memory but can still own a live
			// lease and snapshot. Preserve that record for crash recovery.
			current, getErr := p.store.Get(ctx, c.id)
			if errors.Is(getErr, sessionstore.ErrEnded) {
				p.forget(c)
			} else if errors.Is(getErr, sessionstore.ErrNotFound) {
				c.rememberLease(lease)
				p.expiredLocked(ctx, c)
				res.Lost, res.LossCause = true, "ownership_expired"
			} else if getErr == nil &&
				(current.Worker != lease.Worker || current.Epoch != lease.Epoch) {
				p.forget(c)
			}
		}

		return res, err
	}

	res.Result.StateBytes = len(state)
	// A malformed copy keeps the plain release; resume would reject it anyway.
	exported, _ = mediaworker.SnapshotConsumedInput(state)
	transferred, err := p.transfer(movediag.Phase(ctx, "transfer"), lease, target.addr)
	if err != nil {
		if p.retainUncertainMove(source, c, lease, source.addr, state, err, res.Start) {
			return res, err
		}
		err = p.rollback(c, source, target, r, state, lease, false, false, false, &res.Result, err, res.Start)
		return res, err
	}

	if err = moveAfterExport(diagnosticRelay(r, movediag.Phase(ctx, "transfer")), c.id, source.addr, target.addr, 0); err != nil {
		err = p.rollback(c, source, target, r, state, transferred, true, true, false, &res.Result, err, res.Start)
		return res, err
	}

	started = time.Now()
	opts := mediaworker.ResumeOptions{Kind: agent.PlannedMove, Lease: transferred}
	if movediag.ID(ctx) != "" {
		opts.Context = movediag.Copy(context.Background(), movediag.Phase(ctx, "resume"))
	}
	_, err = target.worker.ResumeSession(state, opts)
	res.Result.Resume = time.Since(started)
	if err != nil {
		// Explicit transport uncertainty can hide a committed adoption, even
		// with an agent cause. Otherwise typed callback failures are definitive.
		agentFailure := errors.Is(err, agentv2.ErrIncompatibleAgent) || errors.Is(err, agent.ErrRestore) || errors.Is(err, agent.ErrVersion) || errors.Is(err, agent.ErrStateTooLarge) || errors.Is(err, agent.ErrSave)
		uncertainResume := errors.Is(err, privateapi.ErrUncertain) || (!agentFailure && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)))
		if uncertainResume {
			p.metrics.uncertainMoveResumes.Add(1)
			// The source resumes from a store checkpoint with crash margins.
			// That copy may predate the export, so keep the plain release.
			exported = nil
		}
		err = p.rollback(c, source, target, r, state, transferred, true, true, uncertainResume, &res.Result, err, res.Start)
		return res, err
	}

	releaseTo = target.addr
	now := time.Now()
	c.stateMu.Lock()
	c.lastMove = &now
	c.moveCount++
	c.lastMoveKind = "move"
	c.stateMu.Unlock()
	moved = true
	return res, nil
}

func (p *Plane) rollback(c *call, source, target *registration, r Relay, state []byte, lease sessionstore.Lease, transferred, rerouted, uncertainResume bool, result *mediaworker.HandoverResult, cause error, moveStarted time.Time) error {
	// Recovery uses a fresh context after the request has flushed its source.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	res := MoveResult{Kind: "move", ID: c.id, From: source.name, To: target.name, Start: moveStarted}
	var err error
	checkpointOutside := false
	if transferred {
		from := lease
		lease, err = p.transfer(ctx, lease, source.addr)
		routed := source.addr
		if rerouted {
			routed = netip.AddrPort{}
		}
		if p.retainUncertainMove(source, c, from, routed, state, err, moveStarted) {
			p.mu.Lock()
			source.pending[c.id].crashMargins = uncertainResume
			p.mu.Unlock()
			return err
		}
	}
	var routeErr error
	if err == nil && rerouted {
		routeErr = r.MoveSession(c.id, target.addr, source.addr)
	}
	// A failed reverse notification must not prevent A from running again.
	// B never resumed, and the next authenticated consent check follows A's
	// restored store ownership even if the relay itself needs to recover.
	if err == nil {
		resumeCtx := ctx
		if uncertainResume {
			var resumeCancel context.CancelFunc
			resumeCtx, resumeCancel = context.WithTimeout(ctx, takeoverBudget)
			defer resumeCancel()
		}
		opts := mediaworker.ResumeOptions{Kind: agent.PlannedMove, Lease: lease, Context: resumeCtx}
		if uncertainResume {
			// B may already have emitted media and persisted adjusted counters.
			// Its fenced store state, rather than the final export from A, is now
			// the only valid checkpoint for a rollback that needs crash margins.
			var tupleMetadata *sessionstore.Checkpoint
			state, tupleMetadata, err = mediaworker.ReadRecoverySnapshot(ctx, p.store, c.id)
			if err == nil {
				var decision checkpointDecision
				decision, err = p.checkpointDecision(resumeCtx, c.id, state, checkpointOutageBudget(resumeCtx, moveStarted), tupleMetadata)
				res.CheckpointAge, res.SnapshotAge, res.CheckpointStoredAt = decision.age, decision.snapshotAge, decision.storedAt
				res.Checkpoint, res.SequenceMargin, res.SRTCPIndexMargin = decision.info, decision.margin, decision.rtcpMargin
				checkpointOutside = decision.outside
				opts.Kind = agent.Takeover
				opts.SequenceMargin, opts.SRTCPIndexMargin = decision.margin, decision.rtcpMargin
				opts.CallerSequenceReserve = decision.reserve
				opts.CheckpointAge, opts.SnapshotAge, opts.CheckpointStoredAt = decision.age, decision.snapshotAge, decision.storedAt
			}
		}
		if err == nil {
			_, err = source.worker.ResumeSession(state, opts)
		}
	}
	if errors.Is(err, mediaworker.ErrMoveBudget) && !errors.Is(err, privateapi.ErrUncertain) {
		// Both targets refused before adoption. Keep the exact export and hold;
		// the bounded recovery loop can retry either live worker or an alternate.
		p.mu.Lock()
		if source.pending == nil {
			source.pending = make(map[string]*takeoverState)
		}
		excluded := make(map[netip.AddrPort]bool)
		if source.observation.knowledge().dead {
			excluded[source.addr] = true
		}
		source.pending[c.id] = &takeoverState{call: c, lease: lease, routed: source.addr, routedLease: lease,
			planned: true, plannedState: state, crashMargins: uncertainResume, held: true, excluded: excluded,
			attemptLimit: maxResumeAttempts, outageStarted: moveStarted, budgetRollback: true, budgetStarted: time.Now(),
			budgetExcluded: map[netip.AddrPort]bool{source.addr: true, target.addr: true}}
		p.mu.Unlock()
		return fmt.Errorf("controlplane: rollback awaiting move budget: %w", err)
	}
	if checkpointOutside && (err == nil || checkpointEnvelopeLoss(err)) {
		res.CheckpointPolicy = "scaled"
		if err != nil {
			res.CheckpointPolicy = "definitive-loss"
		}
		metrics.CheckpointEnvelopeEvents.WithLabelValues(res.CheckpointPolicy).Inc()
	}
	if err != nil {
		loss := fmt.Errorf("controlplane: call lost: move failed (%v), rollback failed: %w", cause, err)
		p.completeTakeover(source, c, lease, &res, true, loss)
		return loss
	}

	result.RolledBack = true
	if routeErr != nil {
		return fmt.Errorf("controlplane: rolled back on source; relay restore failed (%v): %w", routeErr, cause)
	}

	return fmt.Errorf("controlplane: move failed, rolled back: %w", cause)
}

// Keep Drain's historical HTTP 500 for an unknown worker while retaining a
// typed not-found cause and a worker-specific diagnostic. The private mark
// endpoint explicitly returns 404 for this same lookup failure.
type unknownWorkerError struct{ error }

func (e *unknownWorkerError) Unwrap() error { return e.error }

// MarkDraining is an idempotent placement fence. It does not wait for
// reservations, acquire call locks or drain slots, or move any sessions.
func (p *Plane) MarkDraining(name string) error {
	if err := p.requireMutationReady(); err != nil {
		return err
	}
	_, err := p.markDraining(name)
	return err
}

func (p *Plane) markDraining(name string) (*registration, error) {
	if p.restart != nil {
		p.mu.Lock()
		exists := p.workers[name] != nil
		p.mu.Unlock()
		if !exists {
			return nil, &unknownWorkerError{fmt.Errorf("controlplane: unknown worker %q: %w", name, sessionstore.ErrNotFound)}
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := p.controlRecord(ctx, "drain:"+name, true); err != nil {
			return nil, err
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	w := p.workers[name]
	if w == nil {
		return nil, &unknownWorkerError{fmt.Errorf("controlplane: unknown worker %q: %w", name, sessionstore.ErrNotFound)}
	}
	w.observation.markDraining()
	return w, nil
}

// Drain blocks new selections immediately. Existing calls move concurrently,
// at most drainParallelism at a time: a call waiting its turn keeps flowing
// on the old worker, so running more moves than there are CPUs would only
// lengthen every held call's gap. Reservations keep concurrent selections
// balanced. With no destination it
// moves nothing. It marks draining before waiting up to one second for
// incoming reservations, then includes those calls in the drain.
func (p *Plane) Drain(ctx context.Context, name string) (moves []MoveResult, drainErr error) {
	if err := p.requireMutationReady(); err != nil {
		return nil, err
	}
	defer func() {
		if drainErr != nil {
			p.metrics.drainErrors.Add(1)
		} else {
			p.metrics.drains.Add(1)
		}
	}()
	w, err := p.markDraining(name)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()

	for w.reserved != 0 {
		changed := p.incomingChanged
		p.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
			return nil, errors.New("controlplane: draining; incoming work did not finish within one second")
		}

		p.mu.Lock()
	}

	p.mu.Unlock()
	leases, err := p.store.ListByWorker(ctx, w.addr)
	if err != nil {
		return nil, err
	}
	type job struct {
		lease  sessionstore.Lease
		target *registration
		c      *call
		err    error
	}

	jobs := make([]job, 0, len(leases))
	for _, lease := range leases {
		p.mu.Lock()
		c := p.calls[lease.SessionID]
		p.mu.Unlock()
		// Do not promise a second destination slot for a call whose operator
		// already owns coordination. Keep its typed partial outcome while
		// leaving capacity available to the other calls in this drain.
		if p.config.WorkerCallCap > 0 && c != nil && c.operatorInProgress() {
			jobs = append(jobs, job{lease: lease, c: c, err: ErrMoveInProgress})
			continue
		}
		target, pickErr := p.pick(ctx, "", w.addr)
		if pickErr != nil {
			for _, j := range jobs {
				if j.target != nil {
					p.unreserve(j.target)
				}
			}
			return nil, pickErr
		}

		jobs = append(jobs, job{lease: lease, target: target, c: c})
	}
	results := make([]MoveResult, len(jobs))
	errs := make([]error, len(jobs))
	var wg sync.WaitGroup
	sem := make(chan struct{}, drainParallelism())
	for i, j := range jobs {
		wg.Go(func() {
			results[i] = MoveResult{ID: j.lease.SessionID, From: w.name}
			if j.target != nil {
				defer p.unreserve(j.target)
				results[i].To = j.target.name
			}
			defer func() {
				if errs[i] != nil {
					results[i].Error = errs[i].Error()
				}
			}()
			if j.err != nil {
				errs[i] = j.err
				return
			}
			sem <- struct{}{}
			defer func() { <-sem }()

			if j.c == nil {
				errs[i] = ErrUnknownCall
				return
			}

			if err := j.c.lockOperator(ctx, false); err != nil {
				errs[i] = err
				return
			}
			defer j.c.mu.Unlock()

			// Re-read after taking the move lock: a completed move may have changed
			// the owner since Drain listed the leases.
			current, err := p.store.Get(ctx, j.lease.SessionID)
			if err != nil {
				errs[i] = err
				return
			}
			if current.Worker != w.addr || current.Epoch != j.lease.Epoch {
				errs[i] = sessionstore.ErrLeaseLost
				return
			}

			results[i], errs[i] = p.move(ctx, j.c, current, j.target)
		})
	}
	wg.Wait()
	return results, errors.Join(errs...)
}

// WorkerStatus describes one registered worker and its live lease count.
type WorkerStatus struct {
	Name            string         `json:"name"`
	Address         netip.AddrPort `json:"address"`
	Draining        bool           `json:"draining"`
	Recovering      bool           `json:"recovering"`
	Dead            bool           `json:"dead"`
	LivenessUnknown bool           `json:"liveness_unknown,omitempty"`
	LastHeartbeat   time.Time      `json:"last_heartbeat"`
	Calls           int            `json:"call_count"`
}

// CallStatus describes current lease ownership and successful moves.
type CallStatus struct {
	ID            string         `json:"id"`
	Owner         string         `json:"owner"`
	Address       netip.AddrPort `json:"owner_address"`
	Epoch         uint64         `json:"lease_epoch"`
	LastMove      *time.Time     `json:"last_move"`
	MoveCount     uint64         `json:"move_count"`
	TakeoverCount uint64         `json:"takeover_count"`
	LastMoveKind  string         `json:"last_move_kind"`
}

// Status is the HTTP view of workers and live calls.
type Status struct {
	Rebuild          *RebuildStatus     `json:"rebuild,omitempty"`
	Workers          []WorkerStatus     `json:"workers"`
	Calls            []CallStatus       `json:"calls"`
	Takeovers        []MoveResult       `json:"takeovers"`
	LostCount        uint64             `json:"lost_count"`
	Ready            bool               `json:"ready"`
	Pool             PoolSample         `json:"pool"`
	SessionStoreKeys KeyInventoryStatus `json:"session_store_keys"`
}

// Status lists live calls and reconciles definitive ownership loss.
func (p *Plane) Status(ctx context.Context) (Status, error) {
	inventory := p.keyInventoryStatus(ctx)
	_ = p.SamplePool(ctx)
	p.mu.Lock()
	workers := make([]*registration, 0, len(p.workers))
	for _, w := range p.workers {
		workers = append(workers, w)
	}
	// Copy registry flags here; metadata uses its own brief lock below.
	status := Status{Rebuild: p.RebuildStatus(), SessionStoreKeys: inventory, Workers: []WorkerStatus{}, Calls: []CallStatus{}, Takeovers: p.recentTakeovers(), LostCount: p.lostCount}
	for _, w := range workers {
		knowledge := w.observation.knowledge()
		status.Workers = append(status.Workers, WorkerStatus{Name: w.name, Address: w.addr, Draining: knowledge.draining, Dead: knowledge.dead, LivenessUnknown: knowledge.livenessUnknown, Recovering: knowledge.recovering, LastHeartbeat: knowledge.lastHeartbeat})
	}

	calls := make([]*call, 0, len(p.calls))
	for _, c := range p.calls {
		calls = append(calls, c)
	}
	p.mu.Unlock()
	for _, c := range calls {
		p.mu.Lock()
		pendingCleanup := c.cleanupPending
		p.mu.Unlock()
		if pendingCleanup {
			continue
		}
		// Sample completed-move metadata before reading authoritative ownership.
		// An in-flight transfer may advance the lease before its move completes;
		// the next poll will include that move's metadata. Live polling never takes
		// the coordination lock, so it cannot reject a real move. Missing
		// ownership uses TryLock and an authoritative recheck for termination.
		c.stateMu.Lock()
		cs := CallStatus{ID: c.id, LastMove: c.lastMove, MoveCount: c.moveCount, TakeoverCount: c.takeoverCount, LastMoveKind: c.lastMoveKind}
		c.stateMu.Unlock()
		lease, err := p.statusLease(ctx, c.id)
		if errors.Is(err, sessionstore.ErrNotFound) || errors.Is(err, sessionstore.ErrEnded) {
			if c.mu.TryLock() {
				p.expiredLocked(ctx, c)
				c.mu.Unlock()
			}
			continue
		}
		if err != nil {
			return Status{}, err
		}

		c.rememberLease(lease)
		cs.Address, cs.Epoch = lease.Worker, lease.Epoch
		for i := range status.Workers {
			if status.Workers[i].Address == lease.Worker {
				cs.Owner = status.Workers[i].Name
				status.Workers[i].Calls++
			}
		}

		status.Calls = append(status.Calls, cs)
	}
	p.mu.Lock()
	status.Ready, status.Pool = p.readyLocked(), p.poolLocked(time.Now())
	status.Takeovers, status.LostCount = p.recentTakeovers(), p.lostCount
	p.mu.Unlock()
	sort.Slice(status.Workers, func(i, j int) bool { return status.Workers[i].Name < status.Workers[j].Name })
	sort.Slice(status.Calls, func(i, j int) bool { return status.Calls[i].ID < status.Calls[j].ID })
	return status, nil
}

// Redis can refuse fresh sockets briefly while draining connections queued
// during an outage. Status retries only authoritative reads, never effects or
// cached ownership, within a bounded part of the private HTTP request budget.
func (p *Plane) statusLease(ctx context.Context, id string) (sessionstore.Lease, error) {
	if p.restart == nil {
		return p.store.Get(ctx, id)
	}
	bounded, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	for {
		lease, err := p.store.Get(bounded, id)
		if !errors.Is(err, sessionstore.ErrTransient) || bounded.Err() != nil {
			return lease, err
		}
		if !restartWait(bounded, 100*time.Millisecond) {
			return lease, err
		}
	}
}

// drainParallelism bounds the moves a drain runs at once: one per CPU the
// process may use, at most 16.
func drainParallelism() int {
	return min(max(runtime.GOMAXPROCS(0), 1), 16)
}

// transfer adopts an exact candidate after uncertainty. Only settlement can
// prove a negative; a failed Get must never trigger rollback of a committed move.
func (p *Plane) transfer(ctx context.Context, from sessionstore.Lease, to netip.AddrPort) (sessionstore.Lease, error) {
	if p.restart != nil {
		if !p.controlAuthority() {
			return sessionstore.Lease{}, ErrNotReady
		}
		c, e := p.lookup(from.SessionID)
		if e != nil {
			return sessionstore.Lease{}, e
		}
		if c.intent == nil {
			return sessionstore.Lease{}, errors.New("controlplane: transfer missing durable intent")
		}
		ctx = sessionstore.WithTransitionIntent(ctx, c.id, c.intent.Token)
	}
	lease, err := p.store.Transfer(ctx, from, to, p.ttl)
	defer func() {
		if err == nil {
			if c, e := p.lookup(from.SessionID); e == nil {
				c.rememberLease(lease)
			}
		}
	}()
	var transient *sessionstore.TransientError
	if !errors.As(err, &transient) || transient.Candidate == nil {
		return lease, err
	}
	lease, err = p.resolveCandidate(ctx, *transient.Candidate, err)
	return lease, err
}
func (p *Plane) resolveCandidate(ctx context.Context, candidate sessionstore.Lease, cause error) (sessionstore.Lease, error) {
	ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	current, err := p.store.Get(ctx, candidate.SessionID)
	if err == nil && current.Worker == candidate.Worker && current.Epoch == candidate.Epoch {
		return current, nil
	}
	if resolver, ok := p.store.(sessionstore.TransitionResolver); ok {
		settled, committed, settleErr := resolver.Settle(ctx, candidate)
		if settleErr == nil {
			if committed {
				return settled, nil
			}
			return sessionstore.Lease{}, &sessionstore.TransientError{Op: "settled transfer", Err: cause}
		}
	}
	return sessionstore.Lease{}, &sessionstore.TransientError{Op: "uncertain transfer", Err: cause, Candidate: &candidate}
}

// Retain the final export and candidate if the store is still unavailable.
// Run retries these calls without declaring a healthy source worker dead or
// exporting it again. The relay hold waits for adoption or its bounded
// backstop; adopted retries explicitly release to the confirmed owner.
func (p *Plane) retainUncertainMove(source *registration, c *call, from sessionstore.Lease, routed netip.AddrPort, state []byte, cause error, moveStarted time.Time) bool {
	var transient *sessionstore.TransientError
	if !errors.As(cause, &transient) || transient.Candidate == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if source.pending == nil {
		source.pending = make(map[string]*takeoverState)
	}
	excluded := map[netip.AddrPort]bool{source.addr: true}
	if transient.Candidate.Worker == source.addr {
		excluded = map[netip.AddrPort]bool{from.Worker: true}
	}
	source.pending[c.id] = &takeoverState{call: c, lease: from, candidate: transient.Candidate, routed: routed, routedLease: from, excluded: excluded, attemptLimit: maxResumeAttempts, plannedState: state, planned: true, outageStarted: moveStarted}
	return true
}

// Only typed errors proving no flush and no discarded input are definitive.
// A successful scratch admission may already have consumed or dropped input;
// subsequent callback, encoding and transport errors require crash recovery.
func uncertainExport(err error) bool {
	if errors.Is(err, privateapi.ErrUncertain) {
		return true
	}
	for _, known := range []error{mediaworker.ErrUnknownSession, mediaworker.ErrClosed, mediaworker.ErrNotEstablished, mediaworker.ErrExportNotFlushed, mediaworker.ErrExportIncompatible,
		sessionstore.ErrNotFound, sessionstore.ErrLeaseLost, sessionstore.ErrTransient, agentv2.ErrCapacity} {
		if errors.Is(err, known) {
			return false
		}
	}
	return true
}
