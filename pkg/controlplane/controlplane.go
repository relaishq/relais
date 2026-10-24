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
	"github.com/relaishq/relais/pkg/mediaworker"
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

// ReplayRelay is the optional phase-2 caller-media cache capability. Both
// *relay.Relay and RemoteRelay implement it; phase-1/fake relays remain valid.
type ReplayRelay interface {
	BeginReplay(context.Context, string, netip.AddrPort, map[uint32]uint64) (relay.ReplayPlan, error)
	ReplaySession(context.Context, string, netip.AddrPort) (relay.ReplayResult, error)
}

type registration struct {
	inventoryOnce    sync.Once
	inventoryReader  func(context.Context) (mediaworker.WorkerStatus, error)
	preservedIntents []*call      // abandoned intents; cleaned after recovery jobs release call locks
	transfers        int          // store Transfer requests in flight; guarded by Plane.mu
	createMu         sync.RWMutex // isolate create observation cuts to this worker
	name             string
	addr             netip.AddrPort
	worker           Worker
	observation      workerObservation
	reserved         int // incoming creates/moves; counted when balancing
	rollbackSlots    int // source protection, used only by placement load
	retryingMoves    bool
}

type call struct {
	intent         *sessionstore.ControlIntent // guarded by mu
	terminal       bool                        // reads hold mu or the plane registry lock; writes hold both
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
	recovery        *moveRecovery
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
	workerChanged   chan struct{}
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
	p := &Plane{
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
		workerChanged:   make(chan struct{}),
	}
	p.recovery = &moveRecovery{plane: p, pending: make(map[*registration]map[string]*moveTransfer)}
	return p
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
	p.observationChangedLocked()
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
	p.observationChangedLocked()
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

	return p.recovery.moveLocked(ctx, c, lease, target)
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
	if !w.observation.knowledge().draining {
		w.observation.markDraining()
		p.observationChangedLocked()
	}
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

			results[i], errs[i] = p.recovery.moveLocked(ctx, j.c, current, j.target)
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

	calls := make(map[string]*call, len(p.calls))
	for id, c := range p.calls {
		calls[id] = c
	}
	p.mu.Unlock()
	for _, c := range calls {
		p.mu.Lock()
		terminal := c.terminal
		p.mu.Unlock()
		if terminal {
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
			}
		}

		status.Calls = append(status.Calls, cs)
	}
	p.mu.Lock()
	// Revalidate every sampled call at the same cut as terminal accounting.
	// A later lease read can block after an earlier call was assembled, and
	// terminal remains true even when cleanup clears cleanupPending.
	live := status.Calls[:0]
	for _, cs := range status.Calls {
		c := calls[cs.ID]
		if p.calls[cs.ID] == c && !c.terminal {
			live = append(live, cs)
		}
	}
	status.Calls = live
	status.Ready, status.Pool = p.readyLocked(), p.poolLocked(time.Now())
	status.Takeovers, status.LostCount = p.recentTakeovers(), p.lostCount
	p.mu.Unlock()
	counts := make(map[string]int, len(status.Workers))
	for _, cs := range status.Calls {
		counts[cs.Owner]++
	}
	for i := range status.Workers {
		status.Workers[i].Calls = counts[status.Workers[i].Name]
	}
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
