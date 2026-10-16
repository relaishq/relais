package room

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/net/websocket"
)

func bound(t *testing.T) (*Room, *Connection, Message) {
	t.Helper()
	r := New("test")
	token, _, err := r.Lobby()
	require.NoError(t, err)
	c := newConnection(r, nil, false)
	v := r.Handle(c, Message{Type: "join", RoomID: r.ID, Token: token})
	require.Empty(t, v.Error)
	return r, c, Message{RoomID: r.ID, Epoch: r.Epoch, Generation: v.Generation}
}
func trigger(m Message) Message {
	m.Type = "trigger"
	m.CommandID = "cmd1"
	m.InputSeq = 1
	m.Payload = json.RawMessage(`{"id":"wave","version":1,"start_tick":0}`)
	m.Digest = Digest(m.Payload)
	return m
}
func TestDedupConflictAndGeneration(t *testing.T) {
	r, c, m := bound(t)
	m = trigger(m)
	first := r.Handle(c, m)
	require.True(t, first.Result.Applied)
	retry := r.Handle(c, m)
	require.Equal(t, first.Result, retry.Result)
	require.EqualValues(t, 1, r.Metrics().Applied)
	token := ""
	for k := range r.identities {
		token = k
	}
	next := newConnection(r, nil, false)
	joined := r.Handle(next, Message{RoomID: r.ID, Type: "join", Token: token, Epoch: r.Epoch})
	require.EqualValues(t, 2, joined.Generation)
	select {
	case <-c.done:
	default:
		t.Fatal("old subscription remains live")
	}
	require.Equal(t, "stale_generation", r.Handle(c, m).Error)
	m.Generation = joined.Generation
	require.Equal(t, first.Result, r.Handle(next, m).Result)
	m.InputSeq++
	require.Equal(t, "command_conflict", r.Handle(next, m).Error)
	require.EqualValues(t, 1, r.Metrics().Applied)
}
func TestEpochIdentityStateAndLimits(t *testing.T) {
	r, c, m := bound(t)
	other := New("test")
	require.NotEqual(t, r.Epoch, other.Epoch)
	m.Type = "query"
	m.Epoch = other.Epoch
	require.Equal(t, "epoch_reset", r.Handle(c, m).Error)
	m.Epoch = r.Epoch
	m.Type = "state"
	m.Seq = 2
	m.Payload = json.RawMessage(`{"x":2}`)
	require.Empty(t, r.Handle(c, m).Error)
	m.Seq = 1
	require.Equal(t, "stale_state", r.Handle(c, m).Error)
	require.EqualValues(t, 2, r.Snapshot().Players[0].StateSeq)
	unbound := newConnection(r, nil, false)
	require.Equal(t, "join_required", r.Handle(unbound, m).Error)
	_, _, err := r.Lobby()
	require.NoError(t, err)
	_, _, err = r.Lobby()
	require.Error(t, err)
	m = trigger(m)
	for i := 0; i < MaxCommands; i++ {
		r.identities[cToken(r)].ledger[string(rune(i+1))] = Result{}
	}
	require.Equal(t, "ledger_full", r.Handle(c, m).Error)
}
func cToken(r *Room) string {
	for k, p := range r.identities {
		if p.player.ID == "p1" {
			return k
		}
	}
	return ""
}
func TestQueueBoundAndExplicitSlowConsumer(t *testing.T) {
	r := New("q")
	c := newConnection(r, nil, false)
	for i := 0; i < 1000; i++ {
		if !c.send(Reply{Type: "snapshot", RoomID: r.ID, Epoch: r.Epoch, Players: []Player{{ID: "p1", State: json.RawMessage(`{"x":1}`)}}}, "control") {
			break
		}
	}
	require.LessOrEqual(t, c.maxPending, MaxPending)
	require.Equal(t, "slow_consumer", c.terminal)
	huge := newConnection(r, nil, false)
	require.False(t, huge.send(Reply{Error: string(make([]byte, 2000))}, "control"))
	require.Equal(t, "outbound_message_too_large", huge.terminal)
}
func TestReceiptDropAndInvalidAction(t *testing.T) {
	r, c, m := bound(t)
	r.AllowFaults = true
	m = trigger(m)
	m.DropReceipt = true
	require.Nil(t, r.Handle(c, m))
	q := m
	q.Type = "query"
	v := r.Handle(c, q)
	require.True(t, v.Result.Applied)
	require.EqualValues(t, 1, r.Metrics().Applied)
	m.DropReceipt = false
	require.Equal(t, v.Result, r.Handle(c, m).Result)
	m.CommandID = "bad"
	m.Payload = json.RawMessage(`{"id":"unknown","version":1}`)
	m.Digest = Digest(m.Payload)
	require.Equal(t, "invalid_action", r.Handle(c, m).Error)
}
func TestUpstreamFrozenFraming(t *testing.T) {
	r := New("up")
	token, _, _ := r.Lobby()
	s := httptest.NewServer(r.Handler())
	defer s.Close()
	ws, err := websocket.Dial("ws"+s.URL[4:]+"/relais-upstream?session=s1&worker=w1", "", "http://localhost")
	require.NoError(t, err)
	defer ws.Close()
	label := []byte("control")
	open := append([]byte{1, byte(len(label))}, label...)
	open = append(open, []byte(`{"ordered":true,"maxRetransmits":null,"protocol":""}`)...)
	require.NoError(t, websocket.Message.Send(ws, open))
	join, _ := json.Marshal(Message{Type: "join", RoomID: r.ID, Epoch: r.Epoch, Token: token, SessionID: "s1"})
	frame := append([]byte{0, byte(len(label))}, label...)
	require.NoError(t, websocket.Message.Send(ws, append(frame, join...)))
	_ = ws.SetReadDeadline(time.Now().Add(time.Second))
	var got []byte
	require.NoError(t, websocket.Message.Receive(ws, &got))
	require.Equal(t, frame, got[:len(frame)])
	var v Reply
	require.NoError(t, json.Unmarshal(got[len(frame):], &v))
	require.Equal(t, "joined", v.Type)
}

func TestRejectedCommandAlsoHasStableResult(t *testing.T) {
	r, c, m := bound(t)
	m = trigger(m)
	m.Payload = json.RawMessage(`{"id":"bad","version":1,"start_tick":0}`)
	m.Digest = Digest(m.Payload)
	first := r.Handle(c, m)
	require.Equal(t, "invalid_action", first.Error)
	require.False(t, first.Result.Applied)
	require.Equal(t, first.Result, r.Handle(c, m).Result)
	m.Payload = json.RawMessage(`{"id":"wave","version":1,"start_tick":0}`)
	m.Digest = Digest(m.Payload)
	require.Equal(t, "command_conflict", r.Handle(c, m).Error)
	require.Zero(t, r.Metrics().Applied)
}
func TestOversizeFrameHasTypedWireError(t *testing.T) {
	r := New("limit")
	server := httptest.NewServer(r.Handler())
	defer server.Close()
	ws, err := websocket.Dial("ws"+server.URL[4:]+"/ws", "", "http://localhost")
	require.NoError(t, err)
	defer ws.Close()
	require.NoError(t, websocket.Message.Send(ws, make([]byte, MaxMessage+1)))
	_ = ws.SetReadDeadline(time.Now().Add(time.Second))
	var b []byte
	require.NoError(t, websocket.Message.Receive(ws, &b))
	var got Reply
	require.NoError(t, json.Unmarshal(b, &got))
	require.Equal(t, "message_too_large", got.Error)
}

func TestLeaveFencesNewMutationButKeepsLedger(t *testing.T) {
	r, c, m := bound(t)
	m = trigger(m)
	original := r.Handle(c, m)
	leave := m
	leave.Type = "leave"
	leave.CommandID = "leave"
	leave.InputSeq = 2
	leave.Payload = json.RawMessage(`{}`)
	leave.Digest = Digest(leave.Payload)
	require.True(t, r.Handle(c, leave).Result.Applied)
	require.Equal(t, original.Result, r.Handle(c, m).Result)
	m.CommandID = "new"
	require.Equal(t, "not_joined", r.Handle(c, m).Error)
	require.EqualValues(t, 2, r.Metrics().Applied)
}
