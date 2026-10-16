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
