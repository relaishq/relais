package agentbridge

import (
	"bufio"
	"context"
	"crypto/sha1" // WebSocket handshake, RFC 6455.
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/mediaworker"

	"github.com/stretchr/testify/require"
	"golang.org/x/net/websocket"
)

type blockedWrite struct {
	net.Conn
	entered chan struct{}
}

func (c *blockedWrite) Write(p []byte) (int, error) {
	if c.entered != nil {
		select {
		case c.entered <- struct{}{}:
		default:
		}
	}
	return c.Conn.Write(p)
}
func (*blockedWrite) SetWriteDeadline(time.Time) error { return nil }

func TestBlockedSocketDoesNotEnterAgentCallbacks(t *testing.T) {
	client, server := net.Pipe()
	defer func() { _ = server.Close() }()
	c := &blockedWrite{Conn: client}
	handshake := make(chan error, 1)
	go func() {
		request, err := http.ReadRequest(bufio.NewReader(server))
		if err != nil {
			handshake <- err
			return
		}
		sum := sha1.Sum([]byte(request.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
		_, err = fmt.Fprintf(server, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", base64.StdEncoding.EncodeToString(sum[:]))
		handshake <- err
	}()
	cfg, err := websocket.NewConfig("ws://127.0.0.1/bridge", "http://localhost")
	require.NoError(t, err)
	conn, err := websocket.NewClient(cfg, c)
	require.NoError(t, err)
	require.NoError(t, <-handshake)
	defer func() { _ = conn.Close() }()
	c.entered = make(chan struct{}, 1)
	a := newAdapter(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err = a.Handle(ctx, agent.Event{Kind: agent.Control, Control: &agent.ControlMessage{ID: "blocked", Sequence: 1, Name: "test"}})
	require.NoError(t, err)
	a.Accepted()
	sent := make(chan error, 1)
	go func() { sent <- a.bridge.write(ctx, conn, a, mediaworker.BridgeOutputScope{}, make(chan any), nil) }()
	select {
	case <-c.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("Bridge.write never entered the controlled blocked socket")
	}
	callbacks := make(chan error, 1)
	go func() {
		_, err := a.Handle(ctx, agent.Event{Kind: agent.Control, Control: &agent.ControlMessage{ID: "next", Sequence: 2, Name: "test"}})
		if err == nil {
			_, err = a.Save(ctx)
		}
		if err == nil {
			a.Accepted()
			_, err = a.SaveMoveOnly(ctx)
		}
		callbacks <- err
	}()
	select {
	case err := <-callbacks:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("agent callbacks waited for Bridge.write's blocked socket")
	}
	select {
	case err := <-sent:
		t.Fatalf("Bridge.write completed before peer release: %v", err)
	default:
	}
	require.NoError(t, server.Close())
	require.Error(t, <-sent)
}
