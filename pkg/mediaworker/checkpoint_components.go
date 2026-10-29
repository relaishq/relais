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

func (c *sessionCheckpoint) componentBudgets() componentBudgets {
	s := c.session
	hot := s.snapshotLimits().MaxBytes
	history := 8192
	if s.agent != nil && s.agent.v2 != nil {
		history = s.agent.v2.registration.Bounds.MaxHistoryBytes
	}
	return splitComponentBudgets(hot, history)
}
func (c *sessionCheckpoint) componentAdmission(data []byte) (componentSizes, error) {
	s := c.session
	var wire componentSnapshot
	if err := json.Unmarshal(data, &wire); err != nil {
		return componentSizes{}, err
	}
	m := componentMetadata{Format: 10, Owner: s.worker.LocalAddr().String()}
	return c.componentBudgets().admit(sessionComponents(wire.Components), m)
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
func (c *sessionCheckpoint) settleComponentWrite(ctx context.Context) error {
	s := c.session
	pending := c.componentPending
	if pending == nil {
		return nil
	}
	store, ok := s.worker.cfg.Relay.Owners.(sessionstore.ComponentStore)
	if !ok {
		return sessionstore.ErrCheckpointLayout
	}
	tuple, err := store.ReadComponents(ctx, s.id)
	if errors.Is(err, sessionstore.ErrCheckpointLayout) && pending.expected == nil {
		c.failComponentPending()
		return nil
	}
	if err != nil {
		return err
	}
	lease := c.currentComponentLease()
	if tuple.Lease.Worker != lease.Worker || tuple.Lease.Epoch != lease.Epoch {
		c.failComponentPending()
		return sessionstore.ErrLeaseLost
	}
	if equalComponentBytes(tuple.Components, pending.components) {
		c.componentMetadata = &tuple.Metadata
		c.ackSnapshot(pending.pair, pending.next, pending.captured, pending.bytes, pending.intent)
		c.componentPending = nil
		return nil
	}
	if sameComponentMetadata(&tuple.Metadata, pending.expected) {
		c.failComponentPending()
		return nil
	}
	// An earlier command can land after its settlement read. Rebase only onto
	// our own tenure. This is ordering evidence, not an acknowledgment of the
	// pending capture or its DTLS bound.
	if tuple.Metadata.Owner == lease.Worker.String() && tuple.Metadata.OwnerEpoch == lease.Epoch {
		c.componentMetadata = &tuple.Metadata
		c.failComponentPending()
		return nil
	}
	return sessionstore.ErrStateSuperseded
}

func (c *sessionCheckpoint) failComponentPending() {
	pending := c.componentPending
	c.componentPending = nil
	c.snapshotBudgetMu.Lock()
	c.snapshotWriteInFlight = false
	c.snapshotBudgetMu.Unlock()
	if pending != nil {
		c.failSnapshot(pending.pair.V2 != nil, pending.intent)
	}
}

func componentBudgetRefusal(intent bool) error {
	if intent {
		return errCheckpointSkipped
	}
	return fmt.Errorf("%w: required component snapshot byte budget", agent.ErrCapacity)
}

func (c *sessionCheckpoint) writeComponents(ctx context.Context, lease sessionstore.Lease, data []byte, pair agentState, next dtlsSequenceCheckpoint, captured time.Time, intent bool) (int, error) {
	s := c.session
	store, ok := s.worker.cfg.Relay.Owners.(sessionstore.ComponentStore)
	if !ok {
		return 0, sessionstore.ErrCheckpointLayout
	}
	var wire componentSnapshot
	if err := json.Unmarshal(data, &wire); err != nil {
		return 0, err
	}
	if _, err := c.componentAdmission(data); err != nil {
		return 0, err
	}
	available := 0
	historyHeld := false
	if pair.V2 != nil {
		c.snapshotBudgetMu.Lock()
		available = int(c.snapshotBytesBudget.available(s.snapshotLimits(), captured))
		if hold := c.snapshotHistoryHold; hold != nil {
			historyHeld = true
			if !hold.matches(pair) {
				available -= hold.bytes
			}
		}
		c.snapshotBudgetMu.Unlock()
		available -= componentProtocolReserve
		// Intent writes and held app pairs preserve a following hot checkpoint.
		// Required adoption/transport writes can spend the carried transaction.
		// Without a hold, store-chosen rotation shares this same budget; a
		// separate rotation reserve would require a store API change.
		if intent || historyHeld {
			available -= s.snapshotLimits().MaxBytes
		}
		if available <= 0 {
			if historyHeld {
				return 0, errCheckpointSkipped
			}
			return 0, componentBudgetRefusal(intent)
		}
	}
	final := wire.Components
	charge := 0
	request := sessionstore.ComponentCommit{Components: wire.Components, StateVersion: wire.StateVersion, Expected: c.componentMetadata, WriteBudgetBytes: available}
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
			c.componentPending = &componentPending{components: final, expected: c.componentMetadata, pair: pair, next: next, captured: captured, bytes: charge, intent: intent}
		}
		if errors.Is(err, sessionstore.ErrComponentCapacity) {
			if historyHeld {
				return 0, errCheckpointSkipped
			}
			return 0, componentBudgetRefusal(intent)
		}
		return 0, err
	}
	c.componentMetadata = &tuple.Metadata
	return tuple.WrittenBytes + componentProtocolReserve, nil
}

func (c *sessionCheckpoint) componentExportState() *componentSnapshotState {
	s := c.session
	if !s.componentMode {
		return nil
	}
	history := s.componentHistory
	if s.state.Agent.V2 != nil {
		history = s.state.Agent.History
	}
	return &componentSnapshotState{history: bytes.Clone(history)}
}
func (c *sessionCheckpoint) restoreComponentMode(snap *snapshot) {
	s := c.session
	if snap.split == nil {
		return
	}
	s.componentMode = true
	s.componentHistory = bytes.Clone(snap.split.history)
	if snap.split.metadata != nil {
		m := *snap.split.metadata
		c.componentMetadata = &m
	}
}
func (c *sessionCheckpoint) prepareAdoption(ctx context.Context) error {
	s := c.session
	if !s.componentMode || c.componentMetadata != nil || s.worker.cfg.Relay == nil {
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
	lease := c.currentComponentLease()
	if tuple.Lease.Worker != lease.Worker || tuple.Lease.Epoch != lease.Epoch {
		return sessionstore.ErrLeaseLost
	}
	m := tuple.Metadata
	c.componentMetadata = &m
	return nil
}

func (c *sessionCheckpoint) currentComponentLease() sessionstore.Lease {
	s := c.session
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lease
}
