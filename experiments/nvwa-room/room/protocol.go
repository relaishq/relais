// Package room implements the disposable, bounded two-player room experiment.
package room

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

const MaxMessage = 1024
const MaxPending = 16384
const MaxCommands = 4096
const MaxRate = 120

// Message is shared by both transports. Identity is obtained only from Token.
type Message struct {
	TestBarrier bool            `json:"test_barrier,omitempty"`
	TestPauseMS int             `json:"test_pause_ms,omitempty"`
	Type        string          `json:"type"`
	RequestID   string          `json:"request_id,omitempty"`
	RoomID      string          `json:"room_id,omitempty"`
	Epoch       string          `json:"room_epoch,omitempty"`
	Generation  uint64          `json:"connection_generation,omitempty"`
	Token       string          `json:"token,omitempty"`
	SessionID   string          `json:"session_id,omitempty"`
	CommandID   string          `json:"command_id,omitempty"`
	InputSeq    uint64          `json:"input_seq,omitempty"`
	Digest      string          `json:"payload_digest,omitempty"`
	Payload     json.RawMessage `json:"payload,omitempty"`
	Seq         uint64          `json:"seq,omitempty"`
	SentNS      int64           `json:"sent_ns,omitempty"`
	DropReceipt bool            `json:"test_drop_receipt,omitempty"`
}
type Action struct {
	ID        string `json:"id"`
	Version   int    `json:"version"`
	StartTick int64  `json:"start_tick"`
}
type Player struct {
	ID          string          `json:"id"`
	Character   string          `json:"character"`
	Generation  uint64          `json:"generation"`
	Joined      bool            `json:"joined"`
	Action      *Action         `json:"action,omitempty"`
	StateSeq    uint64          `json:"state_seq"`
	State       json.RawMessage `json:"state,omitempty"`
	StateSentNS int64           `json:"state_sent_ns,omitempty"`
}
type Result struct {
	Error     string `json:"error,omitempty"`
	CommandID string `json:"command_id"`
	Type      string `json:"type"`
	InputSeq  uint64 `json:"input_seq"`
	Digest    string `json:"payload_digest"`
	Version   uint64 `json:"room_version"`
	Applied   bool   `json:"applied"`
}
type Reply struct {
	Type       string   `json:"type"`
	RequestID  string   `json:"request_id,omitempty"`
	Error      string   `json:"error,omitempty"`
	RoomID     string   `json:"room_id"`
	Epoch      string   `json:"room_epoch"`
	Version    uint64   `json:"room_version"`
	Tick       int64    `json:"room_tick"`
	PlayerID   string   `json:"player_id,omitempty"`
	Generation uint64   `json:"connection_generation,omitempty"`
	Players    []Player `json:"players,omitempty"`
	Result     *Result  `json:"result,omitempty"`
	Seq        uint64   `json:"seq,omitempty"`
	SentNS     int64    `json:"sent_ns,omitempty"`
}

func Canonical(data json.RawMessage) ([]byte, error) {
	if len(data) == 0 {
		data = json.RawMessage(`{}`)
	}
	var v any
	// UseNumber preserves large integer payloads.
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}
func Digest(data json.RawMessage) string {
	b, err := Canonical(data)
	if err != nil {
		return ""
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
