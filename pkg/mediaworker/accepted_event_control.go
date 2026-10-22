package mediaworker

import (
	"encoding/json"
	"slices"

	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/sessionstore"
)

func stageControl(e *v2Envelope, event agent.Event) {
	if event.Kind != agent.Control {
		return
	}
	m := *event.Control
	e.Controls = append(slices.Clone(e.Controls), controlRecord{ID: controlID(m.ID), Sequence: m.Sequence, Digest: controlDigest(m)})
	e.ControlHigh = m.Sequence
}

func (host acceptedEvents) control(m agent.ControlMessage, mode eventMode) (agent.ControlReceipt, error) {
	s := host.session
	s.mu.Lock()
	paused := s.agent.paused || s.fenced.Load() || s.ctx.Err() != nil
	ack := s.checkpoint.progress(checkpointProgressRequest{}).controlAck
	s.mu.Unlock()
	if paused {
		return agent.ControlReceipt{}, sessionstore.ErrLeaseLost
	}
	if err := validateControl(m); err != nil {
		return agent.ControlReceipt{}, err
	}
	if !s.controlSupported() {
		return agent.ControlReceipt{}, agent.ErrControlUnsupported
	}
	e := s.agent.latest.V2
	if !e.Started {
		return agent.ControlReceipt{}, agent.ErrControlNotReady
	}
	digest := controlDigest(m)
	for _, r := range e.Controls {
		if r.ID == controlID(m.ID) || r.Sequence == m.Sequence {
			if r.ID != controlID(m.ID) || r.Sequence != m.Sequence || r.Digest != digest {
				return agent.ControlReceipt{}, agent.ErrControlConflict
			}
			return controlReceipt(r, m.ID, ack), nil
		}
	}
	if m.Sequence <= e.ControlHigh {
		return agent.ControlReceipt{}, agent.ErrControlStale
	}
	if m.Sequence != e.ControlHigh+1 {
		return agent.ControlReceipt{}, agent.ErrControlOrder
	}
	// Compact only an acknowledged prefix outside the sequence replay horizon.
	// New messages are contiguous and ordered. Future sequences are refused
	// without consumption, so a sparse retry cannot silently discard new work.
	next := *e
	next.Controls = slices.Clone(e.Controls)
	for len(next.Controls) > 0 && next.Controls[0].Sequence <= ack && m.Sequence-next.Controls[0].Sequence > controlWindow {
		next.ControlFloor = next.Controls[0].Sequence
		next.Controls = next.Controls[1:]
	}
	event := agent.Event{Kind: agent.Control, ID: agent.EventID("control/" + digest), Control: &m}
	candidate := next
	stageControl(&candidate, event)
	limit := min(s.agent.v2.registration.Bounds.MaxPendingBytes, v2HostMetadataBytes)
	// A small declared limit can shorten the sparse window. Only acknowledged
	// prefixes may be compacted; a full unacknowledged window refuses admission.
	for v2MetadataCharge(candidate, limit) > limit && len(next.Controls) > 0 && next.Controls[0].Sequence <= ack {
		next.ControlFloor = next.Controls[0].Sequence
		next.Controls = next.Controls[1:]
		candidate = next
		stageControl(&candidate, event)
	}
	wire, _ := json.Marshal(candidate)
	// Callback-local Data shares pending bytes by its real JSON encoding.
	data, _ := json.Marshal(m.Data)
	if v2MetadataCharge(candidate, limit) > limit || len(wire)+len(data) > limit {
		return agent.ControlReceipt{}, agent.ErrCapacity
	}
	// Pruning is transaction-local. No observer, rollback or callback sees a
	// temporarily substituted accepted envelope.
	if err := host.accept(event, mode, &next); err != nil {
		return agent.ControlReceipt{}, err
	}
	return controlReceipt(controlRecord{ID: controlID(m.ID), Sequence: m.Sequence, Digest: digest}, m.ID, ack), nil
}

// Receipt detail is only a duplicate-response cache. Under admission pressure,
// discard its acknowledged prefix, preserving the durable sequence floor.
// Callers use owned candidates and publish compaction with the admitted change.
func (s *session) compactControlMetadata(e *v2Envelope, limit int) int {
	n := v2MetadataCharge(*e, limit)
	if n <= limit {
		return n
	}
	s.mu.Lock()
	ack := s.checkpoint.progress(checkpointProgressRequest{}).controlAck
	s.mu.Unlock()
	count := 0
	for count < len(e.Controls) && e.Controls[count].Sequence <= ack {
		e.ControlFloor = e.Controls[count].Sequence
		count++
	}
	e.Controls = e.Controls[count:]
	return v2MetadataCharge(*e, limit)
}
