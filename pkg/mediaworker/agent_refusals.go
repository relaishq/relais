package mediaworker

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"

	agent "github.com/relaishq/relais/pkg/agent/v2"
)

// Control-bearing v8 envelopes encode refusal IDs as their original 32 digest
// bytes, with a sparse turn bitmap. Version 1 preserves every ID/turn exactly.
// Without Control, keep the original v8 encoding and admission accounting.
func (e v2Envelope) MarshalJSON() ([]byte, error) {
	type plain v2Envelope
	if e.ControlHigh == 0 || len(e.Capacity) == 0 {
		return json.Marshal(plain(e))
	}
	packed, ok := packTaskRefusals(e.Capacity)
	if !ok {
		return json.Marshal(plain(e))
	} // old, non-digest test/application IDs
	capacity, err := json.Marshal(packed)
	if err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		*plain
		Capacity json.RawMessage
	}{plain: (*plain)(&e), Capacity: capacity})
}
func (e *v2Envelope) UnmarshalJSON(data []byte) error {
	type plain v2Envelope
	var decoded struct {
		*plain
		Capacity json.RawMessage
	}
	decoded.plain = (*plain)(e)
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	e.Capacity = nil
	if len(decoded.Capacity) == 0 {
		return nil
	}
	if decoded.Capacity[0] != '"' {
		return json.Unmarshal(decoded.Capacity, &e.Capacity)
	}
	var packed []byte
	if err := json.Unmarshal(decoded.Capacity, &packed); err != nil {
		return err
	}
	refs, err := unpackTaskRefusals(packed)
	if err != nil {
		return err
	}
	e.Capacity = refs
	return nil
}
func packTaskRefusals(refusals []taskRefusal) ([]byte, bool) {
	// Header: version, count, bitmap. Each entry has 32 ID bytes and, only when
	// its bitmap bit is set, an unsigned varint turn. Size follows real content.
	out := binary.AppendUvarint([]byte{1}, uint64(len(refusals)))
	bitmap := len(out)
	out = append(out, make([]byte, (len(refusals)+7)/8)...)
	for i, r := range refusals {
		id, err := base64.RawURLEncoding.DecodeString(string(r.ID))
		if err != nil || len(id) != 32 || base64.RawURLEncoding.EncodeToString(id) != string(r.ID) {
			return nil, false
		}
		out = append(out, id...)
		if r.Turn != 0 {
			out[bitmap+i/8] |= 1 << uint(i%8)
			out = binary.AppendUvarint(out, uint64(r.Turn))
		}
	}
	return out, true
}
func unpackTaskRefusals(data []byte) ([]taskRefusal, error) {
	invalid := agent.ErrIncompatibleAgent
	if len(data) < 2 || len(data) > v2HostMetadataBytes || data[0] != 1 {
		return nil, invalid
	}
	count, n := binary.Uvarint(data[1:])
	if n <= 0 || count == 0 || count > uint64(len(data)/32) {
		return nil, invalid
	}
	// Only the canonical varint representation is accepted.
	if len(binary.AppendUvarint(nil, count)) != n {
		return nil, invalid
	}
	data = data[1+n:]
	width := (int(count) + 7) / 8
	if len(data) < width {
		return nil, invalid
	}
	bitmap := data[:width]
	data = data[width:]
	if count%8 != 0 && bitmap[width-1]>>uint(count%8) != 0 {
		return nil, invalid
	}
	refs := make([]taskRefusal, int(count))
	for i := range refs {
		if len(data) < 32 {
			return nil, invalid
		}
		refs[i].ID = agent.TaskID(base64.RawURLEncoding.EncodeToString(data[:32]))
		data = data[32:]
		if bitmap[i/8]&(1<<uint(i%8)) != 0 {
			turn, n := binary.Uvarint(data)
			if n <= 0 || turn == 0 || len(binary.AppendUvarint(nil, turn)) != n {
				return nil, invalid
			}
			refs[i].Turn = agent.Turn(turn)
			data = data[n:]
		}
	}
	if len(data) != 0 {
		return nil, invalid
	}
	return refs, nil
}
