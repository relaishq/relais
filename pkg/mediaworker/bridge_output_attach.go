package mediaworker

import "slices"

// BridgeOutputBinding returns an owned identity for runner assignment. It is
// not authorization: attach must still prove its token and exact bot identity.
func (w *Worker) BridgeOutputBinding(id string) (BridgeOutputScope, error) {
	s, err := w.bridgeSession(id)
	if err != nil {
		return BridgeOutputScope{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.agent.v2.bridge.state == nil {
		return BridgeOutputScope{}, ErrBridgeOutputUnbound
	}
	return BridgeOutputScope(s.agent.v2.bridge.state.Scope), nil
}

// ReattachBridgeOutput transfers an exact ledger to this owner epoch. It never
// authorizes a replacement or a crash tail. The authenticated runner must keep
// the same bot, generation and lineage, and supply its last-issued sequence.
func (w *Worker) ReattachBridgeOutput(scope BridgeOutputScope, last uint64) error {
	s, err := w.bridgeSession(scope.SessionID)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.agent.v2.bridge
	if err := s.ctx.Err(); err != nil {
		return err
	}
	if b.state == nil {
		return ErrBridgeOutputUnbound
	}
	old := BridgeOutputScope(b.state.Scope)
	expected := old
	expected.Epoch = scope.Epoch
	if !scope.valid(s.id) || scope != expected || scope.Epoch != s.lease.Epoch || scope.Epoch < old.Epoch {
		return ErrBridgeOutputScope
	}
	if s.fenced.Load() || s.agent.paused {
		return ErrBridgeOutputMoving
	}
	if b.state.Stopped {
		return ErrBridgeRevoked
	}
	if !b.provenUnadmitted || last < b.state.Through {
		return ErrBridgeOutputUncertain
	}
	if scope == old {
		return nil
	}
	v := cloneBridgeState(b.state)
	v.Scope = bridgeOutputIdentity(scope)
	v.Revision++
	// Values and range headers are copied. Frozen source images keep their old
	// scope and digest chains; immutable payloads and packed data remain shared.
	for seq, r := range b.records {
		previous := r
		r.Scope = scope
		r.Receipt.BridgeOutputScope = scope
		b.replaceRecordEncoding(&previous, &r)
		b.records[seq] = r
	}
	b.settled = slices.Clone(b.settled)
	for i := range b.settled {
		r := &b.settled[i]
		previous := *r
		r.Scope = bridgeOutputIdentity(scope)
		chain, revision, data := r.Seed, r.BaseRevision, r.Data
		for seq := r.First; seq <= r.Last; seq++ {
			digest, delta, n := bridgeSettledEntry(data)
			revision += delta
			data = data[n:]
			chain = bridgeSettledChain(r.Scope, chain, seq, r.Turn, r.Stream, revision, digest)
		}
		r.Chain = chain
		b.replaceSettledCost(&previous, r)
	}
	b.state = v
	b.durable = nil
	b.checkpointSeq = 0
	s.projectBridgeLocked(s.state.Agent.V2)
	return nil
}
