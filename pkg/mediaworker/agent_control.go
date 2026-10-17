package mediaworker

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"unicode/utf8"

	"github.com/relaishq/relais/internal/privateapi"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/sessionstore"
)

// Keep receipts across four newer sequence positions. Prefix compaction needs
// checkpoint acknowledgement too. Each retained export has its own exact window;
// there is no wall-clock receipt TTL or assumption about HTTP retry latency.
const controlWindow = 4

type controlRecord struct {
	ID       string `json:"i"`
	Sequence uint64 `json:"s"`
	Digest   string `json:"d"`
}
type controlRequest struct {
	message agent.ControlMessage
	done    chan controlReply
}
type controlReply struct {
	receipt agent.ControlReceipt
	err     error
}

// AgentControl submits to the serialized actor, never the media input queue.
// There is no accepted-but-droppable control queue. A timeout after handoff is
// uncertain; the client must retry the same message on the current route.
func (w *Worker) AgentControl(ctx context.Context, id string, message agent.ControlMessage) (agent.ControlReceipt, error) {
	if err := validateControl(message); err != nil {
		return agent.ControlReceipt{}, err
	}
	s := w.session(id)
	if s == nil {
		return agent.ControlReceipt{}, ErrUnknownSession
	}
	if s.agent == nil || s.agent.v2 == nil {
		return agent.ControlReceipt{}, agent.ErrControlUnsupported
	}
	request := controlRequest{message: message, done: make(chan controlReply, 1)}
	request.message.Data = slices.Clone(message.Data)
	ctx, cancel := context.WithTimeout(ctx, privateapi.Timeout)
	defer cancel()
	select {
	case s.agent.v2.controls <- request:
	case <-ctx.Done():
		return agent.ControlReceipt{}, ctx.Err()
	case <-s.ctx.Done():
		return agent.ControlReceipt{}, sessionstore.ErrLeaseLost
	}
	select {
	case reply := <-request.done:
		return reply.receipt, reply.err
	case <-ctx.Done():
		return agent.ControlReceipt{}, privateapi.Uncertain(ctx.Err())
	case <-s.ctx.Done():
		return agent.ControlReceipt{}, privateapi.Uncertain(sessionstore.ErrLeaseLost)
	}
}

func validateControl(m agent.ControlMessage) error {
	if m.ID == "" || len(m.ID) > 128 || !utf8.ValidString(m.ID) || m.Sequence == 0 || m.Name == "" || len(m.Name) > 128 || !utf8.ValidString(m.Name) || len(m.Data) > 1024 {
		return agent.ErrControlInvalid
	}
	return nil
}
func controlDigest(m agent.ControlMessage) string {
	h := sha256.New()
	h.Write([]byte{1}) // versioned, length-delimited, binary-safe identity
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], m.Sequence)
	h.Write(n[:])
	for _, b := range [][]byte{[]byte(m.ID), []byte(m.Name), m.Data} {
		binary.BigEndian.PutUint64(n[:], uint64(len(b)))
		h.Write(n[:])
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))
}
func controlReceipt(r controlRecord, ack uint64) agent.ControlReceipt {
	return agent.ControlReceipt{ID: r.ID, Sequence: r.Sequence, EventID: agent.EventID("control/" + r.Digest), Checkpointed: r.Sequence <= ack}
}
func stageControl(e *v2Envelope, event agent.Event) {
	if event.Kind != agent.Control {
		return
	}
	m := *event.Control
	e.Controls = append(slices.Clone(e.Controls), controlRecord{ID: m.ID, Sequence: m.Sequence, Digest: controlDigest(m)})
	e.ControlHigh = m.Sequence
}
func validateControlWindow(e *v2Envelope, ack uint64) error {
	if e.ControlFloor > e.ControlHigh || e.ControlFloor > ack || ack > e.ControlHigh {
		return agent.ErrControlInvalid
	}
	last := e.ControlFloor
	ids := make(map[string]bool)
	for _, r := range e.Controls {
		if r.Sequence <= last || r.Sequence > e.ControlHigh || r.ID == "" || len(r.ID) > 128 || !utf8.ValidString(r.ID) || ids[r.ID] || len(r.Digest) != 64 {
			return agent.ErrControlInvalid
		}
		if _, err := hex.DecodeString(r.Digest); err != nil {
			return agent.ErrControlInvalid
		}
		ids[r.ID] = true
		last = r.Sequence
	}
	if last != e.ControlHigh {
		return agent.ErrControlInvalid
	}
	return nil
}
func controlRefusal(err error) bool {
	return errors.Is(err, agent.ErrControlInvalid) || errors.Is(err, agent.ErrControlConflict) || errors.Is(err, agent.ErrControlStale) || errors.Is(err, agent.ErrControlOrder) || errors.Is(err, agent.ErrCapacity) || errors.Is(err, errTaskAdmissionBackpressure) || errors.Is(err, sessionstore.ErrLeaseLost)
}
func (s *session) processControl(m agent.ControlMessage) (agent.ControlReceipt, error) {
	s.mu.Lock()
	paused := s.agent.paused || s.fenced.Load() || s.ctx.Err() != nil
	ack := s.controlAck
	s.mu.Unlock()
	if paused {
		return agent.ControlReceipt{}, sessionstore.ErrLeaseLost
	}
	if err := validateControl(m); err != nil {
		return agent.ControlReceipt{}, err
	}
	e := s.agent.latest.V2
	digest := controlDigest(m)
	for _, r := range e.Controls {
		if r.ID == m.ID || r.Sequence == m.Sequence {
			if r.ID != m.ID || r.Sequence != m.Sequence || r.Digest != digest {
				return agent.ControlReceipt{}, agent.ErrControlConflict
			}
			return controlReceipt(r, ack), nil
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
	wire, _ := json.Marshal(candidate)
	// Data is callback-local, but its real encoded size shares pending allowance.
	data, _ := json.Marshal(m.Data)
	if len(wire)+len(data) > s.agent.v2.registration.Bounds.MaxPendingBytes {
		return agent.ControlReceipt{}, agent.ErrCapacity
	}
	// The actor owns latest. Stage pruning for validation, then restore it if the
	// callback fails. No published state sees a receipt without the callback state.
	old := s.agent.latest.V2
	s.agent.latest.V2 = &next
	err := s.handleV2Event(event)
	if err != nil {
		s.agent.latest.V2 = old
		return agent.ControlReceipt{}, err
	}
	return controlReceipt(controlRecord{ID: m.ID, Sequence: m.Sequence, Digest: digest}, ack), nil
}
