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
	if m.targetWait != nil {
		return m.targetWait(ctx, changed, wait)
	}
	return waitTargetObservation(ctx, changed, wait, pending.targetDeadline)
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

// preserveSource runs under the call lock. A heartbeat alone is insufficient:
// the private inventory must identify the exact still-authoritative live tenure.
func (m *moveRecovery) preserveSource(ctx context.Context, source *registration, pending *moveTransfer) bool {
	p := m.plane
	p.mu.Lock()
	pending.sourceChecked = source.observation.lastHeartbeat
	alive := m.sourceAliveLocked(source, pending.firstDetection)
	p.mu.Unlock()
	if !alive || pending.planned || pending.held || pending.transferIssued || pending.candidate != nil ||
		pending.lease.Worker != source.addr || pending.routed != source.addr || pending.attempts != 0 {
		return false
	}
	bounded, stop := context.WithTimeout(ctx, 250*time.Millisecond)
	defer stop()
	status, err := readRecoveryInventory(bounded, source.worker)
	if err != nil || !completeRecoveryInventory(status, source) {
		return false
	}
	found := false
	for _, entry := range status.Inventory {
		if entry.ID == pending.lease.SessionID && entry.Owner == source.addr && entry.Epoch == pending.lease.Epoch {
			found = true
			break
		}
	}
	if !found {
		return false
	}
	lease, err := p.store.Get(bounded, pending.lease.SessionID)
	if err != nil || lease.Worker != pending.lease.Worker || lease.Epoch != pending.lease.Epoch {
		return false
	}
	p.mu.Lock()
	if !m.sourceAliveLocked(source, pending.firstDetection) {
		p.mu.Unlock()
		return false
	}
	// Suppress the drop-all rejoin handshake while any source calls survive.
	source.observation.preserving = true
	p.mu.Unlock()
	m.finishPending(source, lease.SessionID)
	return true
}

func (m *moveRecovery) sourceAliveLocked(source *registration, detected time.Time) bool {
	o := &source.observation
	return m.plane.workers[source.name] == source && o.lastHeartbeat.After(detected) &&
		m.observationTime().Sub(o.lastHeartbeat) < m.plane.config.DeadAfter
}

// restoreSource completes a cancelled death cycle only after all superseded
// sessions have self-fenced. It must never challenge a surviving call to drop.
func (m *moveRecovery) restoreSource(ctx context.Context, source *registration, remaining []sessionstore.Lease) bool {
	p := m.plane
	p.mu.Lock()
	preserving := source.observation.preserving
	p.mu.Unlock()
	if !preserving {
		return false
	}
	bounded, stop := context.WithTimeout(ctx, 250*time.Millisecond)
	defer stop()
	status, err := readRecoveryInventory(bounded, source.worker)
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
	if p.workers[source.name] != source || source.reserved != 0 || p.recovery.hasWorkLocked(source, recoveryAll) ||
		m.observationTime().Sub(source.observation.lastHeartbeat) >= p.config.DeadAfter {
		return false
	}
	o := &source.observation
	o.observeStatus(status, false, false)
	o.dead, o.recovering, o.preserving, o.livenessUnknown = false, false, false, false
	o.rejoinReady, o.rejoinToken, o.acceptedToken = false, "", o.deathToken
	p.loadVersion++
	p.observationChangedLocked()
	return true
}

func completeRecoveryInventory(status mediaworker.WorkerStatus, source *registration) bool {
	return status.InventoryKnown && !status.Truncated && status.Address == source.addr
}

func readRecoveryInventory(ctx context.Context, worker Worker) (mediaworker.WorkerStatus, error) {
	if remote, ok := worker.(interface {
		Status(context.Context) (mediaworker.WorkerStatus, error)
	}); ok {
		return remote.Status(ctx)
	}
	// In-process workers expose the same existing private inventory handler.
	// No listening socket or worker-side API change is needed.
	local, ok := worker.(interface{ PrivateHandler() http.Handler })
	if !ok {
		return mediaworker.WorkerStatus{}, ErrNotReady
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://worker/status", nil)
	if err != nil {
		return mediaworker.WorkerStatus{}, err
	}
	reply := &inventoryResponse{header: make(http.Header)}
	local.PrivateHandler().ServeHTTP(reply, req)
	if reply.status != http.StatusOK {
		return mediaworker.WorkerStatus{}, ErrNotReady
	}
	var status mediaworker.WorkerStatus
	err = json.Unmarshal(reply.body.Bytes(), &status)
	return status, err
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
