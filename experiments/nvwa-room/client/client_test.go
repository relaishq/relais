package client

import (
	"context"
	"encoding/json"
	"github.com/relaishq/relais/experiments/nvwa-room/room"
	"github.com/stretchr/testify/require"
	"net/http/httptest"
	"testing"
	"time"
)

func TestTwoClientWebSocket(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r := room.New("e2e")
	s := httptest.NewServer(r.Handler())
	defer s.Close()
	a, err := Lobby(ctx, s.URL)
	require.NoError(t, err)
	b, err := Lobby(ctx, s.URL)
	require.NoError(t, err)
	dial := func(c Credentials) *Client {
		ws, e := DialWS(WSURL(s.URL))
		require.NoError(t, e)
		cl, e := Connect(ctx, ws, c, "")
		require.NoError(t, e)
		t.Cleanup(func() { _ = cl.Close() })
		return cl
	}
	ca, cb := dial(a), dial(b)
	p := json.RawMessage(`{"id":"wave","version":1,"start_tick":0}`)
	m := room.Message{Type: "trigger", CommandID: "action", InputSeq: 1, Payload: p, Digest: room.Digest(p)}
	v, err := ca.Request(ctx, m)
	require.NoError(t, err)
	require.True(t, v.Result.Applied)
	snapshot, err := cb.Request(ctx, room.Message{Type: "snapshot"})
	require.NoError(t, err)
	require.Equal(t, "wave", snapshot.Players[0].Action.ID)
	require.Equal(t, v.Version, snapshot.Version)
	require.NoError(t, ca.Close())
	ca = dial(a)
	v, err = ca.Request(ctx, m)
	require.NoError(t, err)
	require.EqualValues(t, 1, r.Metrics().Applied)
	require.True(t, v.Result.Applied)
}

// This cuts a real socket after commit, with the original request still waiting
// for its held receipt. It exercises the same RequestWithSent seam as the driver.
func TestInFlightCommandReconnect(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r := room.New("in-flight")
	r.AllowFaults = true
	server := httptest.NewServer(r.Handler())
	defer server.Close()
	creds, err := Lobby(ctx, server.URL)
	require.NoError(t, err)
	dial := func() *Client {
		ws, e := DialWS(WSURL(server.URL))
		require.NoError(t, e)
		c, e := Connect(ctx, ws, creds, "")
		require.NoError(t, e)
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	c := dial()
	payload := json.RawMessage(`{"id":"wave","version":1,"start_tick":0}`)
	command := room.Message{Type: "trigger", CommandID: "in-flight", InputSeq: 1, Payload: payload, Digest: room.Digest(payload), TestPauseMS: 2000}
	sent, done := make(chan struct{}), make(chan error, 1)
	go func() { _, e := c.RequestWithSent(ctx, command, func() { close(sent) }); done <- e }()
	select {
	case <-sent:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	require.Eventually(t, func() bool { return r.Metrics().HeldCommandID == command.CommandID }, time.Second, time.Millisecond)
	select {
	case e := <-done:
		t.Fatalf("receipt completed before cut: %v", e)
	default:
	}
	generation := c.Generation
	require.NoError(t, c.Close())
	require.Error(t, <-done)
	c = dial()
	require.Greater(t, c.Generation, generation)
	query, e := c.Request(ctx, room.Message{Type: "query", CommandID: command.CommandID})
	require.NoError(t, e)
	require.NotNil(t, query.Result)
	require.True(t, query.Result.Applied)
	command.TestPauseMS = 0
	retry, e := c.Request(ctx, command)
	require.NoError(t, e)
	require.Equal(t, query.Result, retry.Result)
	require.EqualValues(t, 1, r.Metrics().Applied)
	require.EqualValues(t, 1, r.Metrics().Deduplicated)
	require.EqualValues(t, 1, r.Metrics().HeldCommandApplications)
}
