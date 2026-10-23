package controlplane

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	"github.com/relaishq/relais/pkg/mediaworker"
)

// workerObservation owns the knowledge attached to one registration. All
// operations run under the plane's registry lock; none performs network or
// store work. Only detect establishes death, and only heartbeat completes rejoin.
// Replacement constructs fresh knowledge instead of reusing a death cycle.
type workerObservation struct {
	relayPending      bool
	livenessUnknown   bool
	draining          bool
	lastHeartbeat     time.Time
	deathHeartbeat    time.Time
	deathDetected     time.Time
	deathToken        string
	preserving        bool
	dead              bool
	recovering        bool
	recovered         bool
	rejoinReady       bool
	rejoinToken       string
	acceptedToken     string
	heartbeatObserved bool
}

type workerPoolClass uint8

const (
	workerPoolPending workerPoolClass = iota
	workerPoolUnreachable
	workerPoolDraining
	workerPoolReady
)

type workerCreateDisposition uint8

const (
	workerCreateObserve workerCreateDisposition = iota
	workerCreateWait
	workerCreateRecover
)

// workerKnowledge is a copy, including the existing public status diagnostics.
// Consumers use decisions instead of rebuilding predicates from diagnostics.
type workerKnowledge struct {
	resumeRetry       bool
	eligible          bool
	poolClass         workerPoolClass
	relayEligible     bool
	relayRequired     bool
	replacementReady  bool
	retryPlanned      bool
	createDisposition workerCreateDisposition
	dead              bool
	draining          bool
	livenessUnknown   bool
	recovering        bool
	lastHeartbeat     time.Time
}

type workerHeartbeat struct {
	remote  bool
	restart bool
	token   string
}

type workerDetection struct {
	recover  bool
	newDeath bool
}

func newWorkerObservation(now time.Time, relayPending bool) workerObservation {
	return workerObservation{lastHeartbeat: now, relayPending: relayPending}
}

// observeStatus distinguishes initial restoration from the later rebuild merge.
// Its result requests exactly the load-version increment of initial failure.
func (o *workerObservation) observeStatus(status mediaworker.WorkerStatus, failed, initial bool) bool {
	if initial {
		if failed {
			o.livenessUnknown = !o.heartbeatObserved
			return true
		}
		o.draining = status.Draining
	} else if !failed {
		o.draining = o.draining || status.Draining
	}
	return false
}

// heartbeat preserves the two existing rejoin protocols. The bool requests the
// existing load-version increment when unknown liveness becomes known. A zero
// time samples the live clock at the original heartbeat-recording step; explicit
// times let callers describe an observation sequence without a clock adapter.
func (o *workerObservation) heartbeat(input workerHeartbeat, now time.Time) (HeartbeatReply, bool, error) {
	// Liveness during recovery is evidence only, never a rejoin ACK. Preserve
	// the original gap separately for deadlines, checkpoint reserves and events.
	if o.recovering || o.dead && (!o.recovered || o.preserving) {
		if input.remote && input.token != "" && input.token != o.deathToken {
			return HeartbeatReply{}, false, errors.New("controlplane: stale rejoin token")
		}
		o.recordHeartbeat(now)
		return HeartbeatReply{}, false, errors.New("controlplane: worker takeover in progress")
	}
	if input.remote {
		if input.restart && !o.dead && !o.heartbeatObserved {
			o.acceptedToken = input.token
			o.heartbeatObserved = true
		}
		if o.dead {
			if o.rejoinToken == "" {
				var token [16]byte
				if _, err := rand.Read(token[:]); err != nil {
					return HeartbeatReply{}, false, err
				}
				o.rejoinToken = hex.EncodeToString(token[:])
			}
			if input.token != o.rejoinToken {
				return HeartbeatReply{Token: o.rejoinToken}, false, nil
			}
			o.dead = false
			o.acceptedToken = o.rejoinToken
			o.rejoinToken = ""
		} else if input.token != "" && input.token != o.acceptedToken {
			return HeartbeatReply{}, false, errors.New("controlplane: stale rejoin token")
		}
		changed := false
		if o.livenessUnknown {
			o.livenessUnknown = false
			changed = true
		}
		o.recordHeartbeat(now)
		return HeartbeatReply{}, changed, nil
	}
	if o.dead && !o.rejoinReady {
		o.rejoinReady = true
		return HeartbeatReply{}, false, mediaworker.ErrRejoinRequired
	}
	o.dead = false
	changed := false
	if o.livenessUnknown {
		o.livenessUnknown = false
		changed = true
	}
	o.recordHeartbeat(now)
	return HeartbeatReply{}, changed, nil
}

func (o *workerObservation) recordHeartbeat(now time.Time) {
	if now.IsZero() {
		now = time.Now()
	}
	o.lastHeartbeat = now
}

// recoveryHeartbeat never lets returning liveness shorten the original outage.
func (o *workerObservation) recoveryHeartbeat() time.Time {
	if o.dead && !o.deathHeartbeat.IsZero() {
		return o.deathHeartbeat
	}
	return o.lastHeartbeat
}

func (o *workerObservation) detect(now time.Time, deadAfter time.Duration) workerDetection {
	if o.recovering {
		return workerDetection{}
	}
	if (!o.dead && now.Sub(o.lastHeartbeat) >= deadAfter) || (o.dead && !o.recovered) {
		newDeath := !o.dead
		if !o.dead {
			o.deathHeartbeat, o.deathToken = o.lastHeartbeat, o.acceptedToken
			o.deathDetected = now
			o.rejoinToken, o.acceptedToken = "", ""
		}
		o.dead, o.recovering, o.recovered, o.rejoinReady = true, true, false, false
		return workerDetection{recover: true, newDeath: newDeath}
	}
	return workerDetection{}
}

func (o *workerObservation) recoveryFinished(settled bool) {
	o.recovered = settled
	o.recovering = false
}

func (o *workerObservation) markDraining() { o.draining = true }

// relayRegistered returns whether the old relay-pending branch changed state.
func (o *workerObservation) relayRegistered() bool {
	if o.relayPending {
		o.relayPending = false
		return true
	}
	return false
}

func (o *workerObservation) canReplace(reserved int) bool {
	return o.dead && o.recovered && !o.recovering && !o.preserving && reserved == 0
}

// resetDrain follows the caller's durable deletion and identity/lease recheck.
func (o *workerObservation) resetDrain() { o.draining = false }

func (o *workerObservation) knowledge() workerKnowledge {
	k := workerKnowledge{
		resumeRetry:      !o.dead && !o.draining,
		eligible:         !o.dead && !o.draining && !o.recovering && !o.relayPending && !o.livenessUnknown,
		relayEligible:    !o.dead && !o.recovering,
		relayRequired:    !o.dead && o.relayPending,
		replacementReady: o.dead && o.recovered && !o.recovering && !o.preserving,
		retryPlanned:     !o.recovering && !o.dead,
		dead:             o.dead, draining: o.draining, livenessUnknown: o.livenessUnknown,
		recovering: o.recovering, lastHeartbeat: o.lastHeartbeat,
	}
	if o.dead {
		k.createDisposition = workerCreateRecover
	} else if o.livenessUnknown {
		k.createDisposition = workerCreateWait
	}
	switch {
	case o.relayPending:
		k.poolClass = workerPoolPending
	case o.dead || o.recovering:
		k.poolClass = workerPoolUnreachable
	case o.livenessUnknown:
		k.poolClass = workerPoolPending
	case o.draining:
		k.poolClass = workerPoolDraining
	default:
		k.poolClass = workerPoolReady
	}
	return k
}
