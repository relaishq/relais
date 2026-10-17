package room

import (
	"encoding/binary"
	"encoding/json"
	"net/http"
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

func TestUpstreamSessionEndReasonIsRecorded(t *testing.T) {
	r := New("end")
	server := httptest.NewServer(r.Handler())
	defer server.Close()
	ws, err := websocket.Dial("ws"+server.URL[4:]+"/relais-upstream?session=ended-call&worker=w1", "", "http://localhost")
	require.NoError(t, err)
	defer ws.Close()
	require.NoError(t, websocket.Message.Send(ws, append([]byte{3, 0}, []byte(`{"reason":"reconnect_required"}`)...)))
	require.Eventually(t, func() bool {
		events := r.Metrics().Events
		return len(events) == 1 && events[0].SessionID == "ended-call" && events[0].Op == 3 && events[0].Reason == "reconnect_required"
	}, time.Second, time.Millisecond)
	copied := r.Metrics()
	copied.Events[0].Reason = "changed"
	require.Equal(t, "reconnect_required", r.Metrics().Events[0].Reason)
}

func TestMetricsEncodingWhileTransportEventsRollOver(t *testing.T) {
	r := New("metrics-race")
	c := newConnection(r, nil, true)
	c.upstreamSession = "owned-session"
	server := httptest.NewServer(r.Handler())
	defer server.Close()
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			r.recordTransportEvent(c, 3, "", []byte(`{"reason":"reconnect_required"}`))
		}
	}()
	defer func() { close(stop); <-done }()
	require.Eventually(t, func() bool { return len(r.Metrics().Events) == 64 }, time.Second, time.Millisecond)
	for range 64 {
		response, err := server.Client().Get(server.URL + "/metrics")
		require.NoError(t, err)
		var metrics Metrics
		err = json.NewDecoder(response.Body).Decode(&metrics)
		_ = response.Body.Close()
		require.NoError(t, err)
		require.LessOrEqual(t, len(metrics.Events), 64)
	}
	require.Len(t, r.Metrics().Events, 64)
}

func TestBurstProbeIsBoundedAndOptIn(t *testing.T) {
	plain := httptest.NewServer(New("plain").Handler())
	defer plain.Close()
	response, err := plain.Client().Get(plain.URL + "/test-burst")
	require.NoError(t, err)
	_ = response.Body.Close()
	require.Equal(t, http.StatusNotFound, response.StatusCode)
	r := New("probe")
	r.AllowFaults = true
	server := httptest.NewServer(r.Handler())
	defer server.Close()
	ws, err := websocket.Dial("ws"+server.URL[4:]+"/test-burst", "", "http://localhost")
	require.NoError(t, err)
	defer ws.Close()
	_ = ws.SetDeadline(time.Now().Add(3 * time.Second))
	for n := range 200 {
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], uint32(n))
		require.NoError(t, websocket.Message.Send(ws, b[:]))
	}
	for n := range 200 {
		var b []byte
		require.NoError(t, websocket.Message.Receive(ws, &b))
		require.Len(t, b, 4)
		require.EqualValues(t, n, binary.BigEndian.Uint32(b))
	}
	require.NoError(t, websocket.Message.Send(ws, []byte{0, 0, 0, 0}))
	var b []byte
	err = websocket.Message.Receive(ws, &b)
	if err == nil {
		var reply Reply
		require.NoError(t, json.Unmarshal(b, &reply))
		require.Equal(t, "test_burst_limit", reply.Error)
	}
	require.Zero(t, r.Metrics().Applied)
}

// The actual send queue must route state broadcasts onto the selected state
// channel. A control-only snapshot would hide the partial-reliability variant.
func TestNetworkStateBroadcastUsesStateChannel(t *testing.T) {
	r, c, m := bound(t)
	r.NetworkStateChannel = true
	token, _, err := r.Lobby()
	require.NoError(t, err)
	peer := newConnection(r, nil, true)
	joined := r.Handle(peer, Message{Type: "join", RoomID: r.ID, Token: token})
	require.Empty(t, joined.Error)
	m.Type, m.Seq, m.SentNS, m.Payload = "state", 2, 123, json.RawMessage(`{"x":2}`)
	require.Empty(t, r.Handle(c, m).Error)
	frame := <-peer.queue
	size := int(frame.data[1])
	require.Equal(t, "state", string(frame.data[2:2+size]))
	var update Reply
	require.NoError(t, json.Unmarshal(frame.data[2+size:], &update))
	require.Equal(t, "state-update", update.Type)
	require.EqualValues(t, 2, update.Seq)
	require.EqualValues(t, 123, update.SentNS)
	require.Equal(t, "p1", update.PlayerID)
	require.Equal(t, m.Payload, update.Players[0].State)
	// Commands still broadcast on control.
	m = trigger(m)
	require.True(t, r.Handle(c, m).Result.Applied)
	frame = <-peer.queue
	require.Equal(t, "control", string(frame.data[2:2+int(frame.data[1])]))
	update = Reply{}
	require.NoError(t, json.Unmarshal(frame.data[2+int(frame.data[1]):], &update))
	for _, player := range update.Players {
		require.Empty(t, player.State)
		require.Zero(t, player.StateSeq)
		require.Zero(t, player.StateSentNS)
	}
}

func TestNetworkBarrierDoesNotPauseSurvivingWebSocket(t *testing.T) {
	r, c, m := bound(t)
	r.AllowFaults = true
	m = trigger(m)
	m.TestBarrier, m.DropReceipt = true, true
	require.Nil(t, r.Handle(c, m))
	require.EqualValues(t, 1, r.Metrics().HeldCommandApplications)
	require.True(t, c.pauseUntil.IsZero())
	m.DropReceipt, m.TestBarrier = false, false
	retry := r.Handle(c, m)
	require.True(t, retry.Result.Applied)
	require.EqualValues(t, 1, r.Metrics().Applied)
	require.EqualValues(t, 1, r.Metrics().Deduplicated)
}

func TestPrivateRoomOriginsAreHeadersNotLoopbackDestinations(t *testing.T) {
	for _, test := range []struct {
		origin  string
		allowed bool
	}{
		{"", true}, {"http://10.189.80.4:33001", true}, {"http://localhost", true}, {"http://localhost/", true}, {"http://8.8.8.8:33001", false},
	} {
		request := httptest.NewRequest("POST", "http://10.189.80.4:33001/lobby", nil)
		request.Header.Set("Origin", test.origin)
		require.Equal(t, test.allowed, localOrigin(request), test.origin)
	}
}
