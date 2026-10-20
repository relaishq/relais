package mediaworker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/sessionstore"
)

// The private v10 envelope is self-contained. Metadata is an optional expected
// publication token, never a Redis key or a substitute for component bytes.
type componentSnapshot struct {
	Version       int
	Components    sessionstore.Components
	StateVersion  uint32
	Metadata      *sessionstore.ComponentMetadata `json:",omitempty"`
	AgentMoveOnly *movePayload                    `json:",omitempty"`
}
type componentSnapshotState struct {
	history  []byte
	metadata *sessionstore.ComponentMetadata
}

func encodeComponentSnapshot(snap snapshot) ([]byte, error) {
	c, err := splitSessionComponents(snap, snap.split.history, nil)
	if err != nil {
		return nil, err
	}
	return json.Marshal(componentSnapshot{Version: 10, Components: sessionstore.Components(c), StateVersion: snap.State.Agent.State.Version, Metadata: snap.split.metadata, AgentMoveOnly: snap.AgentMoveOnly})
}
func decodeComponentSnapshot(data []byte) (*snapshot, error) {
	var wire componentSnapshot
	if err := json.Unmarshal(data, &wire); err != nil {
		return nil, fmt.Errorf("%w: %w", errBadState, err)
	}
	if wire.Version != 10 {
		return nil, errStateVersion
	}
	snap, history, err := assembleSessionComponents(sessionComponents(wire.Components), wire.StateVersion)
	if err != nil {
		return nil, err
	}
	snap.AgentMoveOnly = wire.AgentMoveOnly
	if err := validateSnapshot(&snap); err != nil {
		return nil, err
	}
	snap.split = &componentSnapshotState{history: history, metadata: wire.Metadata}
	return &snap, nil
}

// ComponentSnapshot assembles the already authenticated tuple for the existing
// private restore boundary. The exact publication token travels with it.
func ComponentSnapshot(tuple sessionstore.ComponentTuple) ([]byte, error) {
	snap, history, err := assembleSessionComponents(sessionComponents(tuple.Components), tuple.Metadata.StateVersion)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", sessionstore.ErrUnsafeCheckpoint, err)
	}
	if snap.State.ID != tuple.Lease.SessionID {
		return nil, sessionstore.ErrUnsafeCheckpoint
	}
	metadata := tuple.Metadata
	snap.split = &componentSnapshotState{history: history, metadata: &metadata}
	return json.Marshal(snap)
}

// ReadRecoverySnapshot selects the additive API without trying a legacy read
// for a v10 lease. Unsafe, missing and transient tuple reads never fall back.
// Legacy stores/wrappers retain the original GetState path and its seams.
func ReadRecoverySnapshot(ctx context.Context, store sessionstore.Store, id string) ([]byte, *sessionstore.Checkpoint, error) {
	started := time.Now()
	if components, ok := store.(sessionstore.ComponentStore); ok {
		tuple, err := components.ReadComponents(ctx, id)
		if err == nil {
			data, err := ComponentSnapshot(tuple)
			checkpoint := tuple.Checkpoint
			checkpoint.Age += time.Since(started)
			return data, &checkpoint, err
		}
		if !errors.Is(err, sessionstore.ErrCheckpointLayout) && !errors.Is(err, sessionstore.ErrNotFound) {
			return nil, nil, err
		}
	}
	data, err := store.GetState(ctx, id)
	return data, nil, err
}

func (s *session) componentBudgets() componentBudgets {
	hot := s.snapshotLimits().MaxBytes
	history := 8192
	if s.agent != nil && s.agent.v2 != nil {
		history = s.agent.v2.registration.Bounds.MaxHistoryBytes
	}
	return splitComponentBudgets(hot, history)
}
func (s *session) componentAdmission(data []byte) (componentSizes, error) {
	var wire componentSnapshot
	if err := json.Unmarshal(data, &wire); err != nil {
		return componentSizes{}, err
	}
	m := componentMetadata{Format: 10, Owner: s.worker.localAddr.String()}
	return s.componentBudgets().admit(sessionComponents(wire.Components), m)
}

// A pending write may have committed while settlement was unavailable. Keep
// its exact owned capture until a tuple read proves that capture or the old
// publication. A late publication from this tenure can update the expected
// root without acknowledging the pending app pair or its DTLS capture.
type componentPending struct {
	components sessionstore.Components
	expected   *sessionstore.ComponentMetadata
	pair       agentState
	next       dtlsSequenceCheckpoint
	captured   time.Time
	bytes      int
	intent     bool
}

func equalComponentBytes(a, b sessionstore.Components) bool {
	return bytes.Equal(a.Setup, b.Setup) && bytes.Equal(a.Transport, b.Transport) && bytes.Equal(a.Work, b.Work) && bytes.Equal(a.History, b.History)
}
func sameComponentMetadata(a, b *sessionstore.ComponentMetadata) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
func (s *session) settleComponentWrite(ctx context.Context) error {
	pending := s.componentPending
	if pending == nil {
		return nil
	}
	store, ok := s.worker.cfg.Relay.Owners.(sessionstore.ComponentStore)
	if !ok {
		return sessionstore.ErrCheckpointLayout
	}
	tuple, err := store.ReadComponents(ctx, s.id)
	if errors.Is(err, sessionstore.ErrCheckpointLayout) && pending.expected == nil {
		s.failComponentPending()
		return nil
	}
	if err != nil {
		return err
	}
	lease := s.currentComponentLease()
	if tuple.Lease.Worker != lease.Worker || tuple.Lease.Epoch != lease.Epoch {
		s.failComponentPending()
		return sessionstore.ErrLeaseLost
	}
	if equalComponentBytes(tuple.Components, pending.components) {
		s.componentMetadata = &tuple.Metadata
		s.ackSnapshot(pending.pair, pending.next, pending.captured, pending.bytes, pending.intent)
		s.componentPending = nil
		return nil
	}
	if sameComponentMetadata(&tuple.Metadata, pending.expected) {
		s.failComponentPending()
		return nil
	}
	// An earlier command can land after its settlement read. Rebase only onto
	// our own tenure. This is ordering evidence, not an acknowledgment of the
	// pending capture or its DTLS bound.
	if tuple.Metadata.Owner == lease.Worker.String() && tuple.Metadata.OwnerEpoch == lease.Epoch {
		s.componentMetadata = &tuple.Metadata
		s.failComponentPending()
		return nil
	}
	return sessionstore.ErrStateSuperseded
}

func (s *session) failComponentPending() {
	pending := s.componentPending
	s.componentPending = nil
	s.snapshotBudgetMu.Lock()
	s.snapshotWriteInFlight = false
	s.snapshotBudgetMu.Unlock()
	if pending != nil {
		s.failSnapshot(pending.pair.V2 != nil, pending.intent)
	}
}

func componentBudgetRefusal(intent bool) error {
	if intent {
		return errCheckpointSkipped
	}
	return fmt.Errorf("%w: required component snapshot byte budget", agent.ErrCapacity)
}

func (s *session) writeComponents(ctx context.Context, lease sessionstore.Lease, data []byte, pair agentState, next dtlsSequenceCheckpoint, captured time.Time, intent bool) (int, error) {
	store, ok := s.worker.cfg.Relay.Owners.(sessionstore.ComponentStore)
	if !ok {
		return 0, sessionstore.ErrCheckpointLayout
	}
	var wire componentSnapshot
	if err := json.Unmarshal(data, &wire); err != nil {
		return 0, err
	}
	if _, err := s.componentAdmission(data); err != nil {
		return 0, err
	}
	available := 0
	historyHeld := false
	if pair.V2 != nil {
		s.snapshotBudgetMu.Lock()
		available = int(s.snapshotBytesBudget.available(s.snapshotLimits(), captured))
		if hold := s.snapshotHistoryHold; hold != nil {
			historyHeld = true
			if !hold.matches(pair) {
				available -= hold.bytes
			}
		}
		s.snapshotBudgetMu.Unlock()
		available -= componentProtocolReserve
		// Cold changes and rotation spend only spare tokens. Keep one required
		// hot checkpoint independent of their size and of intent barriers.
		available -= s.snapshotLimits().MaxBytes
		if available <= 0 {
			if historyHeld {
				return 0, errCheckpointSkipped
			}
			return 0, componentBudgetRefusal(intent)
		}
	}
	final := wire.Components
	charge := 0
	request := sessionstore.ComponentCommit{Components: wire.Components, StateVersion: wire.StateVersion, Expected: s.componentMetadata, WriteBudgetBytes: available}
	request.FinalizeTransport = func(written int) ([]byte, error) {
		charge = written + componentProtocolReserve
		transport := wire.Components.Transport
		if pair.V2 != nil {
			var value transportComponent
			if err := unmarshalComponent(transport, componentTransport, &value); err != nil {
				return nil, err
			}
			host := *pair.V2
			quota := *host.Quota
			balance, err := quota.balance()
			if err != nil {
				return nil, err
			}
			quota.Balance = quotaBalance(uint64(max(0, int64(balance)-int64(charge))))
			host.Quota = &quota
			value.Host, err = json.Marshal(host)
			if err != nil {
				return nil, err
			}
			transport, err = marshalComponent(componentTransport, value)
			if err != nil {
				return nil, err
			}
		}
		final.Transport = bytes.Clone(transport)
		return transport, nil
	}
	tuple, err := store.CommitComponents(ctx, lease, request)
	if err != nil {
		if errors.Is(err, sessionstore.ErrTransient) || errors.Is(err, sessionstore.ErrStateSuperseded) {
			s.componentPending = &componentPending{components: final, expected: s.componentMetadata, pair: pair, next: next, captured: captured, bytes: charge, intent: intent}
		}
		if errors.Is(err, sessionstore.ErrComponentCapacity) {
			if historyHeld {
				return 0, errCheckpointSkipped
			}
			return 0, componentBudgetRefusal(intent)
		}
		return 0, err
	}
	s.componentMetadata = &tuple.Metadata
	return tuple.WrittenBytes + componentProtocolReserve, nil
}

func (s *session) componentExportState() *componentSnapshotState {
	if !s.componentMode {
		return nil
	}
	history := s.componentHistory
	if s.state.Agent.V2 != nil {
		history = s.state.Agent.History
	}
	return &componentSnapshotState{history: bytes.Clone(history)}
}
func (s *session) restoreComponentMode(snap *snapshot) {
	if snap.split == nil {
		return
	}
	s.componentMode = true
	s.componentHistory = bytes.Clone(snap.split.history)
	if snap.split.metadata != nil {
		m := *snap.split.metadata
		s.componentMetadata = &m
	}
}
func (s *session) prepareComponentAdoption(ctx context.Context) error {
	if !s.componentMode || s.componentMetadata != nil || s.worker.cfg.Relay == nil {
		return nil
	}
	store, ok := s.worker.cfg.Relay.Owners.(sessionstore.ComponentStore)
	if !ok {
		return sessionstore.ErrCheckpointLayout
	}
	tuple, err := store.ReadComponents(ctx, s.id)
	// A planned export can precede the first periodic checkpoint. The exact
	// exported capture may initialize this new tenure; the store still refuses
	// an existing legacy value or a concurrently published component tuple.
	if errors.Is(err, sessionstore.ErrCheckpointLayout) {
		return nil
	}
	if err != nil {
		return err
	}
	lease := s.currentComponentLease()
	if tuple.Lease.Worker != lease.Worker || tuple.Lease.Epoch != lease.Epoch {
		return sessionstore.ErrLeaseLost
	}
	m := tuple.Metadata
	s.componentMetadata = &m
	return nil
}

// IsComponentSnapshot identifies the explicit v10 envelope before selecting a
// store path. It never accepts the payload; decodeSnapshot performs validation.
func IsComponentSnapshot(data []byte) bool {
	var v struct{ Version int }
	return json.Unmarshal(data, &v) == nil && v.Version == 10
}

func (s *session) currentComponentLease() sessionstore.Lease {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lease
}
