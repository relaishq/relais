package room

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

type identity struct {
	player  Player
	token   string
	expires time.Time
	ledger  map[string]Result
	conn    *Connection
	session string
	rateAt  time.Time
	rate    int
}
type Metrics struct {
	Applied         uint64  `json:"applied"`
	Deduplicated    uint64  `json:"deduplicated"`
	Conflicts       uint64  `json:"conflicts"`
	StaleState      uint64  `json:"stale_state"`
	Fenced          uint64  `json:"fenced"`
	SlowConsumers   uint64  `json:"slow_consumers"`
	DroppedReceipts uint64  `json:"dropped_receipts"`
	MaxQueueBytes   int     `json:"max_queue_bytes"`
	MaxQueueAgeMS   float64 `json:"max_queue_age_ms"`
	Connections     int     `json:"connections"`
}
type Room struct {
	mu          sync.Mutex
	ID, Epoch   string
	version     uint64
	start       time.Time
	identities  map[string]*identity
	metrics     Metrics
	AllowFaults bool
}

func randomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func New(id string) *Room {
	return &Room{ID: id, Epoch: randomID(), start: time.Now(), identities: map[string]*identity{}}
}

// Lobby issues at most two 30-minute capabilities. There is no client-selected identity.
func (r *Room) Lobby() (string, string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.identities) >= 2 {
		return "", "", fmt.Errorf("lobby_full")
	}
	id := fmt.Sprintf("p%d", len(r.identities)+1)
	token := randomID()
	r.identities[token] = &identity{player: Player{ID: id, Character: "spike-character-v1"}, token: token, expires: time.Now().Add(30 * time.Minute), ledger: map[string]Result{}}
	return token, id, nil
}
func (r *Room) reply(m Message, kind string) Reply {
	return Reply{Type: kind, RequestID: m.RequestID, RoomID: r.ID, Epoch: r.Epoch, Version: r.version, Tick: time.Since(r.start).Milliseconds()}
}
func (r *Room) fail(m Message, code string) Reply { v := r.reply(m, "error"); v.Error = code; return v }
func (r *Room) snapshot(m Message, kind string) Reply {
	v := r.reply(m, kind)
	for _, id := range []string{"p1", "p2"} {
		for _, p := range r.identities {
			if p.player.ID == id {
				v.Players = append(v.Players, p.player)
			}
		}
	}
	return v
}
func (r *Room) Metrics() Metrics { r.mu.Lock(); defer r.mu.Unlock(); return r.metrics }
func (r *Room) Snapshot() Reply {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.snapshot(Message{}, "snapshot")
}
func (r *Room) Handle(c *Connection, m Message) *Reply {
	r.mu.Lock()
	defer r.mu.Unlock()
	fail := func(code string) *Reply { v := r.fail(m, code); return &v }
	if m.RoomID != r.ID {
		return fail("wrong_room")
	}
	if m.Epoch != "" && m.Epoch != r.Epoch {
		return fail("epoch_reset")
	}
	if m.Type == "join" {
		p := r.identities[m.Token]
		if p == nil || time.Now().After(p.expires) {
			return fail("invalid_token")
		}
		if c.player != "" {
			return fail("already_bound")
		}
		if m.SessionID != "" && len(m.SessionID) > 128 {
			return fail("invalid_session")
		}
		for _, other := range r.identities {
			if other != p && m.SessionID != "" && other.session == m.SessionID {
				return fail("session_in_use")
			}
		}
		if c.upstreamSession != "" && m.SessionID != c.upstreamSession {
			return fail("session_mismatch")
		}
		if p.conn != nil {
			p.conn.stop("taken_over")
		}
		p.player.Generation++
		p.player.Joined = true
		p.conn = c
		p.session = m.SessionID
		c.player = p.player.ID
		c.generation = p.player.Generation
		r.version++
		v := r.snapshot(m, "joined")
		v.PlayerID = p.player.ID
		v.Generation = p.player.Generation
		r.broadcast(c)
		return &v
	}
	var p *identity
	for _, candidate := range r.identities {
		if candidate.player.ID == c.player {
			p = candidate
		}
	}
	if p == nil {
		return fail("join_required")
	}
	if p.conn != c || m.Generation != p.player.Generation || c.generation != p.player.Generation {
		r.metrics.Fenced++
		return fail("stale_generation")
	}
	if m.Epoch != r.Epoch {
		return fail("epoch_required")
	}
	if time.Since(p.rateAt) >= time.Second {
		p.rateAt = time.Now()
		p.rate = 0
	}
	p.rate++
	if p.rate > MaxRate {
		return fail("rate_limit")
	}
	if !p.player.Joined && (m.Type == "state" || m.Type == "test_pause") {
		return fail("not_joined")
	}
	switch m.Type {
	case "test_pause":
		if !r.AllowFaults || m.TestPauseMS < 1 || m.TestPauseMS > 2000 {
			return fail("test_fault_refused")
		}
		c.mu.Lock()
		c.pauseUntil = time.Now().Add(time.Duration(m.TestPauseMS) * time.Millisecond)
		c.mu.Unlock()
		v := r.reply(m, "test_paused")
		return &v

	case "snapshot":
		v := r.snapshot(m, "snapshot")
		return &v
	case "query":
		v := r.reply(m, "result")
		if got, ok := p.ledger[m.CommandID]; ok {
			v.Result = &got
		} else {
			v.Error = "unknown_command"
		}
		return &v
	case "state":
		if len(m.Payload) > 128 || !json.Valid(m.Payload) {
			return fail("invalid_state")
		}
		v := r.reply(m, "state")
		v.Seq = m.Seq
		v.SentNS = m.SentNS
		if m.Seq <= p.player.StateSeq {
			r.metrics.StaleState++
			v.Error = "stale_state"
			return &v
		}
		p.player.StateSeq = m.Seq
		p.player.State = append(json.RawMessage(nil), m.Payload...)
		p.player.StateSentNS = m.SentNS
		r.version++
		v.Version = r.version
		r.broadcast(c)
		return &v
	case "trigger", "stop", "leave":
		if m.CommandID == "" || len(m.CommandID) > 64 || m.InputSeq == 0 || m.Digest == "" || m.Digest != Digest(m.Payload) {
			return fail("invalid_command")
		}
		if old, ok := p.ledger[m.CommandID]; ok {
			if old.Type != m.Type || old.InputSeq != m.InputSeq || old.Digest != m.Digest {
				r.metrics.Conflicts++
				return fail("command_conflict")
			}
			r.metrics.Deduplicated++
			v := r.reply(m, "result")
			v.Result = &old
			v.Error = old.Error
			return &v
		}
		if !p.player.Joined {
			return fail("not_joined")
		}
		if len(p.ledger) >= MaxCommands {
			return fail("ledger_full")
		}
		reject := func(code string) *Reply {
			result := Result{CommandID: m.CommandID, Type: m.Type, InputSeq: m.InputSeq, Digest: m.Digest, Version: r.version, Error: code}
			p.ledger[m.CommandID] = result
			v := r.reply(m, "result")
			v.Result = &result
			v.Error = code
			return &v
		}
		if m.Type == "trigger" {
			var action Action
			if json.Unmarshal(m.Payload, &action) != nil || action.ID != "wave" || action.Version != 1 || action.StartTick < 0 || action.StartTick > time.Since(r.start).Milliseconds()+60000 {
				return reject("invalid_action")
			}
			p.player.Action = &action
		} else if m.Type == "stop" {
			p.player.Action = nil
		} else {
			p.player.Joined = false
			p.session = ""
		}
		r.version++
		r.metrics.Applied++
		result := Result{CommandID: m.CommandID, Type: m.Type, InputSeq: m.InputSeq, Digest: m.Digest, Version: r.version, Applied: true}
		p.ledger[m.CommandID] = result
		r.broadcast(c)
		v := r.reply(m, "result")
		v.Result = &result
		if m.DropReceipt && r.AllowFaults {
			r.metrics.DroppedReceipts++
			return nil
		}
		return &v
	default:
		return fail("unknown_type")
	}
}
func (r *Room) broadcast(except *Connection) {
	v := r.snapshot(Message{}, "snapshot")
	for _, p := range r.identities {
		if p.conn != nil && p.conn != except && p.player.Joined {
			p.conn.send(v, "control")
		}
	}
}
