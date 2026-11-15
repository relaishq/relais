package controlplane

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/relaishq/relais/pkg/sessionstore"
)

// Relay change events per slot (ADR 0016, Consequences). Each is recorded
// where control decides it: confirmation events by the proven durable write
// that makes the transition, so a control that finds a decision already
// saved (a replay after restart) records nothing.
const (
	relayStarted         = "started"          // a new attempt (generation) was saved
	relaySwitched        = "switched"         // control saved its first observation of the switch commit
	relayDelivered       = "delivered"        // C covers every required MID
	relayDecodeConfirmed = "decode_confirmed" // A covers every required MID
	relayConfirmed       = "confirmed"        // the attempt met the completion predicate
	relayRolledBack      = "rolled_back"      // a planned change was confirmed back on its original slot
	relayFailed          = "failed"           // the episode ended the call with relay_change_failed
)

var relayChangeEvents = [...]string{relayStarted, relaySwitched, relayDelivered, relayDecodeConfirmed, relayConfirmed, relayRolledBack, relayFailed}

// relayTimeBuckets cover the A attempt bound (3 s), the planned deadline
// (10 s) and the episode bound (25 s).
var relayTimeBuckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 3, 5, 10, 15, 25}

type relayMetrics struct {
	changes      *prometheus.CounterVec
	losses       *prometheus.CounterVec
	switchTime   *prometheus.HistogramVec
	confirmation *prometheus.HistogramVec
}

func newRelayMetrics() *relayMetrics {
	return &relayMetrics{
		changes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "relais_control_relay_changes_total",
			Help: "Relay change attempts by target slot and event: started, switched, delivered (C), decode_confirmed (A), confirmed, rolled_back and failed (call ended).",
		}, []string{"slot", "event"}),
		losses: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "relais_control_relay_loss_decisions_total",
			Help: "Decisions that a relay slot is lost.",
		}, []string{"slot"}),
		switchTime: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "relais_control_relay_switch_seconds",
			Help:    "Per attempt, from saving the attempt to this control's first observation of its switch commit.",
			Buckets: relayTimeBuckets,
		}, []string{"slot"}),
		confirmation: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "relais_control_relay_confirmation_seconds",
			Help:    "Per attempt, from this control's first observation of the switch commit to the saved confirmation.",
			Buckets: relayTimeBuckets,
		}, []string{"slot", "confirmation"}),
	}
}

func (m *relayMetrics) collectors() []prometheus.Collector {
	return []prometheus.Collector{m.changes, m.losses, m.switchTime, m.confirmation}
}

// exposeSlot publishes zero series for a configured slot.
func (m *relayMetrics) exposeSlot(slot string) {
	for _, event := range relayChangeEvents {
		m.changes.WithLabelValues(slot, event)
	}
	m.losses.WithLabelValues(slot)
	m.switchTime.WithLabelValues(slot)
	m.confirmation.WithLabelValues(slot, sessionstore.RelayConfirmDelivery)
	m.confirmation.WithLabelValues(slot, sessionstore.RelayConfirmDecode)
}

func (m *relayMetrics) event(slot, event string) { m.changes.WithLabelValues(slot, event).Inc() }

// relayTiming is this control's own observation of a call's current attempt.
// Times saved by another control are never used: they come from another
// host's clock.
type relayTiming struct {
	generation        uint64
	started, switched time.Time
}

// recordRelayWrite records the events that one proven confirmation write
// decided. Only the writer of a transition records it, so a later read of the
// saved state (a retry, a replay after restart) records nothing.
func (p *Plane) recordRelayWrite(id string, at sessionstore.RelayAssignment, from, to sessionstore.RelayConfirmation) {
	now := time.Now()
	m := p.relayMetrics
	if ep := to.Episode; ep != nil && ep.Outcome == sessionstore.RelayEpisodeEnded && (from.Episode == nil || from.Episode.Outcome != sessionstore.RelayEpisodeEnded) {
		slot := at.Slot
		if t := to.Attempt; t != nil && t.Episode == ep.Token {
			slot = t.Slot // the attempt named in the ended event's detail
		}
		m.event(slot, relayFailed)
	}
	t := to.Attempt
	if t == nil {
		return
	}
	prev := from.Attempt
	begun := prev == nil || prev.Generation != t.Generation
	if begun {
		prev = &sessionstore.RelayAttempt{Phase: sessionstore.RelayPhasePending}
		m.event(t.Slot, relayStarted)
	}
	switched := prev.Phase == sessionstore.RelayPhasePending && (t.Phase == sessionstore.RelayPhaseSwitched || t.Phase == sessionstore.RelayPhaseConfirmed)
	confirmed := prev.Phase != sessionstore.RelayPhaseConfirmed && t.Phase == sessionstore.RelayPhaseConfirmed
	if switched {
		m.event(t.Slot, relaySwitched)
	}
	if !sessionstore.Covers(prev.Delivered, t.Required) && sessionstore.Covers(t.Delivered, t.Required) {
		m.event(t.Slot, relayDelivered)
	}
	if !sessionstore.Covers(prev.Decoded, t.Required) && sessionstore.Covers(t.Decoded, t.Required) {
		m.event(t.Slot, relayDecodeConfirmed)
	}
	if confirmed {
		m.event(t.Slot, relayConfirmed)
	}
	p.mu.Lock()
	c := p.calls[id]
	p.mu.Unlock()
	if c == nil {
		return
	}
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	if begun {
		c.relayTiming = relayTiming{generation: t.Generation, started: now}
	} else if c.relayTiming.generation != t.Generation {
		c.relayTiming = relayTiming{generation: t.Generation} // begun by another control
	}
	if switched {
		if !c.relayTiming.started.IsZero() {
			m.switchTime.WithLabelValues(t.Slot).Observe(now.Sub(c.relayTiming.started).Seconds())
		}
		c.relayTiming.switched = now
	}
	if confirmed && !c.relayTiming.switched.IsZero() {
		m.confirmation.WithLabelValues(t.Slot, to.Mode).Observe(now.Sub(c.relayTiming.switched).Seconds())
	}
}
