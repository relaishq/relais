// Package client is the common Go room consumer. The connection implements app
// messages; the DataChannel path only needs to supply the same MessageConn seam.
package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/relaishq/relais/experiments/nvwa-room/room"
	"golang.org/x/net/websocket"
)

type MessageConn interface {
	Send([]byte) error
	Receive() ([]byte, error)
	Close() error
}
type WS struct{ Conn *websocket.Conn }

func (w *WS) Send(b []byte) error {
	_ = w.Conn.SetWriteDeadline(time.Now().Add(time.Second))
	return websocket.Message.Send(w.Conn, b)
}
func (w *WS) Receive() ([]byte, error) {
	var b []byte
	err := websocket.Message.Receive(w.Conn, &b)
	return b, err
}
func (w *WS) Close() error { return w.Conn.Close() }
func DialWS(url string) (MessageConn, error) {
	ws, err := websocket.Dial(url, "", "http://localhost")
	if err != nil {
		return nil, err
	}
	ws.MaxPayloadBytes = room.MaxMessage
	return &WS{ws}, nil
}

type Credentials struct {
	Token    string `json:"token"`
	PlayerID string `json:"player_id"`
	RoomID   string `json:"room_id"`
	Epoch    string `json:"room_epoch"`
}

func Lobby(ctx context.Context, base string) (Credentials, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", base+"/lobby", nil)
	if err != nil {
		return Credentials{}, err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return Credentials{}, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return Credentials{}, fmt.Errorf("lobby: %s", res.Status)
	}
	var c Credentials
	err = json.NewDecoder(res.Body).Decode(&c)
	return c, err
}

type Observation struct {
	AtNS  int64      `json:"at_ns"`
	Reply room.Reply `json:"reply"`
}
type Client struct {
	Conn         MessageConn
	Credentials  Credentials
	Generation   uint64
	SessionID    string
	counter      atomic.Uint64
	mu           sync.Mutex
	pending      map[string]chan room.Reply
	latest       room.Reply
	observations []Observation
	done         chan struct{}
	sendMu       sync.Mutex
}

func Connect(ctx context.Context, conn MessageConn, creds Credentials, session string) (*Client, error) {
	c := &Client{Conn: conn, Credentials: creds, SessionID: session, pending: map[string]chan room.Reply{}, done: make(chan struct{})}
	go c.read()
	v, err := c.Request(ctx, room.Message{Type: "join", Token: creds.Token, SessionID: session})
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	if v.Error != "" {
		_ = conn.Close()
		return nil, errors.New(v.Error)
	}
	c.Generation = v.Generation
	return c, nil
}
func (c *Client) read() {
	defer close(c.done)
	for {
		b, err := c.Conn.Receive()
		if err != nil {
			return
		}
		var v room.Reply
		if json.Unmarshal(b, &v) != nil {
			continue
		}
		c.mu.Lock()
		if len(c.observations) < 16000 {
			c.observations = append(c.observations, Observation{time.Now().UnixNano(), v})
		}
		if v.Version >= c.latest.Version {
			c.latest = v
		}
		ch := c.pending[v.RequestID]
		c.mu.Unlock()
		if ch != nil {
			select {
			case ch <- v:
			default:
			}
		}
	}
}
func (c *Client) Request(ctx context.Context, m room.Message) (room.Reply, error) {
	m.RequestID = fmt.Sprintf("r%d", c.counter.Add(1))
	m.RoomID = c.Credentials.RoomID
	m.Epoch = c.Credentials.Epoch
	m.Generation = c.Generation
	b, err := json.Marshal(m)
	if err != nil {
		return room.Reply{}, err
	}
	if len(b) > room.MaxMessage {
		return room.Reply{}, errors.New("message_too_large")
	}
	ch := make(chan room.Reply, 1)
	c.mu.Lock()
	c.pending[m.RequestID] = ch
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.pending, m.RequestID); c.mu.Unlock() }()
	c.sendMu.Lock()
	err = c.Conn.Send(b)
	c.sendMu.Unlock()
	if err != nil {
		return room.Reply{}, err
	}
	select {
	case v := <-ch:
		return v, nil
	case <-ctx.Done():
		return room.Reply{}, ctx.Err()
	case <-c.done:
		return room.Reply{}, errors.New("connection_closed")
	}
}
func (c *Client) Close() error { return c.Conn.Close() }
func (c *Client) Observations() []Observation {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Observation(nil), c.observations...)
}
func WSURL(base string) string { return "ws" + strings.TrimPrefix(base, "http") + "/ws" }
