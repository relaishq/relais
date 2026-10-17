package mediaworker

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
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

// Both identities are fixed-size SHA-256 digests. Raw client IDs never occupy
// checkpoint metadata. A retry supplies its original ID for the receipt.
type controlRecord struct {
	ID       string
	Sequence uint64
	Digest   string
}

// Version 1 stores a 73-byte record: version, sequence, ID hash, body hash.
// JSON base64 is 102 bytes including quotes, independent of client ID length.
func (r controlRecord) MarshalJSON() ([]byte, error) {
	id, err := hex.DecodeString(r.ID)
	if err != nil || len(id) != sha256.Size {
		return nil, agent.ErrControlInvalid
	}
	body, err := hex.DecodeString(r.Digest)
	if err != nil || len(body) != sha256.Size {
		return nil, agent.ErrControlInvalid
	}
	var record [1 + 8 + 2*sha256.Size]byte
	record[0] = 1
	binary.BigEndian.PutUint64(record[1:9], r.Sequence)
	copy(record[9:41], id)
	copy(record[41:], body)
	return json.Marshal(record[:])
}
func (r *controlRecord) UnmarshalJSON(data []byte) error {
	if len(data) > 0 && data[0] == '{' {
		// Read the initial slice-8a encoding; all subsequent writes use digests.
		var old struct {
			ID       string `json:"i"`
			Sequence uint64 `json:"s"`
			Digest   string `json:"d"`
		}
		if err := json.Unmarshal(data, &old); err != nil {
			return err
		}
		if old.ID == "" || len(old.ID) > 128 || !utf8.ValidString(old.ID) {
			return agent.ErrControlInvalid
		}
		*r = controlRecord{ID: controlID(old.ID), Sequence: old.Sequence, Digest: old.Digest}
		return nil
	}
	var record []byte
	if err := json.Unmarshal(data, &record); err != nil {
		return err
	}
	if len(record) != 73 || record[0] != 1 {
		return agent.ErrControlInvalid
	}
	*r = controlRecord{ID: hex.EncodeToString(record[9:41]), Sequence: binary.BigEndian.Uint64(record[1:9]), Digest: hex.EncodeToString(record[41:])}
	return nil
}
func controlID(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])
}

// Charge the Control sub-budget from actual wire records, including maximum
// counter widths. At most one third of pending metadata belongs to Control.
// No memory is preallocated. Agents that never use Control keep their existing
// allowance. Once enabled, ordinary event admission is independent of receipt
// occupancy, so a stalled store cannot take timer/task headroom away.
func controlMetadataBudget(limit int) int {
	record := controlRecord{ID: controlID(""), Sequence: math.MaxUint64, Digest: controlID("")}
	envelope := v2Envelope{ControlHigh: math.MaxUint64, ControlFloor: math.MaxUint64, Controls: make([]controlRecord, controlWindow+1)}
	for i := range envelope.Controls {
		envelope.Controls[i] = record
	}
	full, _ := json.Marshal(envelope)
	envelope.Controls, envelope.ControlHigh, envelope.ControlFloor = nil, 0, 0
	bare, _ := json.Marshal(envelope)
	return min(limit/3, len(full)-len(bare))
}
func v2MetadataCharge(envelope v2Envelope, limit int) int {
	full, err := json.Marshal(envelope)
	if err != nil {
		return limit + 1
	}
	if envelope.ControlHigh == 0 {
		return len(full)
	}
	envelope.Controls, envelope.ControlHigh, envelope.ControlFloor = nil, 0, 0
	bare, err := json.Marshal(envelope)
	if err != nil {
		return limit + 1
	}
	budget := controlMetadataBudget(limit)
	if len(full)-len(bare) > budget {
		return limit + 1
	}
	return len(bare) + budget
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

func validateControl(m agent.ControlMessage) error { return m.Validate() }
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
func controlReceipt(r controlRecord, id string, ack uint64) agent.ControlReceipt {
	return agent.ControlReceipt{ID: id, Sequence: r.Sequence, EventID: agent.EventID("control/" + r.Digest), Checkpointed: r.Sequence <= ack}
}
func stageControl(e *v2Envelope, event agent.Event) {
	if event.Kind != agent.Control {
		return
	}
	m := *event.Control
	e.Controls = append(slices.Clone(e.Controls), controlRecord{ID: controlID(m.ID), Sequence: m.Sequence, Digest: controlDigest(m)})
	e.ControlHigh = m.Sequence
}
func validateControlWindow(e *v2Envelope, ack uint64) error {
	if e.ControlFloor > e.ControlHigh || e.ControlFloor > ack || ack > e.ControlHigh {
		return agent.ErrControlInvalid
	}
	last := e.ControlFloor
	ids := make(map[string]bool)
	for _, r := range e.Controls {
		if r.Sequence <= last || r.Sequence > e.ControlHigh || len(r.ID) != 64 || ids[r.ID] || len(r.Digest) != 64 {
			return agent.ErrControlInvalid
		}
		if _, err := hex.DecodeString(r.ID); err != nil {
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
	return errors.Is(err, agent.ErrControlRejected) || errors.Is(err, agent.ErrControlNotReady) || errors.Is(err, agent.ErrControlInvalid) || errors.Is(err, agent.ErrControlConflict) || errors.Is(err, agent.ErrControlStale) || errors.Is(err, agent.ErrControlOrder) || errors.Is(err, agent.ErrCapacity) || errors.Is(err, errTaskAdmissionBackpressure) || errors.Is(err, sessionstore.ErrLeaseLost)
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
	// The actor owns latest. Stage pruning for validation, then restore it if the
	// callback fails. No published state sees a receipt without the callback state.
	old := s.agent.latest.V2
	s.agent.latest.V2 = &next
	err := s.handleV2Event(event)
	if err != nil {
		s.agent.latest.V2 = old
		return agent.ControlReceipt{}, err
	}
	return controlReceipt(controlRecord{ID: controlID(m.ID), Sequence: m.Sequence, Digest: digest}, m.ID, ack), nil
}
