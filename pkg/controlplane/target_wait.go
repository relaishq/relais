package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/sessionstore"
)

// ErrTargetWaitExpired is a definite recovery outcome, not a store failure.
var ErrTargetWaitExpired = errors.New("controlplane: target observation wait expired")

// Detection (450 ms) plus adoption (1.5 s) stays below the 2 s crash gate.
const targetOutageBudget = 1950 * time.Millisecond

func (p *Plane) observationChangedLocked() {
	close(p.workerChanged)
	p.workerChanged = make(chan struct{})
}

func (m *moveRecovery) observationTime() time.Time {
	if m.targetNow != nil {
		return m.targetNow()
	}
	return time.Now()
}

func (m *moveRecovery) waitForTarget(ctx context.Context, pending *moveTransfer) error {
	remaining := pending.targetDeadline.Sub(m.observationTime())
	if remaining <= 0 {
		return ErrTargetWaitExpired
	}
	p := m.plane
	p.mu.Lock()
	changed := p.workerChanged
	p.mu.Unlock()
	wait := min(remaining, p.config.CheckInterval)
	var err error
	if m.targetWait != nil {
		err = m.targetWait(ctx, changed, wait)
	} else {
		err = waitTargetObservation(ctx, changed, wait, pending.targetDeadline)
	}
	// A timer or observation can win the select at D before ctx.Done. Finish
	// the known no-target outcome instead of entering selection with expiry.
	if err == nil && !m.observationTime().Before(pending.targetDeadline) {
		return ErrTargetWaitExpired
	}
	return err
}

func waitTargetObservation(ctx context.Context, changed <-chan struct{}, wait time.Duration, deadline time.Time) error {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) && !time.Now().Before(deadline) {
			return ErrTargetWaitExpired
		}
		return ctx.Err()
	case <-changed:
	case <-timer.C:
	}
	return nil
}

// preserveSource makes one worker-level decision. The registry lock serializes
// its final publication with every Transfer-issued marker; no I/O holds it.
func (m *moveRecovery) preserveSource(ctx context.Context, source *registration, pending *moveTransfer) bool {
	if !pending.unrequested {
		return false
	}
	p := m.plane
	p.mu.Lock()
	if source.observation.deathPreserved && source.observation.deathCycle == pending.deathCycle {
		p.mu.Unlock()
		return true
	}
	// D bounds waiting and preservation, not adoption of an eligible target.
	// A queued call must still use the existing adoption budgets after D.
	if !pending.targetDeadline.IsZero() && !m.observationTime().Before(pending.targetDeadline) {
		pending.sourceNeedsHeartbeat = false
		p.mu.Unlock()
		return false
	}
	alive := m.sourceAliveLocked(source, pending.firstDetection)
	pending.sourceNeedsHeartbeat = !alive
	pending.sourceChecked = source.observation.lastHeartbeat
	allowed := !source.observation.deathTransferIssued && source.observation.deathCycle == pending.deathCycle
	for _, other := range m.pending[source] {
		allowed = allowed && other.unrequested && other.candidate == nil && !other.transferIssued
	}
	p.mu.Unlock()
	if !alive || !allowed {
		return false
	}
	// A failed/slow inventory check must not make ordinary heartbeats restart
	// selection forever. Only a prior rejection for liveness needs that retry.
	pending.sourceNeedsHeartbeat = false
	deadline := time.Now().Add(250 * time.Millisecond)
	if !pending.targetDeadline.IsZero() && pending.targetDeadline.Before(deadline) {
		deadline = pending.targetDeadline
	}
	bounded, stop := context.WithDeadline(ctx, deadline)
	defer stop()
	remaining, err := p.store.ListByWorker(bounded, source.addr)
	if err != nil {
		return false
	}
	status, err := source.recoveryInventory(bounded)
	if err != nil || !completeRecoveryInventory(status, source) || len(status.Inventory) != len(remaining) {
		return false
	}
	leases := make(map[string]sessionstore.Lease, len(remaining))
	for _, lease := range remaining {
		leases[lease.SessionID] = lease
	}
	for _, entry := range status.Inventory {
		lease, ok := leases[entry.ID]
		if !ok || entry.Owner != lease.Worker || entry.Epoch != lease.Epoch {
			return false
		}
		current, err := p.store.Get(bounded, entry.ID)
		if err != nil || current.Worker != lease.Worker || current.Epoch != lease.Epoch {
			return false
		}
		delete(leases, entry.ID)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if source.observation.deathPreserved && source.observation.deathCycle == pending.deathCycle {
		return true
	}
	if !m.sourceAliveLocked(source, pending.firstDetection) || source.observation.deathTransferIssued ||
		source.observation.deathCycle != pending.deathCycle || source.reserved != 0 || bounded.Err() != nil {
		return false
	}
	for _, other := range m.pending[source] {
		if !other.unrequested || other.candidate != nil || other.transferIssued {
			return false
		}
	}
	for _, other := range m.pending[source] {
		if other.call != nil {
			source.preservedIntents = append(source.preservedIntents, other.call)
		}
	}
	delete(m.pending, source)
	o := &source.observation
	o.observeStatus(status, false, false)
	o.dead, o.recovering, o.livenessUnknown, o.deathPreserved = false, false, false, true
	o.recovered = true
	o.rejoinReady, o.rejoinToken, o.acceptedToken = false, "", o.deathToken
	p.loadVersion++
	p.observationChangedLocked()
	return true
}

func (m *moveRecovery) sourceAliveLocked(source *registration, detected time.Time) bool {
	o := &source.observation
	return m.plane.workers[source.name] == source && o.deathReturned && o.lastHeartbeat.After(detected) &&
		m.observationTime().Sub(o.lastHeartbeat) < m.plane.config.DeadAfter
}

// Other call locks must be acquired only after this worker's recovery jobs end.
func (m *moveRecovery) finishPreservedIntents(ctx context.Context, source *registration) {
	p := m.plane
	p.mu.Lock()
	calls := source.preservedIntents
	source.preservedIntents = nil
	p.mu.Unlock()
	for _, c := range calls {
		if err := c.lockOperator(ctx, false); err == nil {
			p.finishIntent(c)
			c.mu.Unlock()
		}
	}
}

func completeRecoveryInventory(status mediaworker.WorkerStatus, source *registration) bool {
	return status.InventoryKnown && !status.Truncated && status.Address == source.addr
}

func (source *registration) recoveryInventory(ctx context.Context) (mediaworker.WorkerStatus, error) {
	source.inventoryOnce.Do(func() { source.inventoryReader = recoveryInventoryReader(source.worker) })
	return source.inventoryReader(ctx)
}

func recoveryInventoryReader(worker Worker) func(context.Context) (mediaworker.WorkerStatus, error) {
	if remote, ok := worker.(interface {
		Status(context.Context) (mediaworker.WorkerStatus, error)
	}); ok {
		return remote.Status
	}
	// In-process workers expose the same existing private inventory handler.
	// No listening socket or worker-side API change is needed.
	local, ok := worker.(interface{ PrivateHandler() http.Handler })
	if !ok {
		return func(context.Context) (mediaworker.WorkerStatus, error) {
			return mediaworker.WorkerStatus{}, ErrNotReady
		}
	}
	handler := local.PrivateHandler()
	return func(ctx context.Context) (mediaworker.WorkerStatus, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://worker/status", nil)
		if err != nil {
			return mediaworker.WorkerStatus{}, err
		}
		reply := &inventoryResponse{header: make(http.Header)}
		handler.ServeHTTP(reply, req)
		if reply.status != http.StatusOK {
			return mediaworker.WorkerStatus{}, ErrNotReady
		}
		var status mediaworker.WorkerStatus
		err = json.Unmarshal(reply.body.Bytes(), &status)
		return status, err
	}
}

type inventoryResponse struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func (r *inventoryResponse) Header() http.Header    { return r.header }
func (r *inventoryResponse) WriteHeader(status int) { r.status = status }
func (r *inventoryResponse) Write(data []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	if r.body.Len()+len(data) > 1<<20 {
		return 0, ErrNotReady
	}
	return r.body.Write(data)
}
