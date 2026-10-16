package room

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sync"
	"time"

	"golang.org/x/net/websocket"
)

type queued struct {
	data []byte
	at   time.Time
}
type Connection struct {
	room            *Room
	ws              *websocket.Conn
	upstreamSession string
	framed          bool
	player          string
	generation      uint64
	controlLabel    string
	mu              sync.Mutex
	queue           chan queued
	pending         int
	pendingAts      []time.Time
	maxPending      int
	maxAge          time.Duration
	done            chan struct{}
	writerDone      chan struct{}
	once            sync.Once
	terminal        string
	pauseUntil      time.Time
}

func newConnection(r *Room, ws *websocket.Conn, framed bool) *Connection {
	return &Connection{room: r, ws: ws, framed: framed, queue: make(chan queued, 128), done: make(chan struct{}), writerDone: make(chan struct{}), controlLabel: "control"}
}
func (c *Connection) stop(reason string) {
	c.once.Do(func() {
		c.mu.Lock()
		c.terminal = reason
		if len(c.pendingAts) > 0 {
			if age := time.Since(c.pendingAts[0]); age > c.maxAge {
				c.maxAge = age
			}
		}
		c.mu.Unlock()
		close(c.done)
	})
}
func (c *Connection) send(v Reply, label string) bool {
	data, err := json.Marshal(v)
	if err != nil || len(data) > MaxMessage {
		c.stop("outbound_message_too_large")
		return false
	}
	if c.framed {
		c.mu.Lock()
		if label == "control" {
			label = c.controlLabel
		}
		c.mu.Unlock()
		data = append(append([]byte{0, byte(len(label))}, []byte(label)...), data...)
	}
	c.mu.Lock()
	select {
	case <-c.done:
		c.mu.Unlock()
		return false
	default:
	}
	// Reserve a complete terminal error frame inside the same pending budget.
	reserve := MaxMessage
	if c.framed {
		reserve += 257
	}
	if c.pending+len(data) > MaxPending-reserve {
		c.mu.Unlock()
		c.stop("slow_consumer")
		return false
	}
	at := time.Now()
	c.pending += len(data)
	c.pendingAts = append(c.pendingAts, at)
	if c.pending > c.maxPending {
		c.maxPending = c.pending
	}
	select {
	case c.queue <- queued{data, at}:
		c.mu.Unlock()
		return true
	default:
		c.pending -= len(data)
		c.pendingAts = c.pendingAts[:len(c.pendingAts)-1]
		c.mu.Unlock()
		c.stop("slow_consumer")
		return false
	}
}
func (c *Connection) writer() {
	defer func() {
		c.stop("writer_closed")
		c.mu.Lock()
		reason, label := c.terminal, c.controlLabel
		c.mu.Unlock()
		if reason != "closed" && reason != "writer_closed" {
			data, _ := json.Marshal(Reply{Type: "error", Error: reason, RoomID: c.room.ID, Epoch: c.room.Epoch})
			if c.framed {
				data = append(append([]byte{0, byte(len(label))}, []byte(label)...), data...)
			}
			_ = c.ws.SetWriteDeadline(time.Now().Add(50 * time.Millisecond))
			_ = websocket.Message.Send(c.ws, data)
		}
		_ = c.ws.Close()
		close(c.writerDone)
	}()
	for {
		select {
		case <-c.done:
			return
		case q := <-c.queue:
			c.mu.Lock()
			pause := time.Until(c.pauseUntil)
			c.mu.Unlock()
			if pause > 0 {
				timer := time.NewTimer(pause)
				select {
				case <-timer.C:
				case <-c.done:
					timer.Stop()
					return
				}
			}
			select {
			case <-c.done:
				return
			default:
			}
			_ = c.ws.SetWriteDeadline(time.Now().Add(250 * time.Millisecond))
			err := websocket.Message.Send(c.ws, q.data)
			c.mu.Lock()
			c.pending -= len(q.data)
			if len(c.pendingAts) > 0 {
				c.pendingAts = c.pendingAts[1:]
			}
			age := time.Since(q.at)
			if age > c.maxAge {
				c.maxAge = age
			}
			c.mu.Unlock()
			if err != nil {
				return
			}
		}
	}
}
func (r *Room) serveWS(ws *websocket.Conn, framed bool) {
	r.mu.Lock()
	if r.metrics.Connections >= 16 {
		r.mu.Unlock()
		_ = ws.Close()
		return
	}
	r.metrics.Connections++
	r.mu.Unlock()
	c := newConnection(r, ws, framed)
	if framed {
		c.upstreamSession = ws.Request().URL.Query().Get("session")
		if c.upstreamSession == "" || len(c.upstreamSession) > 128 {
			c.stop("missing_session")
		}
	}
	ws.MaxPayloadBytes = MaxMessage
	if framed {
		ws.MaxPayloadBytes += 257
	}
	go c.writer()
	defer func() {
		c.stop("closed")
		<-c.writerDone
		c.mu.Lock()
		maxBytes, maxAge, reason := c.maxPending, c.maxAge, c.terminal
		c.mu.Unlock()
		r.mu.Lock()
		defer r.mu.Unlock()
		r.metrics.Connections--
		if maxBytes > r.metrics.MaxQueueBytes {
			r.metrics.MaxQueueBytes = maxBytes
		}
		if float64(maxAge)/1e6 > r.metrics.MaxQueueAgeMS {
			r.metrics.MaxQueueAgeMS = float64(maxAge) / 1e6
		}
		if reason == "slow_consumer" {
			r.metrics.SlowConsumers++
		}
		for _, p := range r.identities {
			if p.conn == c {
				p.conn = nil
			}
		}
	}()
	labels := map[string]bool{}
	rateAt, rate := time.Now(), 0
	for {
		_ = ws.SetReadDeadline(time.Now().Add(30 * time.Second))
		var data []byte
		if err := websocket.Message.Receive(ws, &data); err != nil {
			if errors.Is(err, websocket.ErrFrameTooLarge) {
				c.stop("message_too_large")
			}
			return
		}
		if time.Since(rateAt) >= time.Second {
			rateAt = time.Now()
			rate = 0
		}
		rate++
		if rate > MaxRate {
			c.stop("rate_limit")
			return
		}
		label := "control"
		if framed {
			if len(data) < 2 {
				return
			}
			op, n := data[0], int(data[1])
			if len(data) < 2+n {
				return
			}
			label = string(data[2 : 2+n])
			data = data[2+n:]
			switch op {
			case 1:
				var meta struct {
					Ordered        bool   `json:"ordered"`
					MaxRetransmits *int   `json:"maxRetransmits"`
					Protocol       string `json:"protocol"`
				}
				if n == 0 || len(labels) >= 4 || json.Unmarshal(data, &meta) != nil {
					return
				}
				labels[label] = true
				continue
			case 2:
				delete(labels, label)
				if label == c.controlLabel {
					return
				}
				continue
			case 3:
				return
			case 0:
				if !labels[label] {
					return
				}
			default:
				return
			}
		}
		if len(data) > MaxMessage {
			c.send(Reply{Type: "error", Error: "message_too_large", RoomID: r.ID, Epoch: r.Epoch}, label)
			return
		}
		var m Message
		if json.Unmarshal(data, &m) != nil {
			c.send(Reply{Type: "error", Error: "invalid_json", RoomID: r.ID, Epoch: r.Epoch}, label)
			continue
		}
		if m.Type == "join" {
			c.mu.Lock()
			c.controlLabel = label
			c.mu.Unlock()
		}
		if v := r.Handle(c, m); v != nil {
			if !c.send(*v, label) {
				return
			}
		}
	}
}
func (r *Room) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /lobby", func(w http.ResponseWriter, q *http.Request) {
		if !localOrigin(q) {
			http.Error(w, "origin_refused", 403)
			return
		}
		token, id, err := r.Lobby()
		if err != nil {
			http.Error(w, err.Error(), 409)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"token": token, "player_id": id, "room_id": r.ID, "room_epoch": r.Epoch})
	})
	for _, path := range []string{"/ws", "/relais-upstream"} {
		framed := path == "/relais-upstream"
		mux.Handle(path, websocket.Server{Handshake: func(_ *websocket.Config, q *http.Request) error {
			if !localOrigin(q) {
				return &url.Error{Op: "handshake", URL: q.URL.Path, Err: errOrigin{}}
			}
			return nil
		}, Handler: func(ws *websocket.Conn) { r.serveWS(ws, framed) }})
	}
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) { _ = json.NewEncoder(w).Encode(r.Metrics()) })
	mux.HandleFunc("GET /snapshot", func(w http.ResponseWriter, _ *http.Request) { _ = json.NewEncoder(w).Encode(r.Snapshot()) })
	return mux
}

type errOrigin struct{}

func (errOrigin) Error() string { return "origin_refused" }
func localOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, e := url.Parse(origin)
	return e == nil && (u.Host == r.Host || u.Host == "localhost" || u.Host == "127.0.0.1")
}
