// Package workerlink tunnels the private worker API over one outbound HTTP/2
// stream. A link never retries a command and has no role in worker liveness.
package workerlink

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/relaishq/relais/internal/privateapi"
)

const ContractVersion = 1
const ChunkSize = 32 << 10 // JSON base64 plus metadata stays below 64 KiB.
const MaxFrame = 64 << 10
const MaxConcurrent = 64
const PingInterval = 500 * time.Millisecond
const LostAfter = 1500 * time.Millisecond

var ErrNotSent = privateapi.ErrNotSent
var ErrUnavailable = errors.New("workerlink: link unavailable; retryable")

var ErrAfterSend = errors.New("workerlink: command reply unavailable")

// Message is also the wire header. Body chunks can interleave by ID. Timeout
// carries the remaining duration in nanoseconds, independent of host clocks.
// The inbound HTTP API does not propagate a deadline to the worker.
type Message struct {
	Kind    string `json:"kind"`
	ID      uint64 `json:"id,omitempty"`
	Timeout int64  `json:"timeout_ns,omitempty"`
	Error   string `json:"error,omitempty"`
	Method  string `json:"method,omitempty"`
	Path    string `json:"path,omitempty"`
	Epoch   uint64 `json:"epoch,omitempty"`
	Status  int    `json:"status,omitempty"`
	Body    []byte `json:"body,omitempty"`
	More    bool   `json:"more,omitempty"`
}

func Read(r io.Reader) (Message, error) {
	var size [4]byte
	if _, err := io.ReadFull(r, size[:]); err != nil {
		return Message{}, err
	}
	n := binary.BigEndian.Uint32(size[:])
	if n == 0 || n > MaxFrame {
		return Message{}, errors.New("workerlink: invalid frame size")
	}
	data := make([]byte, n)
	if _, err := io.ReadFull(r, data); err != nil {
		return Message{}, err
	}
	var m Message
	err := json.Unmarshal(data, &m)
	return m, err
}

func Write(w io.Writer, m Message) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if len(data) > MaxFrame {
		return errors.New("workerlink: frame too large")
	}
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(data)))
	if _, err = w.Write(size[:]); err != nil {
		return err
	}
	if _, err = w.Write(data); err != nil {
		return err
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

// Peer serves the existing handler and/or acts as an http.RoundTripper. Close
// must interrupt its reader and writer (the HTTP request/response stream).
type Peer struct {
	reader    io.Reader
	writer    io.Writer
	interrupt func()
	handler   http.Handler
	writeMu   sync.Mutex
	mu        sync.Mutex
	pending   map[uint64]chan Message
	next      atomic.Uint64
	last      atomic.Int64
	done      chan struct{}
	once      sync.Once
}

func New(r io.Reader, w io.Writer, interrupt func(), handler http.Handler) *Peer {
	p := &Peer{reader: r, writer: w, interrupt: interrupt, handler: handler, pending: make(map[uint64]chan Message), done: make(chan struct{})}
	p.last.Store(time.Now().UnixNano())
	return p
}
func (p *Peer) Done() <-chan struct{} { return p.done }
func (p *Peer) Close()                { p.once.Do(func() { close(p.done); p.interrupt() }) }

func (p *Peer) send(m Message, deadline time.Time) error {
	body := m.Body
	for {
		n := min(len(body), ChunkSize)
		m.Body, m.More = body[:n], len(body) > n
		p.writeMu.Lock()
		select {
		case <-p.done:
			p.writeMu.Unlock()
			return ErrUnavailable
		default:
		}
		if !deadline.IsZero() {
			m.Timeout = int64(time.Until(deadline))
			deadline = time.Time{} // Keep identical metadata on later chunks.
		}
		err := Write(p.writer, m)
		p.writeMu.Unlock()
		if err != nil {
			p.Close()
			return err
		}
		if !m.More {
			return nil
		}
		body = body[n:]
		runtime.Gosched()
	}
}

// Run owns the read loop; callers start it once. Both sides emit application
// pings as well as transport HTTP/2 health checks, so a live connection cannot
// hide a stalled command stream.
func (p *Peer) Run(ctx context.Context) error {

	//nolint:staticcheck // Joining the writer prevents ResponseWriter use after the link handler returns.
	defer func() { p.Close(); p.writeMu.Lock(); p.writeMu.Unlock() }()

	// A blocked write must not block the loss detector. The interrupt callback
	// cancels both directions when the watchdog observes a silent stream.
	ping := make(chan struct{}, 1)
	go func() {
		for {
			select {
			case <-p.done:
				return
			case <-ping:
				if err := p.send(Message{Kind: "ping"}, time.Time{}); err != nil {
					return
				}
			}
		}
	}()
	go func() {
		ticker := time.NewTicker(PingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				p.Close()
				return
			case <-p.done:
				return
			case <-ticker.C:
				if time.Since(time.Unix(0, p.last.Load())) >= LostAfter {
					p.Close()
					return
				}
				select {
				case ping <- struct{}{}:
				default:
				}
			}
		}
	}()
	partial := make(map[uint64]Message)
	slots := make(chan struct{}, MaxConcurrent)
	for {
		m, err := Read(p.reader)
		if err != nil {
			return err
		}
		p.last.Store(time.Now().UnixNano())
		if m.Kind == "ping" {
			continue
		}
		if m.Kind != "request" && m.Kind != "reply" {
			return errors.New("workerlink: unexpected message")
		}
		if old, ok := partial[m.ID]; ok {
			if old.Kind != m.Kind || old.Method != m.Method || old.Path != m.Path || old.Timeout != m.Timeout || old.Error != m.Error || old.Epoch != m.Epoch || old.Status != m.Status {
				return errors.New("workerlink: changed chunk header")
			}
			m.Body = append(old.Body, m.Body...)
		}
		if len(m.Body) > privateapi.MaxBody {
			return errors.New("workerlink: body too large")
		}
		if m.More {
			_, continuing := partial[m.ID]
			if !continuing && len(partial) >= MaxConcurrent {
				return errors.New("workerlink: too many partial messages")
			}
			partial[m.ID] = m
			continue
		}
		delete(partial, m.ID)
		if m.Kind == "reply" {
			p.mu.Lock()
			reply := p.pending[m.ID]
			p.mu.Unlock()
			if reply != nil {
				select {
				case reply <- m:
				default:
				}
			}
			continue
		}
		if p.handler == nil {
			return errors.New("workerlink: unsolicited request")
		}
		select {
		case slots <- struct{}{}:
		default:
			_ = p.send(Message{Kind: "reply", ID: m.ID, Error: "not_applied"}, time.Time{})
			continue
		}
		go func(m Message) {
			reply := p.serve(ctx, m)
			<-slots // Handler capacity is independent of reply backpressure.
			_ = p.send(reply, time.Time{})
		}(m)
	}
}

func (p *Peer) serve(ctx context.Context, m Message) Message {
	reply := Message{Kind: "reply", ID: m.ID}
	if m.Timeout <= 0 || m.Timeout > int64(privateapi.Timeout) {
		reply.Error = "not_applied"
		return reply
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(m.Timeout))
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, m.Method, "http://worker"+m.Path, bytes.NewReader(m.Body))
	if err != nil || ctx.Err() != nil {
		reply.Error = "not_applied"
		return reply
	}
	req.Header.Set("Content-Type", "application/json")
	if m.Epoch != 0 {
		req.Header.Set("X-Relais-Epoch", fmtEpoch(m.Epoch))
	}
	recorder := &responseBuffer{header: make(http.Header), code: http.StatusOK}
	p.handler.ServeHTTP(recorder, req)
	if recorder.overflow {
		reply.Error = "uncertain"
		return reply
	}
	reply.Status, reply.Body = recorder.code, recorder.body.Bytes()
	return reply
}

func (p *Peer) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(req.Context(), privateapi.Timeout)
	defer cancel()
	var input io.Reader = http.NoBody
	if req.Body != nil {
		input = req.Body
	}
	body, err := io.ReadAll(io.LimitReader(input, privateapi.MaxBody+1))
	if err != nil {
		return nil, errors.Join(ErrNotSent, err)
	}
	if len(body) > privateapi.MaxBody {
		return nil, errors.Join(ErrNotSent, errors.New("workerlink: body too large"))
	}
	id := p.next.Add(1)
	reply := make(chan Message, 1)
	p.mu.Lock()
	if len(p.pending) >= MaxConcurrent {
		p.mu.Unlock()
		return nil, errors.Join(ErrNotSent, ErrUnavailable)
	}
	p.pending[id] = reply
	p.mu.Unlock()
	defer func() { p.mu.Lock(); delete(p.pending, id); p.mu.Unlock() }()
	deadline, _ := ctx.Deadline()
	m := Message{Kind: "request", ID: id, Timeout: int64(time.Until(deadline)), Method: req.Method, Path: req.URL.RequestURI(), Body: body, Epoch: parseEpoch(req.Header.Get("X-Relais-Epoch"))}
	select {
	case <-p.done:
		return nil, errors.Join(ErrNotSent, ErrUnavailable)
	case <-ctx.Done():
		return nil, errors.Join(ErrNotSent, ctx.Err())
	default:
	}
	// Once sending starts, any failure is uncertain. A partial write can still
	// reach the peer; reconnect never replays it.
	sent := make(chan error, 1)
	go func() { sent <- p.send(m, deadline) }()
	select {
	case err = <-sent:
		if err != nil {
			if errors.Is(err, ErrUnavailable) {
				err = ErrAfterSend
			}
			return nil, privateapi.Uncertain(err)
		}
	case <-ctx.Done():
		p.Close()
		return nil, privateapi.Uncertain(ctx.Err())
	case <-p.done:
		return nil, privateapi.Uncertain(ErrAfterSend)
	}
	select {
	case m = <-reply:
		// A reply arriving after the caller deadline cannot win the select race.
		if err := ctx.Err(); err != nil {
			return nil, privateapi.Uncertain(err)
		}
		if !time.Now().Before(deadline) {
			return nil, privateapi.Uncertain(context.DeadlineExceeded)
		}
		switch m.Error {
		case "not_applied":
			return nil, ErrNotSent
		case "uncertain":
			return nil, privateapi.Uncertain(errors.New("workerlink: unusable command reply"))
		case "":
		default:
			return nil, privateapi.Uncertain(errors.New("workerlink: unknown command outcome"))
		}
		return &http.Response{StatusCode: m.Status, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(m.Body)), Request: req}, nil
	case <-ctx.Done():
		return nil, privateapi.Uncertain(ctx.Err())
	case <-p.done:
		return nil, privateapi.Uncertain(ErrAfterSend)
	}
}

// Bound replies while the handler writes, including an oversized error body.
type responseBuffer struct {
	header   http.Header
	code     int
	body     bytes.Buffer
	overflow bool
}

func (b *responseBuffer) Header() http.Header  { return b.header }
func (b *responseBuffer) WriteHeader(code int) { b.code = code }
func (b *responseBuffer) Write(data []byte) (int, error) {
	if len(data) > privateapi.MaxBody-b.body.Len() {
		b.overflow = true
		return 0, errors.New("workerlink: response too large")
	}
	return b.body.Write(data)
}
