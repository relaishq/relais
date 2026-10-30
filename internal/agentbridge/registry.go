package agentbridge

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/mediaworker"
	"golang.org/x/net/websocket"
)

// The development pool has one runner slot per worker. A runner can register
// with multiple eligible workers; an exact move reserves the same bot there.
// Provider/tool starts require a live, renewal-backed permit.
type runner struct {
	id, bot     string
	conn        *websocket.Conn
	slot        chan struct{}
	replies     chan reservationReply
	replyMu     sync.Mutex
	reservation string
	revokeSlot  chan struct{}
	revoking    *revocation
	revoked     chan struct{}
	done        chan struct{}
}
type reservationReply struct {
	Type     string `json:"type"`
	ID       string `json:"reservation_id"`
	Accepted bool   `json:"accepted"`
}
type assignment struct {
	scope    mediaworker.BridgeOutputScope
	expires  time.Time
	attached chan attachment
	claimed  chan struct{}
}
type attachment struct {
	release    func()
	conn       *websocket.Conn
	lastOutput uint64
	done       chan struct{}
}
type attachRequest struct {
	Type       string `json:"type"`
	Protocol   int    `json:"protocol"`
	SessionID  string `json:"session_id"`
	Bot        string `json:"bot_instance"`
	Epoch      uint64 `json:"epoch"`
	Generation uint64 `json:"generation"`
	Token      string `json:"attach_token"`
	LastOutput uint64 `json:"last_output_seq"`
}

func nonce() string {
	var v [32]byte
	if _, err := rand.Read(v[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(v[:])
}
func identity(v string) bool {
	if len(v) == 0 || len(v) > 64 {
		return false
	}
	for _, c := range v {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' && c != '_' && c != '.' {
			return false
		}
	}
	return true
}

func (b *Bridge) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	host, _, err := net.SplitHostPort(req.RemoteAddr)
	ip := net.ParseIP(host)
	if err != nil || ip == nil || !ip.IsLoopback() || req.Header.Get("Origin") != "" || req.URL.RawQuery != "" || (req.URL.Path != "/bridge/registry" && req.URL.Path != "/bridge") {
		http.Error(w, "loopback only", http.StatusForbidden)
		return
	}
	if subtle.ConstantTimeCompare([]byte(req.Header.Get("Authorization")), []byte("Bearer "+b.config.Token)) != 1 {
		http.Error(w, "development authorization required", http.StatusUnauthorized)
		return
	}
	websocket.Server{Handshake: func(*websocket.Config, *http.Request) error { return nil }, Handler: func(c *websocket.Conn) {
		c.MaxPayloadBytes = 32 << 10
		defer func() { _ = c.Close() }()
		_ = c.SetDeadline(time.Now().Add(time.Second))
		if req.URL.Path == "/bridge/registry" {
			b.register(c)
		} else {
			b.attach(c)
		}
	}}.ServeHTTP(w, req)
}
func (b *Bridge) register(c *websocket.Conn) {
	var hello struct {
		Type     string `json:"type"`
		Protocol int    `json:"protocol"`
		RunnerID string `json:"runner_id"`
		Bot      string `json:"bot_instance"`
		Agent    string `json:"agent_name"`
		Revision string `json:"config_revision"`
		Capacity int    `json:"capacity"`
	}
	if websocket.JSON.Receive(c, &hello) != nil || hello.Type != "hello" || hello.Protocol != 1 || !identity(hello.RunnerID) || !identity(hello.Bot) || hello.Agent != "bridge" || hello.Revision != "1" || hello.Capacity != 1 {
		return
	}
	r := &runner{revokeSlot: make(chan struct{}, 1), revoked: make(chan struct{}, 1), id: hello.RunnerID, bot: hello.Bot, conn: c, replies: make(chan reservationReply, 1), done: make(chan struct{}), slot: make(chan struct{}, 1)}
	b.mu.Lock()
	if b.runner != nil {
		b.mu.Unlock()
		return
	}
	b.runner = r
	close(b.changed)
	b.changed = make(chan struct{})
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		b.runner = nil
		close(r.done)
		close(b.changed)
		b.changed = make(chan struct{})
		b.mu.Unlock()
	}()
	_ = c.SetDeadline(time.Time{})
	if sendJSON(c, map[string]any{"type": "registered", "protocol": 1}) != nil {
		return
	}
	for {
		var raw json.RawMessage
		if websocket.JSON.Receive(c, &raw) != nil {
			return
		}
		var header struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(raw, &header) != nil {
			return
		}
		switch header.Type {
		case "reserved":
			var reply reservationReply
			if json.Unmarshal(raw, &reply) != nil || r.receiveReply(reply) != nil {
				return
			}
		case "permit_request":
			var request permitRequest
			if json.Unmarshal(raw, &request) != nil || b.permit(r, request) != nil {
				return
			}
		case "revoked":
			var ack revocation
			if json.Unmarshal(raw, &ack) != nil {
				return
			}
			r.receiveRevoked(ack)
		default:
			return
		}
	}
}
func (b *Bridge) attach(c *websocket.Conn) {
	var m attachRequest
	if websocket.JSON.Receive(c, &m) != nil || m.Type != "attach" || m.Protocol != 1 {
		return
	}
	b.mu.Lock()
	p := b.pending[m.Token]
	stopped := b.revocations[m.SessionID] != nil
	if stopped || p == nil || time.Now().After(p.expires) || m.SessionID != p.scope.SessionID || m.Bot != p.scope.BotInstance || m.Epoch != p.scope.Epoch || m.Generation != p.scope.Generation {
		b.mu.Unlock()
		return
	}
	delete(b.pending, m.Token) // Single use, including a failed attachment.
	a := attachment{conn: c, lastOutput: m.LastOutput, done: make(chan struct{})}
	p.attached <- a
	b.mu.Unlock()
	timer := time.NewTimer(time.Until(p.expires))
	defer timer.Stop()
	select {
	case <-p.claimed:
		<-a.done
	case <-timer.C:
	}
}
func (b *Bridge) reserve(ctx context.Context, id string, epoch uint64, existing mediaworker.BridgeOutputScope) (_ *runner, _ *assignment, resultErr error) {
	for {
		b.mu.Lock()
		r, changed := b.runner, b.changed
		b.mu.Unlock()
		if r == nil {
			select {
			case <-changed:
				continue
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			}
		}
		// One outstanding reservation exchange. Waiting never holds a host lock.
		select {
		case r.slot <- struct{}{}:
		case <-r.done:
			return nil, nil, errProtocol
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
		defer func() {
			if resultErr != nil {
				<-r.slot
			}
		}()
		if existing.BotInstance != "" && existing.BotInstance != r.bot {
			return nil, nil, errProtocol
		}
		scope := existing
		if scope.BotInstance == "" {
			scope = mediaworker.BridgeOutputScope{SessionID: id, BotInstance: r.bot, Generation: 1, LineageID: nonce()}
		}
		scope.Epoch = epoch
		reservation := nonce()
		r.setReservation(reservation)
		defer r.setReservation("")
		expires := time.Now().Add(5 * time.Second)
		if err := sendJSON(r.conn, map[string]any{"type": "reserve", "session_id": id, "epoch": epoch, "reservation_id": reservation, "expires_ms": expires.UnixMilli()}); err != nil {
			return nil, nil, err
		}
		timer := time.NewTimer(time.Until(expires))
		defer timer.Stop()

	waitReserved:
		for {
			select {
			case reply := <-r.replies:
				if reply.ID != reservation {
					continue
				}
				if !reply.Accepted {
					return nil, nil, errProtocol
				}
				break waitReserved
			case <-r.done:
				return nil, nil, errProtocol
			case <-timer.C:
				return nil, nil, context.DeadlineExceeded
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			}
		}
		token := nonce()
		p := &assignment{scope: scope, expires: expires, attached: make(chan attachment, 1), claimed: make(chan struct{})}
		b.mu.Lock()
		b.pending[token] = p
		b.mu.Unlock()
		if err := sendJSON(r.conn, map[string]any{"type": "assign", "session_id": id, "epoch": epoch, "bot_instance": scope.BotInstance, "generation": scope.Generation, "lineage_id": scope.LineageID, "attach_token": token, "expires_ms": expires.UnixMilli()}); err != nil {
			b.removeAssignment(p)
			return nil, nil, err
		}
		return r, p, nil
	}
}
func (b *Bridge) removeAssignment(p *assignment) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for token, v := range b.pending {
		if v == p {
			delete(b.pending, token)
		}
	}
}
func (b *Bridge) connection(ctx context.Context, run *agent.TaskRun, a *adapter) (attachment, mediaworker.BridgeOutputScope, string, *runner, error) {
	existing, err := b.worker.BridgeOutputBinding(a.id)
	if err != nil && !errors.Is(err, mediaworker.ErrBridgeOutputUnbound) {
		return attachment{}, existing, "", nil, err
	}
	epoch, err := b.worker.BridgeOutputEpoch(a.id)
	if err != nil {
		return attachment{}, existing, "", nil, err
	}
	r, p, err := b.reserve(ctx, a.id, epoch, existing)
	if err != nil {
		return attachment{}, existing, "", nil, err
	}
	defer b.removeAssignment(p)
	success := false
	defer func() {
		if !success {
			<-r.slot
		}
	}()
	timer := time.NewTimer(time.Until(p.expires))
	defer timer.Stop()
	var conn attachment
	select {
	case conn = <-p.attached:
		close(p.claimed)
	case <-r.done:
		return conn, existing, "", nil, errProtocol
	case <-ctx.Done():
		return conn, existing, "", nil, ctx.Err()
	case <-timer.C:
		return conn, existing, "", nil, context.DeadlineExceeded
	}
	fail := func(e error) (attachment, mediaworker.BridgeOutputScope, string, *runner, error) {
		close(conn.done)
		return attachment{}, existing, "", nil, e
	}
	if existing.BotInstance != "" && a.runnerID.Load() != r.id {
		return fail(errProtocol)
	}
	data := []byte(`{"runner_id":"` + r.id + `"}`)
	if err = run.Emit(ctx, run.After()+1, agent.Chunk{Data: data}); err != nil {
		return fail(err)
	}
	if existing.BotInstance == "" {
		err = b.worker.BindBridgeOutput(p.scope)
	} else {
		err = b.worker.ReattachBridgeOutput(p.scope, conn.lastOutput)
	}
	if err != nil {
		return fail(err)
	}
	// New identity must reach a fenced checkpoint before attached. Exact same-
	// lineage moves wait for their own renewal/checkpoint before permit grants.
	if existing.BotInstance == "" {
		poll := time.NewTicker(10 * time.Millisecond)
		defer poll.Stop()
		for {
			if _, err = b.worker.BridgeOutputCheckpointed(p.scope); err == nil {
				break
			}
			select {
			case <-poll.C:
			case <-ctx.Done():
				return fail(ctx.Err())
			case <-timer.C:
				return fail(context.DeadlineExceeded)
			}
		}
	}
	resume := "fresh"
	if existing.BotInstance != "" {
		resume = "planned_move"
	}
	b.mu.Lock()
	b.active[a.id] = activeBot{runner: r, scope: p.scope, done: conn.done}
	b.mu.Unlock()
	conn.release = func() {
		b.mu.Lock()
		if active := b.active[a.id]; active.done == conn.done {
			delete(b.active, a.id)
		}
		b.mu.Unlock()
		<-r.slot
	}
	success = true
	return conn, p.scope, resume, r, nil
}

// Listen binds only the literal loopback address validated by Config.
func (b *Bridge) Listen() (net.Listener, error) {
	u, _ := url.Parse(b.config.URL)
	return net.Listen("tcp", u.Host)
}

// setReservation switches the single outstanding RPC and retires replies from
// its predecessor. Slot ownership serializes this with the next request.
func (r *runner) setReservation(id string) {
	r.replyMu.Lock()
	defer r.replyMu.Unlock()
	r.reservation = id
	select {
	case <-r.replies:
	default:
	}
}

// A delayed or duplicate response is valid registry traffic, not a reason to
// disconnect the runner. Only the first response for the active RPC is queued.
func (r *runner) receiveReply(reply reservationReply) error {
	if reply.Type != "reserved" {
		return errProtocol
	}
	r.replyMu.Lock()
	defer r.replyMu.Unlock()
	if r.reservation == "" || reply.ID != r.reservation {
		return nil
	}
	select {
	case r.replies <- reply:
	default:
	}
	return nil
}
