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

	"github.com/relais/internal/privateapi"
	"github.com/relais/pkg/mediaworker"
	"github.com/relais/pkg/metrics"
	"github.com/relais/pkg/relay"
	"github.com/relais/pkg/sessionstore"
)

var (
	// ErrMoveInProgress refuses overlapping coordination of the same call.
	ErrMoveInProgress = errors.New("controlplane: move in progress")
	// ErrUnknownCall means this control plane no longer tracks the call.
	ErrUnknownCall = errors.New("controlplane: unknown call")
	// ErrNoTarget means no non-draining destination is available.
	ErrNoTarget = errors.New("controlplane: no non-draining target worker")
)

// Worker is the media boundary, local or remote. Resume must either adopt the
// supplied lease and run the session, or fail without running it.
type Worker interface {
	CreateSession(ctx context.Context, offer string) (string, string, error)
	EndSession(sessionID string) error
	ExportSession(sessionID string) ([]byte, error)
	ResumeSession(state []byte, opts mediaworker.ResumeOptions) (string, error)
}

// Relay is the trusted routing boundary, satisfied by *relay.Relay.
type Relay interface {
	HoldSession(ctx context.Context, sessionID string, from netip.AddrPort) error
	MoveSession(sessionID string, from, to netip.AddrPort) error
	ReleaseSession(sessionID string, to netip.AddrPort) (int, error)
	ForgetSession(sessionID string)
}

type registration struct {
	name          string
	addr          netip.AddrPort
	worker        Worker
	draining      bool
	reserved      int // incoming creates/moves; counted when balancing
	lastHeartbeat time.Time
	dead          bool
	recovering    bool
	recovered     bool
	rejoinReady   bool
	rejoinToken   string
	acceptedToken string
	retryingMoves bool
	pending       map[string]*takeoverState // unfinished transfers, retained for retry
}

type call struct {
	// hungUp records a completed End; retained retries keep this call.
	hungUp        atomic.Bool
	mu            sync.Mutex
	id            string
	lastMove      *time.Time
	moveCount     uint64
	takeoverCount uint64
	lastMoveKind  string
}

// Plane owns the registry and call metadata. The store remains authoritative
// for ownership. New and Register must be called before accepting calls.
type Plane struct {
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
	running         bool
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
		ttl:             3 * time.Second,
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

	p.workers[name] = &registration{name: name, addr: addr, worker: worker, lastHeartbeat: time.Now()}
	p.mu.Unlock()
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
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.mu.Lock()
	candidates := make([]*registration, 0, len(p.workers))
	for _, w := range p.workers {
		if (name == "" || w.name == name) && !w.draining && !w.dead && !excluded[w.addr] {
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
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var best *registration
	load := int(^uint(0) >> 1)
	for _, w := range candidates {
		// Registry replacement, death or drain may have raced with the listing.
		if p.workers[w.name] != w || w.draining || w.dead {
			continue
		}
		n := loads[w] + w.reserved
		if n < load || n == load && (best == nil || w.name < best.name) {
			best, load = w, n
		}
	}
	if best == nil {
		return nil, ErrNoTarget
	}
	best.reserved++
	return best, nil
}

func (p *Plane) unreserve(w *registration) {
	p.mu.Lock()
	defer p.mu.Unlock()

	w.reserved--
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
	defer func() {
		if createErr != nil {
			p.metrics.callErrors.Add(1)
		} else {
			p.metrics.calls.Add(1)
		}
	}()
	w, err := p.pick(ctx, name, netip.AddrPort{})
	if err != nil {
		return "", "", err
	}
	defer p.unreserve(w)

	id, answer, err := w.worker.CreateSession(ctx, offer)
	if err != nil {
		return "", "", err
	}

	p.mu.Lock()
	p.calls[id] = &call{id: id}
	p.mu.Unlock()
	return id, answer, nil
}

// End waits for a move already in progress, then hangs up the current owner.
func (p *Plane) End(ctx context.Context, id string) error {
	c, err := p.lookup(id)
	if err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	lease, err := p.store.Get(ctx, id)
	if err != nil {
		if errors.Is(err, sessionstore.ErrNotFound) {
			p.forget(c)
			p.deleteFrames(id)
		}

		return err
	}

	p.mu.Lock()
	w := p.owner(lease.Worker)
	p.mu.Unlock()
	if w == nil {
		return ErrUnknownCall
	}

	if err := w.worker.EndSession(id); err != nil {
		if errors.Is(err, mediaworker.ErrUnknownSession) {
			p.forget(c)
			if p.store.Release(ctx, lease) == nil {
				c.hungUp.Store(true)
			}
			p.deleteFrames(id)
		}

		return err
	}
	c.hungUp.Store(true)
	p.forget(c)
	p.deleteFrames(id)
	return nil
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

// Move fails fast if another move or drain holds the call. Empty to picks
// the least-loaded non-draining worker other than the current owner.
func (p *Plane) Move(ctx context.Context, id, to string) (MoveResult, error) {
	c, err := p.lookup(id)
	if err != nil {
		return MoveResult{}, err
	}

	if !c.mu.TryLock() {
		return MoveResult{}, ErrMoveInProgress
	}
	defer c.mu.Unlock()

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
	p.mu.Unlock()
	if source == nil {
		return res, ErrUnknownCall
	}

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
	if err = r.HoldSession(ctx, c.id, source.addr); err != nil {
		if errors.Is(err, privateapi.ErrUncertain) {
			_, _ = r.ReleaseSession(c.id, source.addr)
		}
		return res, err
	}

	res.Result.Drain = time.Since(started)
	// Successful/rolled-back resume releases to its owner. An uncertain final
	// export retains the hold until adoption, bounded by the relay backstop.
	releaseTo := source.addr
	released := false
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
		held, releaseErr := r.ReleaseSession(c.id, releaseTo)
		res.Result.HeldPackets = held
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
	state, err := source.worker.ExportSession(c.id)
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
			source.pending[c.id] = &takeoverState{call: c, lease: lease, routed: source.addr,
				excluded: map[netip.AddrPort]bool{source.addr: true}, attemptLimit: maxResumeAttempts, held: true, outageStarted: res.Start}
			p.mu.Unlock()
			recovery, cancel := context.WithTimeout(context.Background(), takeoverBudget)
			defer cancel()
			p.takeoverLocked(recovery, source, c, lease, res.Start)
			p.mu.Lock()
			pending := source.pending[c.id]
			outcomeRecorded := false
			events := p.recentTakeovers()
			for i := len(events) - 1; i >= 0; i-- {
				if events[i].ID == c.id && !events[i].Start.Before(res.Start) {
					outcomeRecorded = true
					res = events[i]
					if !res.Lost {
						releaseTo = target.addr
						err = nil
					} else {
						err = errors.New(res.Error)
					}
					break
				}
			}
			p.mu.Unlock()
			if pending != nil {
				return res, fmt.Errorf("controlplane: export uncertain; recovery pending: %w", err)
			}
			if !outcomeRecorded {
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
			if errors.Is(getErr, sessionstore.ErrNotFound) || getErr == nil &&
				(current.Worker != lease.Worker || current.Epoch != lease.Epoch) {
				p.forget(c)
			}
		}

		return res, err
	}

	res.Result.StateBytes = len(state)
	transferred, err := p.transfer(ctx, lease, target.addr)
	if err != nil {
		if p.retainUncertainMove(source, c, lease, source.addr, state, err, res.Start) {
			return res, err
		}
		err = p.rollback(c, source, target, r, state, lease, false, false, false, &res.Result, err, res.Start)
		return res, err
	}

	if err = r.MoveSession(c.id, source.addr, target.addr); err != nil {
		err = p.rollback(c, source, target, r, state, transferred, true, true, false, &res.Result, err, res.Start)
		return res, err
	}

	started = time.Now()
	_, err = target.worker.ResumeSession(state, mediaworker.ResumeOptions{Lease: transferred})
	res.Result.Resume = time.Since(started)
	if err != nil {
		err = p.rollback(c, source, target, r, state, transferred, true, true, errors.Is(err, privateapi.ErrUncertain) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded), &res.Result, err, res.Start)
		return res, err
	}

	releaseTo = target.addr
	now := time.Now()
	c.lastMove = &now
	c.moveCount++
	moved = true
	c.lastMoveKind = "move"
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
		opts := mediaworker.ResumeOptions{Lease: lease, Context: resumeCtx}
		if uncertainResume {
			// B may already have emitted media and persisted adjusted counters.
			// Its fenced store state, rather than the final export from A, is now
			// the only valid checkpoint for a rollback that needs crash margins.
			state, err = p.store.GetState(ctx, c.id)
			if err == nil {
				var decision checkpointDecision
				decision, err = p.checkpointDecision(resumeCtx, c.id, state, checkpointOutageBudget(resumeCtx, moveStarted))
				res.CheckpointAge, res.SnapshotAge, res.CheckpointStoredAt = decision.age, decision.snapshotAge, decision.storedAt
				res.Checkpoint, res.SequenceMargin, res.SRTCPIndexMargin = decision.info, decision.margin, decision.rtcpMargin
				checkpointOutside = decision.outside
				opts.SequenceMargin, opts.SRTCPIndexMargin = decision.margin, decision.rtcpMargin
				opts.CallerSequenceReserve = decision.reserve
				opts.CheckpointAge, opts.SnapshotAge, opts.CheckpointStoredAt = decision.age, decision.snapshotAge, decision.storedAt
			}
		}
		if err == nil {
			_, err = source.worker.ResumeSession(state, opts)
		}
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

// Drain blocks new selections immediately. Existing calls move concurrently,
// at most drainParallelism at a time: a call waiting its turn keeps flowing
// on the old worker, so running more moves than there are CPUs would only
// lengthen every held call's gap. Reservations keep concurrent selections
// balanced. With no destination it
// moves nothing. It marks draining before waiting up to one second for
// incoming reservations, then includes those calls in the drain.
func (p *Plane) Drain(ctx context.Context, name string) (moves []MoveResult, drainErr error) {
	defer func() {
		if drainErr != nil {
			p.metrics.drainErrors.Add(1)
		} else {
			p.metrics.drains.Add(1)
		}
	}()
	p.mu.Lock()
	w := p.workers[name]
	if w == nil {
		p.mu.Unlock()
		return nil, errors.New("controlplane: unknown worker")
	}

	w.draining = true
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
	}

	jobs := make([]job, 0, len(leases))
	for _, lease := range leases {
		target, pickErr := p.pick(ctx, "", w.addr)
		if pickErr != nil {
			for _, j := range jobs {
				p.unreserve(j.target)
			}
			return nil, pickErr
		}

		p.mu.Lock()
		c := p.calls[lease.SessionID]
		p.mu.Unlock()
		jobs = append(jobs, job{lease: lease, target: target, c: c})
	}
	results := make([]MoveResult, len(jobs))
	errs := make([]error, len(jobs))
	var wg sync.WaitGroup
	sem := make(chan struct{}, drainParallelism())
	for i, j := range jobs {
		wg.Go(func() {
			defer p.unreserve(j.target)

			results[i] = MoveResult{ID: j.lease.SessionID, From: w.name, To: j.target.name}
			defer func() {
				if errs[i] != nil {
					results[i].Error = errs[i].Error()
				}
			}()
			sem <- struct{}{}
			defer func() { <-sem }()

			if j.c == nil {
				errs[i] = ErrUnknownCall
				return
			}

			if !j.c.mu.TryLock() {
				errs[i] = ErrMoveInProgress
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
	Name          string         `json:"name"`
	Address       netip.AddrPort `json:"address"`
	Draining      bool           `json:"draining"`
	Recovering    bool           `json:"recovering"`
	Dead          bool           `json:"dead"`
	LastHeartbeat time.Time      `json:"last_heartbeat"`
	Calls         int            `json:"call_count"`
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
	Workers   []WorkerStatus `json:"workers"`
	Calls     []CallStatus   `json:"calls"`
	Takeovers []MoveResult   `json:"takeovers"`
	LostCount uint64         `json:"lost_count"`
}

// Status lists live calls and prunes metadata whose lease has disappeared.
func (p *Plane) Status(ctx context.Context) (Status, error) {
	p.mu.Lock()
	workers := make([]*registration, 0, len(p.workers))
	for _, w := range p.workers {
		workers = append(workers, w)
	}
	// Copy registry flags here; metadata is read under each call lock below.
	status := Status{Workers: []WorkerStatus{}, Calls: []CallStatus{}, Takeovers: p.recentTakeovers(), LostCount: p.lostCount}
	for _, w := range workers {
		status.Workers = append(status.Workers, WorkerStatus{Name: w.name, Address: w.addr, Draining: w.draining, Dead: w.dead, Recovering: w.recovering, LastHeartbeat: w.lastHeartbeat})
	}

	calls := make([]*call, 0, len(p.calls))
	for _, c := range p.calls {
		calls = append(calls, c)
	}
	p.mu.Unlock()
	for _, c := range calls {
		c.mu.Lock()
		lease, err := p.store.Get(ctx, c.id)
		if errors.Is(err, sessionstore.ErrNotFound) {
			p.forget(c)
			c.mu.Unlock()
			continue
		}
		if err != nil {
			c.mu.Unlock()
			return Status{}, err
		}

		cs := CallStatus{ID: c.id, Address: lease.Worker, Epoch: lease.Epoch, LastMove: c.lastMove, MoveCount: c.moveCount, TakeoverCount: c.takeoverCount, LastMoveKind: c.lastMoveKind}
		for i := range status.Workers {
			if status.Workers[i].Address == lease.Worker {
				cs.Owner = status.Workers[i].Name
				status.Workers[i].Calls++
			}
		}

		status.Calls = append(status.Calls, cs)
		c.mu.Unlock()
	}
	sort.Slice(status.Workers, func(i, j int) bool { return status.Workers[i].Name < status.Workers[j].Name })
	sort.Slice(status.Calls, func(i, j int) bool { return status.Calls[i].ID < status.Calls[j].ID })
	return status, nil
}

// drainParallelism bounds the moves a drain runs at once: one per CPU the
// process may use, at most 16.
func drainParallelism() int {
	return min(max(runtime.GOMAXPROCS(0), 1), 16)
}

// transfer adopts an exact candidate after uncertainty. Only settlement can
// prove a negative; a failed Get must never trigger rollback of a committed move.
func (p *Plane) transfer(ctx context.Context, from sessionstore.Lease, to netip.AddrPort) (sessionstore.Lease, error) {
	lease, err := p.store.Transfer(ctx, from, to, p.ttl)
	var transient *sessionstore.TransientError
	if !errors.As(err, &transient) || transient.Candidate == nil {
		return lease, err
	}
	return p.resolveCandidate(ctx, *transient.Candidate, err)
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
	source.pending[c.id] = &takeoverState{call: c, lease: from, candidate: transient.Candidate, routed: routed, excluded: excluded, attemptLimit: maxResumeAttempts, plannedState: state, planned: true, outageStarted: moveStarted}
	return true
}

// Typed pre-export failures are definite. All other errors could follow a
// flushed source, including an HTTP internal error whose cause was not mapped.
func uncertainExport(err error) bool {
	if errors.Is(err, privateapi.ErrUncertain) {
		return true
	}
	for _, known := range []error{mediaworker.ErrUnknownSession, mediaworker.ErrClosed, mediaworker.ErrNotEstablished,
		sessionstore.ErrNotFound, sessionstore.ErrLeaseLost, sessionstore.ErrTransient} {
		if errors.Is(err, known) {
			return false
		}
	}
	return true
}
