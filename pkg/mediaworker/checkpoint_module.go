package mediaworker

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	agent "github.com/relaishq/relais/pkg/agent/v2"

	"github.com/relaishq/relais/pkg/sessionstore"
)

// sessionCheckpoint owns the whole checkpoint transaction behind one concrete
// interface. Callers admit before publishing an app/progress pair and roll back
// the reservation with that pair after a failed callback. Host-envelope preflight
// only measures; it never reserves history, consumes quota or publishes state.
//
// persist serializes per session and settles an unknown reply before recapture.
// Only acknowledgement of the captured pair advances progress and consumes quota.
// Required writes bypass the interval cap; intent writes preserve required work.
// Skipped is not acknowledgement. Transient/superseded outcomes retain pending
// state; authoritative lease loss fences and closes the session.
//
// capture returns live bytes or fences an already-flushed exact export. Admission
// and export never wait for writer/store I/O. Live encoding/store work runs outside
// session.mu; exact export retains its existing encoding lock scope. The first
// write arms the DTLS guard during capture, under that same packet lock.
//
// restore runs at the existing staged adoption points, before callbacks. Quota
// restoration validates its store-clock domain and takes its own budget lock.
// progressLocked and allowDTLSLocked require session.mu. Progress is an owned
// projection; the outgoing DTLS decision adds no allocation or lock. request
// coalesces wakeups, run uses the session lifetime, and wait joins a writer only
// after the caller cancels that lifetime. Encoding and store adapters stay behind
// internal seams; no mutable checkpoint bookkeeping escapes this module.
type sessionCheckpoint struct {
	session                 *session
	snapshotBudgetMu        sync.Mutex
	snapshotHistoryHold     *historyWriteHold               // guarded by snapshotBudgetMu
	snapshotBytesBudget     snapshotBucket                  // guarded by snapshotBudgetMu
	snapshotWriteInFlight   bool                            // guarded by snapshotBudgetMu; one serialized writer
	controlAck              uint64                          // guarded by mu; checkpointed control prefix, carried by exact export
	durableAgent            agentState                      // guarded by mu; last acknowledged durable agent pair
	dtlsAcknowledged        *dtlsSequenceCheckpoint         // guarded by mu; captured next sequence of the last successful PutState
	dtlsProvisional         *dtlsSequenceCheckpoint         // guarded by mu; first write capture, retained until acknowledgment
	dtlsCheckpointRequested bool                            // guarded by mu; one guard wakeup per bound value
	dtlsEpochWarning        sync.Once                       // warn once for an unexpected plaintext checkpoint capture
	lastCheckpoint          time.Time                       // guarded by mu; local acknowledgment time of last successful write
	snapshotStored          atomic.Bool                     // internal harness readiness observation, no state bytes exposed
	componentMetadata       *sessionstore.ComponentMetadata // guarded by snapshotMu
	componentPending        *componentPending               // guarded by snapshotMu
	snapshotMu              sync.Mutex                      // serialize encode/store without blocking packet processing
	snapshotWanted          chan struct{}
}

func newSessionCheckpoint(s *session) sessionCheckpoint {
	return sessionCheckpoint{session: s, snapshotWanted: make(chan struct{}, 1)}
}

// checkpointProgress is an owned projection. Callers hold session.mu and cannot
// change acknowledged progress by changing the returned tasks or observations.
type checkpointProgress struct {
	tasks        []taskIntent
	controlAck   uint64
	at           time.Time
	known        bool
	hasAgent     bool
	observations CheckpointState
}

func (c *sessionCheckpoint) progressLocked() checkpointProgress {
	p := checkpointProgress{controlAck: c.controlAck, at: c.lastCheckpoint, known: c.snapshotStored.Load(), hasAgent: c.durableAgent.V2 != nil, observations: c.session.state.Checkpoint}
	if p.hasAgent {
		p.tasks = cloneTaskIntents(c.durableAgent.V2.Tasks)
	}
	return p
}

type checkpointMode uint8

const (
	checkpointOptional checkpointMode = iota
	checkpointRequired
	checkpointIntent
)

func (c *sessionCheckpoint) persist(ctx context.Context, mode checkpointMode) error {
	return c.persistSnapshotBudget(ctx, mode != checkpointOptional, mode == checkpointIntent)
}
func (c *sessionCheckpoint) wait() {
	c.snapshotMu.Lock()
	c.snapshotMu.Unlock() //nolint:staticcheck // Synchronize with a cancelled writer; no mutation is needed.
}

type checkpointRestore struct {
	snapshot   *snapshot
	quota      *snapshotQuota
	pair       *agentState
	controlAck *uint64
	takeover   *ResumeOptions
	profile    bool
}

// restore is staged at the original restore/adoption points. Pair and layout
// restoration precede callbacks; quota restoration takes its own budget lock.
func (c *sessionCheckpoint) restore(r checkpointRestore) error {
	s := c.session
	if r.profile && s.componentMode && s.snapshotLimits().BurstBytes < 2*c.componentBudgets().Total {
		return fmt.Errorf("%w: split burst must hold two %d-byte transactions", agent.ErrCapacity, c.componentBudgets().Total)
	}
	if r.snapshot != nil {
		c.restoreComponentMode(r.snapshot)
	}
	if r.quota != nil {
		if err := c.restoreSnapshotQuota(r.quota); err != nil {
			return err
		}
	}
	if r.pair != nil {
		c.durableAgent = cloneAgentState(*r.pair)
		if r.pair.V2 != nil {
			c.controlAck = r.pair.V2.ControlHigh
			if r.controlAck != nil {
				c.controlAck = *r.controlAck
			}
			if err := validateControlWindow(r.pair.V2, c.controlAck); err != nil {
				return err
			}
		}
	}
	if r.takeover != nil {
		s.state.Checkpoint.TakeoverAge = r.takeover.CheckpointAge
		s.state.Checkpoint.TakeoverSnapshotAge = r.takeover.SnapshotAge
		s.state.Checkpoint.TakeoverStoredAt = r.takeover.CheckpointStoredAt
	}
	return nil
}
