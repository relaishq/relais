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
var ErrNotReady = fmt.Errorf("controlplane: restart not ready: %w", agentv2.ErrControlNotReady)

const ControlLeaseTTL = 2 * time.Second

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
	createMu       sync.RWMutex
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
	p.restart = &restartState{store: store, status: RebuildStatus{Phase: "waiting_lease", Started: time.Now(), Outcomes: []RestartOutcome{}}}
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
	return p.restart == nil || p.restart.ready.Load() && p.controlAuthority()
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
	}
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

// RunManaged acquires authority, rebuilds, then enables detection. Any failed
// renewal removes readiness immediately and stops the detector. This is one
// active process, not a passive/active authority-generation protocol.
func (p *Plane) RunManaged(ctx context.Context, workers []FixedWorker) error {
	if p.restart == nil {
		return errors.New("controlplane: EnableRestart required")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	holder := randomToken()
	var lease sessionstore.ControlLease
	for {
		started := time.Now()
		var err error
		lease, err = p.restart.store.ClaimControl(ctx, holder, ControlLeaseTTL)
		if err == nil {
			// A delayed reply never extends the locally safe authority interval.
			deadline := started.Add(ControlLeaseTTL)
			if !time.Now().Before(deadline) {
				continue
			}
			p.restart.guard.Store(&controlGuard{lease: lease, deadline: deadline})
			break
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !errors.Is(err, sessionstore.ErrLeaseHeld) && !errors.Is(err, sessionstore.ErrTransient) {
			p.rebuildPhase("failed", err)
			return err
		}
		p.rebuildPhase("waiting_lease", err)
		if !restartWait(ctx, 50*time.Millisecond) {
			return ctx.Err()
		}
	}
	p.restart.mu.Lock()
	p.restart.status.LeaseWait = time.Since(p.restart.status.Started)
	p.restart.mu.Unlock()
	p.rebuildPhase("rebuilding", nil)
	renewDone := make(chan struct{})
	go func() {
		defer close(renewDone)
		ticker := time.NewTicker(ControlLeaseTTL / 4)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				started := time.Now()
				guard := p.restart.guard.Load()
				if guard == nil {
					return
				}
				bounded, stop := context.WithTimeout(ctx, ControlLeaseTTL/4)
				next, err := p.restart.store.RenewControl(bounded, guard.lease, ControlLeaseTTL)
				stop()
				if err != nil || !started.Before(guard.deadline) || !time.Now().Before(guard.deadline) {
					p.restart.ready.Store(false)
					p.restart.guard.Store(nil)
					if err == nil {
						err = sessionstore.ErrLeaseLost
					}
					p.rebuildPhase("lease_lost", err)
					cancel()
					return
				}
				p.restart.guard.Store(&controlGuard{lease: next, deadline: started.Add(ControlLeaseTTL)})
			}
		}
	}()
	defer func() {
		p.restart.ready.Store(false)
		cancel()
		<-renewDone
		// Caller shuts down HTTP before relinquishing this process's authority.
		// Expiry remains the fallback if the store is unavailable during shutdown.
	}()
	for {
		err := p.rebuild(ctx, workers)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		p.rebuildPhase("rebuilding", err)
		if !restartWait(ctx, 100*time.Millisecond) {
			return ctx.Err()
		}
	}
	p.restart.mu.Lock()
	p.restart.status.Phase = "ready"
	p.restart.status.Error = ""
	p.restart.status.ReadyAt = time.Now()
	p.restart.status.Duration = time.Since(p.restart.status.Started)
	p.restart.mu.Unlock()
	p.restart.ready.Store(true)
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
				for _, worker := range workers {
					_ = p.discoverWorker(ctx, worker, nil)
				}
				_ = p.reconcileCreates(ctx)
			}
		}
	}()
	err := p.Run(ctx)
	cancel()
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
	p.restart.registrationMu.Lock()
	defer p.restart.registrationMu.Unlock()
	p.mu.Lock()
	old := p.workers[fixed.Name]
	p.mu.Unlock()
	if old != nil {
		if err == nil && status.Address != old.addr {
			// A changed private leg is a replacement, never proof its old calls ended.
			p.mu.Lock()
			canReplace := old.dead && old.recovered && !old.recovering
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
	if down {
		registration.lastHeartbeat = time.Time{}
		registration.dead = true
	} else {
		registration.draining = status.Draining
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
			return fmt.Errorf("controlplane: saved worker %s missing from fixed configuration", name)
		}
	}
	var wg sync.WaitGroup
	failures := make(chan error, len(fixed))
	for _, worker := range fixed {
		wg.Go(func() {
			var old *FixedWorker
			if value, ok := saved[worker.Name]; ok {
				old = &value
			}
			if err := p.discoverWorker(ctx, worker, old); err != nil {
				failures <- err
			}
		})
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		return err
	}
	p.mu.Lock()
	registrations := make([]*registration, 0, len(p.workers))
	for _, worker := range p.workers {
		registrations = append(registrations, worker)
		if _, ok := records["drain:"+worker.name]; ok {
			worker.draining = true
		}
	}
	relayReady := p.relayReady
	p.mu.Unlock()
	if !relayReady {
		return ErrNotReady
	}
	observations := map[string]mediaworker.InstalledSession{}
	for _, worker := range registrations {
		bounded, stop := context.WithTimeout(ctx, time.Second)
		status, statusErr := worker.worker.(*RemoteWorker).Status(bounded)
		stop()
		if statusErr == nil && status.Address == worker.addr {
			if !status.InventoryKnown || status.Truncated {
				return fmt.Errorf("controlplane: worker %s inventory unavailable or truncated", worker.name)
			}
			p.mu.Lock()
			worker.draining = worker.draining || status.Draining
			p.mu.Unlock()
			if status.Draining {
				if err := p.controlRecord(ctx, "drain:"+worker.name, true); err != nil {
					return err
				}
			}
			for _, installed := range status.Inventory {
				lease, err := p.store.Get(ctx, installed.ID)
				if err == nil {
					p.rebuildCall(lease)
				} else if !errors.Is(err, sessionstore.ErrNotFound) && !errors.Is(err, sessionstore.ErrEnded) {
					return err
				}
				// The inventory cannot grant authority. Validate each observation below.
				observations[installed.ID+"/"+fmt.Sprint(installed.Epoch)+"/"+installed.Owner.String()] = installed
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
		_, owners, err := routes.LoadRouteOwners(ctx, sessionstore.MaxControlIntents)
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
			continue
		}

		installed, ok := observations[current.SessionID+"/"+fmt.Sprint(current.Epoch)+"/"+current.Worker.String()]
		if ok && installed.Owner == current.Worker {
			// Route repair/release is idempotent coordination. Never re-export or
			// re-resume a tenure that the store and live worker both confirm.
			if err := p.relay.MoveSession(intent.ID, netip.AddrPort{}, current.Worker); err != nil {
				return err
			}
			if _, err := p.relay.ReleaseSession(intent.ID, current.Worker); err != nil && !errors.Is(err, privateapi.ErrUncertain) {
				// No hold is normal when the operation stopped before its barrier.
				// The relay backstop also expires holds while control is absent.
				if !errors.Is(err, relay.ErrHoldExpired) {
					return err
				}
			}
			if err := p.restart.store.DeleteControlIntent(ctx, intent.ID, intent.Token); err != nil {
				return err
			}
			c.intent = nil
			p.restartOutcome(intent.Kind, intent.ID, "live", "interrupted")
			continue
		}
		// Missing installation is not a failed resume proof. Transfer away from the
		// current tenure before any new resume, using the existing tuple read and
		// bounded crash-margin recovery. A delayed old adoption cannot own that epoch.
		p.mu.Lock()
		source := p.owner(current.Worker)
		p.mu.Unlock()
		if source == nil {
			return fmt.Errorf("controlplane: unknown persisted owner %s", current.Worker)
		}
		c.mu.Lock()
		p.mu.Lock()
		for _, worker := range p.workers {
			delete(worker.pending, c.id)
		}
		if source.pending == nil {
			source.pending = map[string]*takeoverState{}
		}
		source.pending[c.id] = &takeoverState{call: c, lease: current, routed: current.Worker, routedLease: current, excluded: map[netip.AddrPort]bool{current.Worker: true}, attemptLimit: maxResumeAttempts, held: true, outageStarted: intent.Started}
		p.mu.Unlock()
		bounded, stop := context.WithTimeout(ctx, takeoverBudget)
		result := p.takeoverLocked(bounded, source, c, current, time.Now())
		stop()
		c.mu.Unlock()
		if result == nil {
			return ErrNotReady
		}
		outcome, code := "live", "interrupted"
		if result.Lost {
			outcome, code = "lost", result.LossCause
		}
		p.restartOutcome(intent.Kind, intent.ID, outcome, code)
	}
	// Creation reservations remain durable until candidate settlement and live
	// worker observations establish adoption or fenced cleanup.
	if err := p.reconcileCreates(ctx); err != nil {
		return err
	}
	// Drains keep their placement fence. A restart reports a typed partial result
	// instead of replaying the operator's sequence; a later Drain resumes it.
	for _, worker := range registrations {
		p.mu.Lock()
		draining := worker.draining
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
		if c.intent.Kind == "end" && kind != "end" {
			return ErrMoveInProgress
		}
		if kind == "end" && c.intent.Kind != "end" {
			updated := *c.intent
			updated.Kind = "end"
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
	for _, worker := range p.workers {
		pending = pending || worker.pending[c.id] != nil
	}
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
	p.restart.createMu.RLock()
	key := "create:" + randomToken()
	p.restart.mu.Lock()
	if p.restart.creates == nil {
		p.restart.creates = map[string]*pendingCreate{}
	}
	p.restart.creates[key] = &pendingCreate{worker: worker, active: true}
	p.restart.mu.Unlock()
	if err := p.controlRecord(ctx, key, createIntent{Worker: worker.name, Started: time.Now()}); err != nil {
		p.restart.mu.Lock()
		delete(p.restart.creates, key)
		p.restart.mu.Unlock()
		p.restart.createMu.RUnlock()
		return "", err
	}
	return key, nil
}

type pendingCreate struct {
	worker *registration
	active bool
}

func (p *Plane) finishCreate(_ context.Context, key string, worker *registration, id string, cause error) error {
	if p.restart == nil {
		return nil
	}
	defer p.restart.createMu.RUnlock()
	definite := cause == nil || errors.Is(cause, mediaworker.ErrUnsupportedOffer) || errors.Is(cause, mediaworker.ErrDraining) && !errors.Is(cause, privateapi.ErrUncertain)
	if definite {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := p.controlRecord(ctx, key, nil); err == nil {
			p.restart.mu.Lock()
			delete(p.restart.creates, key)
			p.restart.mu.Unlock()
			return nil
		}
	}
	p.restart.mu.Lock()
	if pending := p.restart.creates[key]; pending != nil {
		pending.active = false
	}
	p.restart.mu.Unlock()
	// Keep the incoming reservation. The detector owns its reconciliation now.
	if cause == nil {
		cause = fmt.Errorf("controlplane: create %s accepted on %s; journal settlement pending", id, worker.name)
	}
	return privateapi.Uncertain(cause)
}

func (p *Plane) reconcileCreates(ctx context.Context) error {
	// Prevent a new create from publishing between the observation cut and
	// cleanup of an older unknown request. Parallel ordinary creates share RLock.
	p.restart.createMu.Lock()
	defer p.restart.createMu.Unlock()
	lease, err := p.controlLease()
	if err != nil {
		return err
	}
	records, err := p.restart.store.ControlRecords(ctx, lease)
	if err != nil {
		return err
	}
	// A delete may commit while its reply is lost. The local reservation still
	// needs the same authoritative observation cut even if its record is absent.
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
		p.restart.mu.Lock()
		active := false
		for _, local := range p.restart.creates {
			if local.worker.name == create.Worker && local.active {
				active = true
				break
			}
		}
		p.restart.mu.Unlock()
		if active {
			continue
		}
		p.mu.Lock()
		worker := p.workers[create.Worker]
		p.mu.Unlock()
		if worker == nil {
			return ErrNotReady
		}
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
		bounded, stop := context.WithTimeout(ctx, time.Second)
		status, statusErr := worker.worker.(*RemoteWorker).Status(bounded)
		stop()
		if statusErr != nil && len(leases) != 0 {
			return statusErr
		}
		if statusErr == nil && (!status.InventoryKnown || status.Truncated || status.Address != worker.addr) {
			return ErrNotReady
		}
		installed := map[string]mediaworker.InstalledSession{}
		for _, entry := range status.Inventory {
			installed[entry.ID] = entry
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
			if ok && entry.Owner == lease.Worker && entry.Epoch == lease.Epoch {
				continue
			}
			if moving[lease.SessionID] {
				continue
			}
			// No effect is retried. A fenced release disposes of an uninstalled claim;
			// any late installation then fails ownership checks/renewal.
			if err := p.store.Release(ctx, lease); err != nil {
				return err
			}
		}
		if err := p.controlRecord(ctx, key, nil); err != nil {
			return err
		}
		p.restart.mu.Lock()
		local := p.restart.creates[key]
		delete(p.restart.creates, key)
		p.restart.mu.Unlock()
		if local != nil {
			p.unreserve(local.worker)
		}
		p.restartOutcome("create", strings.TrimPrefix(key, "create:"), "reconciled", "interrupted")
	}
	return nil
}
