package mediaworker

import "time"

// BridgePermitCap is pool-wide in the isolated loopback profile. A mixed pool
// must establish its maximum cap before it can shorten a replacement wait.
const BridgePermitCap = 5 * time.Second

type bridgePermitCache struct {
	scope    BridgeOutputScope
	deadline time.Time
	stopped  time.Time
}

// BridgePermitRemaining reads only the existing successful-renewal cache.
// It never reads the store, asks its clock, or retries a renewal.
func (w *Worker) BridgePermitRemaining(scope BridgeOutputScope) time.Duration {
	s, err := w.bridgeSession(scope.SessionID)
	if err != nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.agent.v2.bridge
	if s.ctx.Err() != nil || s.fenced.Load() || s.agent.paused || b.state == nil || b.durable == nil || b.checkpointSeq == 0 ||
		BridgeOutputScope(b.state.Scope) != scope || BridgeOutputScope(b.durable.Scope) != scope || scope.Epoch != s.lease.Epoch ||
		b.permit.scope != scope || !b.permit.stopped.IsZero() {
		return 0
	}
	return max(0, time.Until(b.permit.deadline))
}

// StopBridgePermits freezes grants, including in-flight renewal replies, before
// revocation. The stable boundary covers queued permit replies as well.
func (w *Worker) StopBridgePermits(scope BridgeOutputScope) (time.Time, error) {
	s, err := w.bridgeSession(scope.SessionID)
	if err != nil {
		return time.Time{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.agent.v2.bridge
	if b.state == nil || BridgeOutputScope(b.state.Scope) != scope {
		return time.Time{}, ErrBridgeOutputScope
	}
	// Once export has paused the actor, its cut wins. The caller must follow
	// the move rather than create a source-only revocation after that cut.
	if s.agent.paused || s.fenced.Load() {
		return time.Time{}, ErrBridgeOutputMoving
	}
	if b.permit.stopped.IsZero() {
		b.permit.stopped = time.Now()
	}
	b.permit.deadline = time.Time{}
	return b.permit.stopped.Add(BridgePermitCap), nil
}

// Called under session.mu on the result of the pre-existing Renew only.
func (s *session) cacheBridgeRenewal(scope BridgeOutputScope, start time.Time, success bool) {
	if !success || s.agent == nil || s.agent.v2 == nil || s.agent.v2.bridge == nil {
		return
	}
	b := s.agent.v2.bridge
	if b.state == nil || scope != (BridgeOutputScope(b.state.Scope)) || scope.Epoch != s.lease.Epoch || !b.permit.stopped.IsZero() {
		return
	}
	b.permit.scope = scope
	b.permit.deadline = start.Add(min(BridgePermitCap, s.worker.cfg.Relay.LeaseTTL))
}
