package controlplane

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/relaishq/relais/internal/privateapi"
	agentv2 "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/relay"
	"github.com/relaishq/relais/pkg/sessionstore"
)

// ErrNotReady distinguishes process readiness from a worker agent's startup
// refusal. The wire retains the existing control_not_ready code and decodes as
// agentv2.ErrControlNotReady for compatibility with current clients.
var ErrNotReady error = &controlReadinessError{}

type controlReadinessError struct{}

func (*controlReadinessError) Error() string { return "controlplane: not ready for mutations" }
func (*controlReadinessError) Unwrap() error { return agentv2.ErrControlNotReady }

const ControlLeaseTTL = time.Second

const controlRenewInterval = 250 * time.Millisecond
const controlRequestTimeout = 750 * time.Millisecond
const rebuildStatusTimeout = 250 * time.Millisecond

// FixedWorker names an operator-configured private HTTP endpoint. Saved relay
// addresses let a restart recover calls even when that worker is unreachable.
type FixedWorker struct {
	Name    string         `json:"name"`
	URL     string         `json:"url"`
	Address netip.AddrPort `json:"address"`
}
type createIntent struct {
	Worker  string    `json:"worker"`
	Started time.Time `json:"started"`
}

var errCreateStatus = errors.New("controlplane: create worker status unavailable")

// RestartOutcome is a typed result for an operation whose HTTP caller was lost.
// A live result does not claim uninterrupted media or replay a worker effect.
type RestartOutcome struct {
	Operation string `json:"operation"`
	ID        string `json:"id"`
	Outcome   string `json:"outcome"`
	Code      string `json:"code,omitempty"`
}
type RebuildStatus struct {
	Phase     string           `json:"phase"`
	Started   time.Time        `json:"started"`
	ReadyAt   time.Time        `json:"ready_at,omitempty"`
	Duration  time.Duration    `json:"duration"`
	LeaseWait time.Duration    `json:"lease_wait"`
	Error     string           `json:"error,omitempty"`
	Outcomes  []RestartOutcome `json:"outcomes"`
}
type controlGuard struct {
	lease    sessionstore.ControlLease
	deadline time.Time // local monotonic time; never compare clocks across hosts
}
type restartState struct {
	store          sessionstore.ControlStore
	guard          atomic.Pointer[controlGuard]
	ready          atomic.Bool
	mu             sync.Mutex
	status         RebuildStatus
	registrationMu sync.Mutex
	healthy        atomic.Bool
	renewalFailed  atomic.Bool
	stopping       atomic.Bool
	authority      chan struct{}
	logs           map[string]time.Time
	creates        map[string]*pendingCreate
}

func randomToken() string {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(token[:])
}

// EnableRestart must precede serving HTTP or calling RunManaged. In-process
// harnesses can retain the existing unmanaged Plane lifecycle.
func (p *Plane) EnableRestart() error {
	store, ok := p.store.(sessionstore.ControlStore)
	if !ok {
		return errors.New("controlplane: restart requires a durable control store")
	}
	p.restart = &restartState{store: store, authority: make(chan struct{}, 1), status: RebuildStatus{Phase: "waiting_lease", Started: time.Now(), Outcomes: []RestartOutcome{}}}
	return nil
}
func (p *Plane) controlAuthority() bool {
	if p.restart == nil {
		return true
	}
	guard := p.restart.guard.Load()
	return guard != nil && time.Now().Before(guard.deadline)
}
func (p *Plane) mutationReady() bool {
	return p.restart == nil || !p.restart.stopping.Load() && p.restart.ready.Load() && p.controlMutationAuthority() && p.controlAuthority()
}

// A failed renewal does not revoke an acknowledged lease. Refuse new work
// near its deadline, while detection remains fenced by the actual deadline.
func (p *Plane) controlMutationAuthority() bool {
	if !p.restart.healthy.Load() {
		return false
	}
	guard := p.restart.guard.Load()
	return guard != nil && (!p.restart.renewalFailed.Load() || time.Now().Before(guard.deadline.Add(-controlRenewInterval)))
}
func (p *Plane) requireMutationReady() error {
	if !p.mutationReady() {
		return ErrNotReady
	}
	return nil
}
func (p *Plane) controlLease() (sessionstore.ControlLease, error) {
	guard := p.restart.guard.Load()
	if guard == nil || !time.Now().Before(guard.deadline) {
		return sessionstore.ControlLease{}, ErrNotReady
	}
	return guard.lease, nil
}
func (p *Plane) RebuildStatus() *RebuildStatus {
	if p.restart == nil {
		return nil
	}
	p.restart.mu.Lock()
	defer p.restart.mu.Unlock()
	status := p.restart.status
	status.Outcomes = append([]RestartOutcome{}, status.Outcomes...)
	if status.ReadyAt.IsZero() {
		status.Duration = time.Since(status.Started)
	}
	return &status
}
func (p *Plane) rebuildPhase(phase string, err error) {
	p.restart.mu.Lock()
	defer p.restart.mu.Unlock()
	p.restart.status.Phase = phase
	p.restart.status.Error = ""
	if err != nil {
		p.restart.status.Error = err.Error()
		p.restartLogLocked("phase:"+phase, "control not ready phase=%s cause=%v", phase, err)
	}
}

func (p *Plane) restartLog(key, format string, args ...any) {
	p.restart.mu.Lock()
	defer p.restart.mu.Unlock()
	p.restartLogLocked(key, format, args...)
}
func (p *Plane) restartLogLocked(key, format string, args ...any) {
	if p.restart.logs == nil {
		p.restart.logs = map[string]time.Time{}
	}
	if time.Since(p.restart.logs[key]) < time.Second {
		return
	}
	p.restart.logs[key] = time.Now()
	log.Printf(format, args...)
}
func (p *Plane) restartOutcome(kind, id, outcome, code string) {
	p.restart.mu.Lock()
	defer p.restart.mu.Unlock()
	status := &p.restart.status
	if len(status.Outcomes) == recentTakeoverLimit {
		status.Outcomes = status.Outcomes[1:]
	}
	status.Outcomes = append(status.Outcomes, RestartOutcome{Operation: kind, ID: id, Outcome: outcome, Code: code})
}
func (p *Plane) controlRecord(ctx context.Context, key string, value any) error {
	if p.restart == nil {
		return nil
	}
	lease, err := p.controlLease()
	if err != nil {
		return err
	}
	var data []byte
	if value != nil {
		data, err = json.Marshal(value)
		if err != nil {
			return err
		}
	}
	return p.restart.store.PutControlRecord(ctx, lease, key, data)
}
func (p *Plane) saveRegistration(ctx context.Context, name string, addr netip.AddrPort, worker Worker) error {
	if p.restart == nil {
		return nil
	}
	remote, ok := worker.(*RemoteWorker)
	if !ok {
		return errors.New("controlplane: managed worker requires HTTP observation")
	}
	return p.controlRecord(ctx, "worker:"+name, FixedWorker{Name: name, URL: remote.URL, Address: addr})
}

// AuthorityChanges wakes relay registration when a lease becomes usable.
func (p *Plane) AuthorityChanges() <-chan struct{} {
	if p.restart == nil {
		return nil
	}
	return p.restart.authority
}

// BeginShutdown pauses work while the lease keeper protects handlers that are
// still draining. The caller cancels RunManaged only after handlers stop.
func (p *Plane) BeginShutdown() {
	if p.restart == nil {
		return
	}
	p.restart.stopping.Store(true)
	p.restart.ready.Store(false)
	p.rebuildPhase("stopping", nil)
}

// A tenure owns its pending requests so a definitive loss can cancel them
// before a fresh claim; shutdown also waits for all callbacks to finish.
type controlRequestTenure struct {
	context.Context
	cancel context.CancelFunc
}

func newControlRequestTenure(ctx context.Context) controlRequestTenure {
	next, cancel := context.WithCancel(ctx)
	return controlRequestTenure{next, cancel}
}

// maintainControl keeps trying the same tenure through transient store errors.
// A definitive lost lease requires a fresh claim and a full rebuild.
func (p *Plane) maintainControl(ctx context.Context, fatal chan<- error) {
	// Replies can take more than half the TTL. Keep a bounded pipeline of
	// same-tenure renewals; waiting an interval after each reply would lapse
	// local authority even when every store command succeeds.
	type result struct {
		lease      sessionstore.ControlLease
		err        error
		started    time.Time
		generation uint64
		claiming   bool
	}
	const maxPending = 4
	replies := make(chan result, maxPending)
	tenure := newControlRequestTenure(ctx)
	var wg sync.WaitGroup
	defer func() { tenure.cancel(); wg.Wait() }()
	timer := time.NewTimer(0)
	defer timer.Stop()
	holder := randomToken()
	var lease sessionstore.ControlLease
	var lastSuccess time.Time
	var generation uint64
	pending := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if pending == maxPending || lease.Holder == "" && pending != 0 {
				timer.Reset(25 * time.Millisecond)
				continue
			}
			started := time.Now()
			requestLease, requestGeneration := lease, generation
			claiming := lease.Holder == ""
			bounded, stop := context.WithTimeout(tenure, controlRequestTimeout)
			pending++
			wg.Go(func() {
				defer stop()
				var next sessionstore.ControlLease
				var err error
				if claiming {
					next, err = p.restart.store.ClaimControl(bounded, holder, ControlLeaseTTL)
				} else {
					next, err = p.restart.store.RenewControl(bounded, requestLease, ControlLeaseTTL)
				}
				// The channel holds every pending reply, including canceled generations.
				replies <- result{next, err, started, requestGeneration, claiming}
			})
			if !claiming {
				timer.Reset(controlRenewInterval)
			}
		case reply := <-replies:
			pending--
			if ctx.Err() != nil {
				return
			}
			if reply.generation != generation {
				continue
			}
			if reply.err == nil {
				deadline := reply.started.Add(ControlLeaseTTL)
				if !time.Now().Before(deadline) {
					// A callback may outlive cancellation. Claims have no
					// periodic timer while pending, so explicitly retry.
					if reply.claiming {
						timer.Reset(0)
					}
					continue
				}
				if reply.started.Before(lastSuccess) {
					continue
				}
				lastSuccess = reply.started
				lease = reply.lease
				wasAuthoritative := p.controlAuthority()
				wasHealthy := p.restart.healthy.Swap(true)
				p.restart.renewalFailed.Store(false)
				p.restart.guard.Store(&controlGuard{lease: lease, deadline: deadline})
				p.restart.mu.Lock()
				if p.restart.status.LeaseWait == 0 {
					p.restart.status.LeaseWait = time.Since(p.restart.status.Started)
				}
				p.restart.mu.Unlock()
				if reply.claiming || !wasHealthy || !wasAuthoritative || !p.restart.ready.Load() {
					select {
					case p.restart.authority <- struct{}{}:
					default:
					}
				}
				if reply.claiming {
					timer.Reset(max(time.Duration(0), controlRenewInterval-time.Since(reply.started)))
				}
				continue
			}
			switch {
			case errors.Is(reply.err, sessionstore.ErrLeaseLost):
				p.restart.healthy.Store(false)
				p.restart.ready.Store(false)
				p.restart.guard.Store(nil)
				lease = sessionstore.ControlLease{}
				generation++
				lastSuccess = time.Time{}
				tenure.cancel()
				tenure = newControlRequestTenure(ctx)
				p.rebuildPhase("lease_lost", reply.err)
				timer.Reset(0)
			case errors.Is(reply.err, sessionstore.ErrLeaseHeld), errors.Is(reply.err, sessionstore.ErrTransient), errors.Is(reply.err, context.DeadlineExceeded), errors.Is(reply.err, context.Canceled):
				if !reply.started.Before(lastSuccess) {
					p.restart.renewalFailed.Store(true)
				}
				if reply.claiming {
					p.rebuildPhase("waiting_lease", reply.err)
					timer.Reset(50 * time.Millisecond)
				} else {
					p.restartLog("renewal", "control lease renewal failed; acknowledged deadline still applies: %v", reply.err)
					// Retry inside the readiness reserve so a recovered store
					// can acknowledge this tenure before the deadline lapses.
					timer.Reset(controlRenewInterval / 2)
				}
			default:
				p.restart.healthy.Store(false)
				p.restart.ready.Store(false)
				p.restart.guard.Store(nil)
				p.rebuildPhase("failed", reply.err)
				fatal <- fmt.Errorf("controlplane: control lease: %w", reply.err)
				return
			}
		}
	}
}

// RunManaged pauses detection at the local lease deadline, retains the process
// through store outages, and rebuilds after reacquisition. Renewal outlives
// cancellation until all recovery jobs have stopped.
func (p *Plane) RunManaged(ctx context.Context, workers []FixedWorker) error {
	if p.restart == nil {
		return errors.New("controlplane: EnableRestart required")
	}
	renewCtx, stopRenew := context.WithCancel(context.WithoutCancel(ctx))
	renewDone := make(chan struct{})
	fatal := make(chan error, 1)
	go func() { defer close(renewDone); p.maintainControl(renewCtx, fatal) }()
	var stopWork context.CancelFunc
	var workDone chan error
	stop := func() {
		p.restart.ready.Store(false)
		if stopWork != nil {
			stopWork()
			<-workDone
			stopWork = nil
		}
	}
	defer func() { stop(); stopRenew(); <-renewDone }()
	var built uint64
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-fatal:
			return err
		case err := <-workDone:
			stopWork, workDone = nil, nil
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("controlplane: detector stopped unexpectedly: %v", err)
		default:
		}
		if p.restart.stopping.Load() {
			stop()
			if !restartWait(ctx, 25*time.Millisecond) {
				return nil
			}
			continue
		}
		guard := p.restart.guard.Load()
		if guard == nil || !time.Now().Before(guard.deadline) {
			stop()
			// Retain the completed rebuild's epoch. A successful renewal of
			// that tenure proves no successor acted; its live intents are ours.
			if guard != nil {
				p.rebuildPhase("renewing", errors.New("local control lease deadline elapsed; detection paused"))
			}
		} else if p.controlMutationAuthority() {
			if built != guard.lease.Epoch {
				stop()
				if err := p.rebuild(ctx, workers); err != nil {
					p.rebuildPhase("rebuilding", err)
				} else if current := p.restart.guard.Load(); current != nil && current.lease.Holder == guard.lease.Holder && current.lease.Epoch == guard.lease.Epoch && p.controlAuthority() && p.controlMutationAuthority() {
					built = guard.lease.Epoch
				}
			}
			if built == guard.lease.Epoch {
				if stopWork == nil {
					workCtx, cancel := context.WithCancel(ctx)
					stopWork = cancel
					workDone = make(chan error, 1)
					go func() { workDone <- p.runManagedWork(workCtx, workers) }()
				}
				p.restart.mu.Lock()
				p.restart.status.Phase, p.restart.status.Error = "ready", ""
				if p.restart.status.ReadyAt.IsZero() {
					p.restart.status.ReadyAt = time.Now()
					p.restart.status.Duration = time.Since(p.restart.status.Started)
				}
				p.restart.mu.Unlock()
				p.restart.ready.Store(true)
			}
		} else {
			p.restart.ready.Store(false)
			p.rebuildPhase("renewing", errors.New("renewal unavailable near local lease deadline; mutations paused"))
		}
		if !restartWait(ctx, 25*time.Millisecond) {
			return nil
		}
	}
}

func (p *Plane) runManagedWork(ctx context.Context, workers []FixedWorker) error {
	discoverDone := make(chan struct{})
	go func() {
		defer close(discoverDone)
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if !p.controlAuthority() || p.restart.stopping.Load() {
					continue
				}
				for _, worker := range workers {
					if err := p.discoverWorker(ctx, worker, nil); err != nil {
						p.restartLog("worker:"+worker.Name, "worker discovery name=%s error=%v", worker.Name, err)
					}
				}
				if err := p.reconcileCreates(ctx); err != nil {
					p.restartLog("creates", "create reconciliation error=%v", err)
				}
			}
		}
	}()
	err := p.Run(ctx)
	<-discoverDone
	return err
}

// ReleaseControl is called only after HTTP handlers and recovery jobs stopped.
func (p *Plane) ReleaseControl(ctx context.Context) error {
	if p.restart == nil {
		return nil
	}
	p.restart.ready.Store(false)
	guard := p.restart.guard.Swap(nil)
	if guard == nil {
		return nil
	}
	p.rebuildPhase("stopped", nil)
	return p.restart.store.ReleaseControl(ctx, guard.lease)
}
func restartWait(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (p *Plane) discoverWorker(ctx context.Context, fixed FixedWorker, saved *FixedWorker) error {
	worker := &RemoteWorker{URL: fixed.URL}
	bounded, cancel := context.WithTimeout(ctx, time.Second)
	status, err := worker.Status(bounded)
	cancel()
	if err != nil {
		p.restartLog("worker:"+fixed.Name, "worker discovery name=%s error=%v", fixed.Name, err)
	}
	return p.registerObservedWorker(ctx, fixed, saved, worker, status, err)
}

func (p *Plane) registerObservedWorker(ctx context.Context, fixed FixedWorker, saved *FixedWorker, worker *RemoteWorker, status mediaworker.WorkerStatus, err error) error {
	p.restart.registrationMu.Lock()
	defer p.restart.registrationMu.Unlock()
	p.mu.Lock()
	old := p.workers[fixed.Name]
	p.mu.Unlock()
	if old != nil {
		if err == nil && status.Address != old.addr {
			// A changed private leg is a replacement, never proof its old calls ended.
			p.mu.Lock()
			canReplace := old.observation.knowledge().replacementReady
			p.mu.Unlock()
			if canReplace {
				return p.Replace(fixed.Name, status.Address, worker)
			}
		}
		return nil
	}
	address := status.Address
	down := err != nil
	if down {
		if saved == nil || !saved.Address.IsValid() {
			return nil
		}
		address = saved.Address
	}
	if !down && (!status.InventoryKnown || status.Truncated) {
		return fmt.Errorf("controlplane: worker %s inventory unavailable or truncated", fixed.Name)
	}
	if saved != nil && saved.Address.IsValid() && saved.Address != address {
		// Recover the prior process before registering a new leg at this endpoint.
		address = saved.Address
		down = true
	}
	if err := p.Register(fixed.Name, address, worker); err != nil {
		return err
	}
	log.Printf("worker bootstrap name=%s attempt=1 result=registered", fixed.Name)
	p.mu.Lock()
	registration := p.workers[fixed.Name]
	if registration.observation.observeStatus(status, down, true) {
		p.loadVersion++
	}
	p.mu.Unlock()
	return nil
}

func (p *Plane) rebuild(ctx context.Context, fixed []FixedWorker) error {
	lease, err := p.controlLease()
	if err != nil {
		return err
	}
	records, err := p.restart.store.ControlRecords(ctx, lease)
	if err != nil {
		return err
	}
	saved := map[string]FixedWorker{}
	for key, data := range records {
		if strings.HasPrefix(key, "worker:") {
			var worker FixedWorker
			if err := json.Unmarshal(data, &worker); err != nil {
				return err
			}
			saved[worker.Name] = worker
		}
	}
	configured := map[string]bool{}
	for _, worker := range fixed {
		configured[worker.Name] = true
	}
	for name := range saved {
		if !configured[name] {
			return fmt.Errorf("controlplane: saved worker %s missing from -worker; restore its fixed entry to recover its calls before removing it", name)
		}
	}
	var wg sync.WaitGroup
	failures := make(chan error, len(fixed))
	type observation struct {
		name   string
		status mediaworker.WorkerStatus
	}
	observed := make(chan observation, len(fixed))
	for _, worker := range fixed {
		wg.Go(func() {
			var old *FixedWorker
			if value, ok := saved[worker.Name]; ok {
				old = &value
			}
			remote := &RemoteWorker{URL: worker.URL}
			bounded, stop := context.WithTimeout(ctx, rebuildStatusTimeout)
			status, statusErr := remote.Status(bounded)
			stop()
			if statusErr != nil {
				p.restartLog("worker:"+worker.Name, "worker discovery name=%s error=%v", worker.Name, statusErr)
			}
			if err := p.registerObservedWorker(ctx, worker, old, remote, status, statusErr); err != nil {
				failures <- err
			}
			if statusErr == nil {
				observed <- observation{worker.Name, status}
			}
		})
	}
	wg.Wait()
	close(observed)
	workerStatuses := map[string]mediaworker.WorkerStatus{}
	for item := range observed {
		workerStatuses[item.name] = item.status
	}
	close(failures)
	for err := range failures {
		return err
	}
	p.mu.Lock()
	registrations := make([]*registration, 0, len(p.workers))
	for _, worker := range p.workers {
		registrations = append(registrations, worker)
		if _, ok := records["drain:"+worker.name]; ok {
			worker.observation.markDraining()
		}
	}
	relayReady := p.relayReady
	for _, worker := range registrations {
		if worker.observation.knowledge().relayRequired {
			relayReady = false
		}
	}
	p.mu.Unlock()
	if !relayReady {
		return fmt.Errorf("controlplane: relay workers not registered: %w", ErrNotReady)
	}
	observations := map[string]mediaworker.InstalledSession{}
	for _, worker := range registrations {
		status, known := workerStatuses[worker.name]
		if known && status.Address == worker.addr {
			if !status.InventoryKnown || status.Truncated {
				return fmt.Errorf("controlplane: worker %s inventory unavailable or truncated", worker.name)
			}
			p.mu.Lock()
			worker.observation.observeStatus(status, false, false)
			p.mu.Unlock()
			if status.Draining {
				if err := p.controlRecord(ctx, "drain:"+worker.name, true); err != nil {
					return err
				}
			}
			for _, installed := range status.Inventory {
				if installed.Owner != worker.addr {
					continue
				}
				lease, err := p.store.Get(ctx, installed.ID)
				if err == nil {
					p.rebuildCall(lease)
				} else if !errors.Is(err, sessionstore.ErrNotFound) && !errors.Is(err, sessionstore.ErrEnded) {
					return err
				}
				// The inventory cannot grant authority. Validate each observation below.
				if installed.Owner == worker.addr {
					observations[installed.ID+"/"+fmt.Sprint(installed.Epoch)+"/"+installed.Owner.String()] = installed
				}
			}
		}
		leases, err := p.store.ListByWorker(ctx, worker.addr)
		if err != nil {
			return err
		}
		for _, lease := range leases {
			p.rebuildCall(lease)
		}
	}
	if routes, ok := p.store.(sessionstore.RouteLoader); ok {
		_, owners, err := routes.LoadRouteOwners(ctx, relay.DefaultMaxFlows)
		if err != nil {
			return err
		}
		for _, lease := range owners {
			p.rebuildCall(lease)
		}
	}
	intents, err := p.restart.store.ControlIntents(ctx)
	if err != nil {
		return err
	}
	for _, intent := range intents {
		if intent.Kind != "end" {
			if err := p.recovery.rebuild(ctx, intent, observations); err != nil {
				return err
			}
			continue
		}

		if intent.Candidate != nil {
			resolver, ok := p.store.(sessionstore.TransitionResolver)
			if !ok {
				return errors.New("controlplane: transition settlement unavailable")
			}
			if _, _, err := resolver.Settle(ctx, *intent.Candidate); err != nil {
				return err
			}
		}
		current, err := p.store.Get(ctx, intent.ID)
		if errors.Is(err, sessionstore.ErrEnded) || errors.Is(err, sessionstore.ErrNotFound) {
			if err := p.restart.store.DeleteControlIntent(ctx, intent.ID, intent.Token); err != nil {
				return err
			}
			p.restartOutcome(intent.Kind, intent.ID, "ended", "interrupted")
			continue
		}
		if err != nil {
			return err
		}
		c := p.rebuildCall(current)
		if err := func() error {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.intent = &intent
			if intent.Kind == "end" {
				// Commit the termination in the fenced store. The worker may have
				// already ended; never resend an unknown worker-side effect.
				if err := p.store.Release(ctx, current); err != nil {
					return err
				}
				ended, readErr := p.store.Get(ctx, intent.ID)
				if !errors.Is(readErr, sessionstore.ErrEnded) {
					if readErr != nil {
						return readErr
					}
					return sessionstore.ErrTransient
				}
				if cleaner, ok := p.relay.(TenureCleanupRelay); ok {
					if err := cleaner.ForgetSessionTenure(ctx, ended); err != nil {
						return err
					}
				}
				c.hungUp.Store(true)
				p.forget(c)
				p.deleteFrames(c.id)
				if err := p.restart.store.DeleteControlIntent(ctx, intent.ID, intent.Token); err != nil {
					return err
				}
				c.intent = nil
				p.restartOutcome(intent.Kind, intent.ID, "ended", "interrupted")
				return nil
			}
			return nil
		}(); err != nil {
			return err
		}
	}
	// Creation reservations remain durable until candidate settlement and live
	// worker observations establish adoption or fenced cleanup.
	if err := p.reconcileCreatesObserved(ctx, workerStatuses); err != nil {
		return err
	}
	// Drains keep their placement fence. A restart reports a typed partial result
	// instead of replaying the operator's sequence; a later Drain resumes it.
	for _, worker := range registrations {
		p.mu.Lock()
		draining := worker.observation.knowledge().draining
		p.mu.Unlock()
		if draining {
			p.restartOutcome("drain", worker.name, "partial", "interrupted")
		}
	}
	return nil
}
func (p *Plane) rebuildCall(lease sessionstore.Lease) *call {
	p.mu.Lock()
	defer p.mu.Unlock()
	c := p.calls[lease.SessionID]
	if c == nil {
		c = &call{id: lease.SessionID, lease: lease}
		p.calls[c.id] = c
	}
	return c
}

func (p *Plane) beginIntent(ctx context.Context, c *call, kind string, lease sessionstore.Lease) error {
	if p.restart == nil {
		return nil
	}
	if !p.controlAuthority() {
		return ErrNotReady
	}
	if c.intent != nil {
		if c.intent.Kind != kind && (kind == "end" || c.intent.Kind == "end") {
			updated := *c.intent
			updated.Kind = kind
			updated.Lease = lease
			if err := p.restart.store.PutControlIntent(ctx, updated); err != nil {
				return err
			}
			c.intent = &updated
		}
		return nil
	}
	intent := sessionstore.ControlIntent{ID: c.id, Token: randomToken(), Kind: kind, Started: time.Now(), Lease: lease}
	if err := p.restart.store.PutControlIntent(ctx, intent); err != nil {
		return err
	}
	c.intent = &intent
	return nil
}
func (p *Plane) finishIntent(c *call) {
	if p.restart == nil || c.intent == nil || !p.controlAuthority() {
		return
	}
	p.mu.Lock()
	pending := c.cleanupPending
	pending = pending || p.recovery.pendingLocked(c.id)
	p.mu.Unlock()
	if pending {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if p.restart.store.DeleteControlIntent(ctx, c.id, c.intent.Token) == nil {
		c.intent = nil
	}
}

func (p *Plane) startCreate(ctx context.Context, worker *registration) (string, error) {
	if p.restart == nil {
		return "", nil
	}
	worker.createMu.RLock()
	key := "create:" + randomToken()
	p.restart.mu.Lock()
	if p.restart.creates == nil {
		p.restart.creates = map[string]*pendingCreate{}
	}
	p.restart.creates[key] = &pendingCreate{worker: worker, active: true, reserved: true}
	p.restart.mu.Unlock()
	if err := p.controlRecord(ctx, key, createIntent{Worker: worker.name, Started: time.Now()}); err != nil {
		p.restart.mu.Lock()
		delete(p.restart.creates, key)
		p.restart.mu.Unlock()
		worker.createMu.RUnlock()
		return "", err
	}
	return key, nil
}

type pendingCreate struct {
	worker   *registration
	active   bool
	reserved bool
	settled  bool // a definite reply only needs a background journal clear
}

func (p *Plane) unknownCreateReservations(worker *registration) int {
	if p.restart == nil {
		return 0
	}
	p.restart.mu.Lock()
	defer p.restart.mu.Unlock()
	count := 0
	for _, pending := range p.restart.creates {
		if pending.worker == worker && !pending.active && pending.reserved {
			count++
		}
	}
	return count
}

func (p *Plane) finishCreate(_ context.Context, key string, worker *registration, _ string, cause error) error {
	if p.restart == nil {
		return nil
	}
	defer worker.createMu.RUnlock()
	definite := cause == nil || errors.Is(cause, ErrCreateNotSent) || !errors.Is(cause, privateapi.ErrUncertain) && (errors.Is(cause, mediaworker.ErrUnsupportedOffer) || errors.Is(cause, mediaworker.ErrDraining))
	if definite {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		err := p.controlRecord(ctx, key, nil)
		cancel()
		if err == nil {
			p.restart.mu.Lock()
			delete(p.restart.creates, key)
			p.restart.mu.Unlock()
			return nil
		}
		p.restartLog("create-clear", "create journal clear pending: %v", err)
	}
	p.restart.mu.Lock()
	if pending := p.restart.creates[key]; pending != nil {
		pending.active = false
		pending.settled = definite
		pending.reserved = !definite
	}
	p.restart.mu.Unlock()
	if definite {
		return nil
	} // the caller releases its reservation and keeps success
	return privateapi.Uncertain(cause)
}

func (p *Plane) reconcileCreates(ctx context.Context) error {
	return p.reconcileCreatesObserved(ctx, nil)
}

// A rebuild reuses failed observations even if a heartbeat arrived meanwhile.
// Heartbeats establish liveness, not an installed-session inventory.
func (p *Plane) reconcileCreatesObserved(ctx context.Context, observed map[string]mediaworker.WorkerStatus) error {
	lease, err := p.controlLease()
	if err != nil {
		return err
	}
	records, err := p.restart.store.ControlRecords(ctx, lease)
	if err != nil {
		return err
	}
	p.restart.mu.Lock()
	for key, local := range p.restart.creates {
		if !local.active {
			if _, ok := records[key]; !ok {
				records[key], _ = json.Marshal(createIntent{Worker: local.worker.name})
			}
		}
	}
	p.restart.mu.Unlock()
	for key, data := range records {
		if !strings.HasPrefix(key, "create:") {
			continue
		}
		var create createIntent
		if err := json.Unmarshal(data, &create); err != nil {
			return err
		}
		p.mu.Lock()
		worker := p.workers[create.Worker]
		p.mu.Unlock()
		if worker == nil {
			return fmt.Errorf("controlplane: create record references unconfigured worker %s", create.Worker)
		}
		// Never queue a writer behind an active create. Each worker has its own
		// observation cut, so a slow worker cannot block admission elsewhere.
		if !worker.createMu.TryLock() {
			continue
		}
		status, known := observed[worker.name]
		readStatus := observed == nil || known && status.Address == worker.addr
		err := p.reconcileCreate(ctx, key, worker, readStatus)
		worker.createMu.Unlock()
		if errors.Is(err, errCreateStatus) {
			// Rebuild must finish before heartbeat detection can establish death.
			// Keep this record reserved while observing the remaining records.
			p.restartLog("create-status:"+key, "create reconciliation record=%s worker=%s deferred: %v", key, worker.name, err)
			continue
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (p *Plane) reconcileCreate(ctx context.Context, key string, worker *registration, readStatus bool) error {
	p.restart.mu.Lock()
	local := p.restart.creates[key]
	settled := local != nil && local.settled
	p.restart.mu.Unlock()
	if !settled {
		if settler, ok := p.store.(interface {
			SettleWorkerTransitions(context.Context, netip.AddrPort) error
		}); ok {
			if err := settler.SettleWorkerTransitions(ctx, worker.addr); err != nil {
				return err
			}
		}
		leases, err := p.store.ListByWorker(ctx, worker.addr)
		if err != nil {
			return err
		}

		p.mu.Lock()
		disposition := worker.observation.knowledge().createDisposition
		dead := disposition == workerCreateRecover
		p.mu.Unlock()
		if !dead && (disposition == workerCreateWait || !readStatus) {
			// The record and reservation outlive this failed observation.
			// Normal ticks can observe again once a heartbeat proves liveness.
			return fmt.Errorf("%w: worker inventory not yet observed", errCreateStatus)
		}
		var status mediaworker.WorkerStatus
		var statusErr error
		if !dead {
			bounded, stop := context.WithTimeout(ctx, time.Second)
			status, statusErr = worker.worker.(*RemoteWorker).Status(bounded)
			stop()
			// Detection can establish death during the observation. Never
			// release committed claims if that happened while awaiting status.
			p.mu.Lock()
			dead = worker.observation.knowledge().createDisposition == workerCreateRecover
			p.mu.Unlock()
		}
		if dead {
			// The detector owns all committed claims, including a create whose
			// reply was lost. Never release a dead worker's recoverable calls.
			for _, lease := range leases {
				p.rebuildCall(lease)
			}
		} else {
			if statusErr != nil {
				// Only heartbeat expiry establishes death. Keep uncertain
				// admission reserved until observation succeeds or detection fires.
				return fmt.Errorf("%w: %w", errCreateStatus, statusErr)
			}
			if !status.InventoryKnown || status.Truncated || status.Address != worker.addr {
				return ErrNotReady
			}
			installed := map[string]mediaworker.InstalledSession{}
			for _, entry := range status.Inventory {
				if entry.Owner == worker.addr {
					installed[entry.ID] = entry
				}
			}
			intents, err := p.restart.store.ControlIntents(ctx)
			if err != nil {
				return err
			}
			moving := map[string]bool{}
			for _, intent := range intents {
				moving[intent.ID] = true
			}
			for _, lease := range leases {
				p.rebuildCall(lease)
				entry, ok := installed[lease.SessionID]
				if ok && entry.Owner == lease.Worker && entry.Epoch == lease.Epoch || moving[lease.SessionID] {
					continue
				}
				if err := p.store.Release(ctx, lease); err != nil {
					return err
				}
			}
		}
	}
	if err := p.controlRecord(ctx, key, nil); err != nil {
		return err
	}
	p.restart.mu.Lock()
	local = p.restart.creates[key]
	delete(p.restart.creates, key)
	delete(p.restart.logs, "create-status:"+key)
	p.restart.mu.Unlock()
	if local != nil && local.reserved {
		p.unreserve(local.worker)
	}
	p.restartOutcome("create", strings.TrimPrefix(key, "create:"), "reconciled", "interrupted")
	return nil
}
