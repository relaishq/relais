package audioagent_test

import (
	"context"
	"encoding/json"
	"net"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/relaishq/relais/experiments/nvwa-room/audioagent"
	"github.com/relaishq/relais/experiments/nvwa-room/client"
	"github.com/relaishq/relais/experiments/nvwa-room/room"
	"github.com/relaishq/relais/pkg/agent"
	"github.com/stretchr/testify/require"
)

func TestCrossSessionBusAndUnmappedSuppression(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	r := room.New("audio")
	s := httptest.NewServer(r.Handler())
	defer s.Close()
	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	require.NoError(t, err)
	defer udp.Close()
	go func() { _ = r.ServeAudio(udp) }()
	factory, err := audioagent.Factory("udp://" + udp.LocalAddr().String())
	require.NoError(t, err)
	var clients []*client.Client
	for _, id := range []string{"session-a", "session-b"} {
		creds, e := client.Lobby(ctx, s.URL)
		require.NoError(t, e)
		ws, e := client.DialWS(client.WSURL(s.URL))
		require.NoError(t, e)
		c, e := client.Connect(ctx, ws, creds, id)
		require.NoError(t, e)
		clients = append(clients, c)
		defer c.Close()
	}
	a, b := factory("session-a"), factory("session-b")
	out, err := a.Process(ctx, agent.Input{Payload: []byte("tone-a")})
	require.NoError(t, err)
	require.Empty(t, out.Audio)
	out, err = b.Process(ctx, agent.Input{Payload: []byte("tone-b")})
	require.NoError(t, err)
	require.Equal(t, []byte("tone-a"), out.Audio)
	out, err = a.Process(ctx, agent.Input{Payload: []byte("tone-a-2")})
	require.NoError(t, err)
	require.Equal(t, []byte("tone-b"), out.Audio)
	unknown := factory("unmapped")
	out, err = unknown.Process(ctx, agent.Input{Payload: []byte("not-authorized")})
	require.NoError(t, err)
	require.Empty(t, out.Audio)
	// Leave removes audio routing immediately, even if old packets remain cached.
	payload := json.RawMessage(`{}`)
	v, err := clients[1].Request(ctx, room.Message{Type: "leave", CommandID: "leave", InputSeq: 1, Payload: payload, Digest: room.Digest(payload)})
	require.NoError(t, err)
	require.Empty(t, v.Error)
	out, err = a.Process(ctx, agent.Input{Payload: []byte("tone-a-3")})
	require.NoError(t, err)
	require.Empty(t, out.Audio)
	saved, err := a.Save(ctx)
	require.NoError(t, err)
	require.NoError(t, a.Restore(ctx, saved))
	require.ErrorIs(t, a.Restore(ctx, agent.State{Version: 1}), agent.ErrVersion)
}
func TestBusValidation(t *testing.T) {
	for _, bad := range []string{"", "http://127.0.0.1:1234", "udp://192.0.2.1:1234", "udp://127.0.0.1:0"} {
		_, err := audioagent.Factory(bad)
		require.Error(t, err)
	}
}
