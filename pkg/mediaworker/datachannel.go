package mediaworker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/datachannel"
	"github.com/pion/dtls/v3"
	"github.com/pion/sctp"
	"golang.org/x/net/websocket"
)

// Version 9 is deliberately unreadable by media-only v7/v8 workers. It is a
// reconnect marker, never a partial SCTP checkpoint. Media envelopes stay v7/v8.
const dataSessionStateVersion = 9
const dataReceiveBytes = 64 << 10
const dataIOTimeout = time.Second

var (
	ErrDataSessionNotMovable        = errors.New("mediaworker: data session not movable")
	ErrDataSessionReconnectRequired = errors.New("mediaworker: data session reconnect required")
	ErrDataChannelOffer             = fmt.Errorf("%w: unsupported DataChannel offer", ErrUnsupportedOffer)
	ErrDataChannelMessageTooLarge   = errors.New("mediaworker: datachannel_message_too_large")
	ErrDataChannelPendingOverflow   = errors.New("mediaworker: datachannel_pending_overflow")
	ErrDataChannelUnsupported       = errors.New("mediaworker: datachannel_unsupported")
)

// DataChannelError is a channel-local failure, visible in the upstream close
// frame, worker log and optional OnError observer. It never ends media.
type DataChannelError struct {
	SessionID, Label, Code string
	Cause                  error
}

func (e *DataChannelError) Error() string {
	return fmt.Sprintf("session %s channel %q: %s", e.SessionID, e.Label, e.Code)
}
func (e *DataChannelError) Unwrap() error { return e.Cause }

// DataChannelConfig opts into the frozen binary upstream protocol. Callbacks
// must return promptly and must be safe for concurrent channel failures.
type DataChannelConfig struct {
	Upstream, WorkerName                string
	MaxMessage, MaxPending, MaxChannels int
	OnError                             func(*DataChannelError)
}

func (c *DataChannelConfig) defaults() error {
	if c.MaxMessage == 0 {
		c.MaxMessage = 1024
	}
	if c.MaxPending == 0 {
		c.MaxPending = 16384
	}
	if c.MaxChannels == 0 {
		c.MaxChannels = 4
	}
	u, err := url.Parse(c.Upstream)
	if err != nil || u.Host == "" || (u.Scheme != "ws" && u.Scheme != "wss") || u.Fragment != "" || u.User != nil {
		return errors.New("mediaworker: datachannel requires a ws/wss upstream URL")
	}
	if c.MaxMessage < 1 || c.MaxMessage > dataReceiveBytes || c.MaxPending < c.MaxMessage || c.MaxPending > 1<<20 || c.MaxChannels < 1 || c.MaxChannels > 16 {
		return errors.New("mediaworker: invalid datachannel bounds (message 1..65536, pending message..1048576, channels 1..16)")
	}
	return nil
}

// SnapshotIsDataSession reads the reconnect classification without requiring
// SRTP, DTLS secrets or a live source. An inconsistent marker fails closed.
func SnapshotIsDataSession(raw []byte) (bool, error) {
	var marker struct {
		Version int
		State   struct {
			Data    bool
			Version int
		}
	}
	if err := json.Unmarshal(raw, &marker); err != nil {
		return false, fmt.Errorf("%w: %v", errBadState, err)
	}
	if marker.Version == dataSessionStateVersion || marker.State.Version == dataSessionStateVersion || marker.State.Data {
		return true, nil
	}
	return false, nil
}

// CheckSessionMove is source preflight. It does not pause or flush input.
func (w *Worker) CheckSessionMove(id string) error {
	s := w.session(id)
	if s == nil {
		return ErrUnknownSession
	}
	if s.state.Data {
		return ErrDataSessionNotMovable
	}
	return nil
}

// EndSessionReason terminates a data session with an upstream reason. Media
// sessions keep their ordinary close path.
func (w *Worker) EndSessionReason(id, reason string) error {
	s := w.session(id)
	if s == nil {
		return ErrUnknownSession
	}
	s.mu.Lock()
	s.endReason = reason
	s.mu.Unlock()
	s.close()
	return nil
}

type dataCounters struct{ opened, closed, oversize, overflow, dropped, unknown atomic.Uint64 }
type DataChannelStats struct{ Opened, Closed, Oversized, PendingOverflow, DroppedOldest, UnknownLabel uint64 }

func (w *Worker) DataChannelStats() DataChannelStats {
	c := &w.dataCounters
	return DataChannelStats{c.opened.Load(), c.closed.Load(), c.oversize.Load(), c.overflow.Load(), c.dropped.Load(), c.unknown.Load()}
}

type dataSession struct {
	s          *session
	cfg        DataChannelConfig
	maxMessage int
	mu         sync.Mutex
	channels   map[string]*dataPipe
	ws         *websocket.Conn
	stopped    bool
	ending     bool
	writeMu    sync.Mutex
}
type dataPipe struct {
	owner    *dataSession
	dc       *datachannel.DataChannel
	reliable bool
	mu       sync.Mutex
	queue    [][]byte
	pending  int
	closed   bool
	wake     chan struct{}
}

func newDataSession(s *session, maxMessage int) *dataSession {
	return &dataSession{s: s, cfg: *s.worker.cfg.DataChannel, maxMessage: maxMessage, channels: make(map[string]*dataPipe)}
}
func (d *dataSession) report(label, code string, cause error) *DataChannelError {
	e := &DataChannelError{SessionID: d.s.id, Label: label, Code: code, Cause: cause}
	d.s.log.Warnf("%v", e)
	if d.cfg.OnError != nil {
		d.cfg.OnError(e)
	}
	return e
}

// SCTP is the ONLY reader of DTLS application records for a data session.
// No transparent resume is attempted even if SCTP ends while media continues.
func (d *dataSession) serve(conn *dtls.Conn) {
	a, err := sctp.ServerWithOptions(sctp.Config{NetConn: conn, MaxReceiveBufferSize: dataReceiveBytes, MaxMessageSize: uint32(max(d.maxMessage, 1024)), LoggerFactory: d.s.worker.cfg.LoggerFactory}, sctp.WithMaxReassemblyQueueEntries(uint32(max(16, (d.maxMessage+1023)/1024))))
	if err != nil {
		return
	}
	defer func() { _ = a.Close() }()
	// Bound association setup/accept by the session lifetime through conn.Close.
	for accepted := 0; ; accepted++ {
		stream, err := a.AcceptStream()
		if err != nil {
			return
		}
		if accepted >= d.cfg.MaxChannels {
			_ = d.report("", "datachannel_max_channels", ErrDataChannelUnsupported)
			return
		}
		_ = stream.SetReadDeadline(time.Now().Add(2 * dataIOTimeout))
		dc, err := datachannel.Server(stream, &datachannel.Config{LoggerFactory: d.s.worker.cfg.LoggerFactory})
		if err != nil {
			_ = d.report("", "datachannel_unsupported", errors.Join(ErrDataChannelUnsupported, err))
			_ = stream.Close()
			continue
		}
		_ = dc.SetReadDeadline(time.Time{})
		if err := d.add(dc); err != nil {
			_ = dc.Close()
		}
	}
}
func (d *dataSession) add(dc *datachannel.DataChannel) error {
	switch dc.ChannelType {
	case datachannel.ChannelTypeReliable, datachannel.ChannelTypeReliableUnordered, datachannel.ChannelTypePartialReliableRexmit, datachannel.ChannelTypePartialReliableRexmitUnordered:
	default:
		return d.report(dc.Label, "datachannel_unsupported", ErrDataChannelUnsupported)
	}
	if len(dc.Label) == 0 || len(dc.Label) > 255 || len(dc.Protocol) > 255 {
		return d.report(dc.Label, "datachannel_unsupported", ErrDataChannelUnsupported)
	}
	d.mu.Lock()
	if d.stopped || d.channels[dc.Label] != nil {
		d.mu.Unlock()
		return ErrDataChannelUnsupported
	}
	// Dial once, on the first accepted channel. Dial is bounded and never under
	// the session/media mutex. A failed upstream is terminal for data only.
	if d.ws == nil {
		u, _ := url.Parse(d.cfg.Upstream)
		q := u.Query()
		q.Set("session", d.s.id)
		q.Set("worker", d.cfg.WorkerName)
		u.RawQuery = q.Encode()
		cfg, err := websocket.NewConfig(u.String(), "http://localhost/")
		if err != nil {
			d.mu.Unlock()
			return err
		}
		ctx, cancel := context.WithTimeout(d.s.ctx, 2*dataIOTimeout)
		ws, err := cfg.DialContext(ctx)
		cancel()
		if err != nil {
			d.stopped = true
			d.mu.Unlock()
			return err
		}
		ws.PayloadType = websocket.BinaryFrame
		ws.MaxPayloadBytes = max(d.cfg.MaxMessage, 1024) + 257
		d.ws = ws
		go d.receive(ws)
	}
	p := &dataPipe{owner: d, dc: dc, reliable: dc.ChannelType == datachannel.ChannelTypeReliable, wake: make(chan struct{}, 1)}
	d.channels[dc.Label] = p
	d.mu.Unlock()
	dc.SetBufferedAmountLowThreshold(uint64(d.cfg.MaxPending / 2))
	dc.OnBufferedAmountLow(p.signal)
	var retransmits *uint32
	if dc.ChannelType == datachannel.ChannelTypePartialReliableRexmit || dc.ChannelType == datachannel.ChannelTypePartialReliableRexmitUnordered {
		v := dc.ReliabilityParameter
		retransmits = &v
	}
	open, _ := json.Marshal(struct {
		Ordered        bool    `json:"ordered"`
		MaxRetransmits *uint32 `json:"maxRetransmits"`
		Protocol       string  `json:"protocol"`
	}{dc.ChannelType&0x80 == 0, retransmits, dc.Protocol})
	if err := d.send(1, dc.Label, open); err != nil {
		d.stopChannels("upstream_closed", nil)
		return err
	}
	d.s.worker.dataCounters.opened.Add(1)
	go p.read()
	go p.write()
	return nil
}
func (d *dataSession) send(op byte, label string, payload []byte) error {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	d.mu.Lock()
	ws := d.ws
	rejected := d.ending && op != 3 || op <= 1 && (d.stopped || d.s.ctx.Err() != nil)
	d.mu.Unlock()
	if rejected || ws == nil {
		return io.ErrClosedPipe
	}
	_ = ws.SetWriteDeadline(time.Now().Add(dataIOTimeout))
	frame := make([]byte, 2+len(label)+len(payload))
	frame[0] = op
	frame[1] = byte(len(label))
	copy(frame[2:], label)
	copy(frame[2+len(label):], payload)
	return websocket.Message.Send(ws, frame)
}
func (d *dataSession) receive(ws *websocket.Conn) {
	defer func() { d.stopChannels("upstream_closed", nil); _ = ws.Close() }()
	for {
		var frame []byte
		if err := dataWSCodec.Receive(ws, &frame); err != nil {
			if errors.Is(err, websocket.ErrFrameTooLarge) {
				d.s.worker.dataCounters.oversize.Add(1)
				d.stopChannels("datachannel_message_too_large", ErrDataChannelMessageTooLarge)
			}
			return
		}
		if len(frame) < 2 || int(frame[1])+2 > len(frame) {
			return
		}
		label := frame[2 : 2+int(frame[1])]
		payload := frame[2+int(frame[1]):]
		if frame[0] != 0 {
			continue
		}
		d.mu.Lock()
		p := d.channels[string(label)]
		d.mu.Unlock()
		if p == nil {
			d.s.worker.dataCounters.unknown.Add(1)
			continue
		}
		p.enqueue(payload)
	}
}
func (d *dataSession) stopChannels(code string, cause error) {
	d.mu.Lock()
	d.stopped = true
	pipes := make([]*dataPipe, 0, len(d.channels))
	for _, p := range d.channels {
		pipes = append(pipes, p)
	}
	d.mu.Unlock()
	for _, p := range pipes {
		p.close(code, cause)
	}
}
func (d *dataSession) end(reason string) {
	d.mu.Lock()
	d.ending = true
	d.stopped = true
	d.mu.Unlock()
	payload, _ := json.Marshal(struct {
		Reason string `json:"reason"`
	}{reason})
	// One bounded write precedes close; if the peer is gone this is best effort.
	_ = d.send(3, "", payload)
	d.mu.Lock()
	ws := d.ws
	d.mu.Unlock()
	if ws != nil {
		_ = ws.Close()
	}
	d.stopChannels(reason, nil)
}
func (p *dataPipe) signal() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}
func (p *dataPipe) close(code string, cause error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	p.queue = nil
	p.pending = 0
	p.mu.Unlock()
	p.owner.s.worker.dataCounters.closed.Add(1)
	if cause != nil {
		_ = p.owner.report(p.dc.Label, code, cause)
	}
	_ = p.dc.Close()
	p.signal()
	payload, _ := json.Marshal(struct {
		Reason string `json:"reason"`
	}{code})
	_ = p.owner.send(2, p.dc.Label, payload)
}
func (p *dataPipe) oversized(n int) bool {
	if n <= p.owner.maxMessage {
		return false
	}
	p.owner.s.worker.dataCounters.oversize.Add(1)
	if p.reliable {
		p.close("datachannel_message_too_large", ErrDataChannelMessageTooLarge)
	}
	return true
}
func (p *dataPipe) read() {
	// A short read is an oversize message. SCTP reassembly itself is bounded
	// separately by the association receive window, not by this scratch slice.
	buf := make([]byte, dataReceiveBytes)
	for {
		n, _, err := p.dc.ReadDataChannel(buf)
		if errors.Is(err, io.ErrShortBuffer) {
			p.close("datachannel_message_too_large", ErrDataChannelMessageTooLarge)
			return
		}
		if err != nil {
			p.close("channel_closed", nil)
			return
		}
		p.mu.Lock()
		closed := p.closed
		p.mu.Unlock()
		if closed {
			return
		}
		if p.oversized(n) {
			if p.reliable {
				return
			}
			continue
		}
		if err := p.owner.send(0, p.dc.Label, buf[:n]); err != nil {
			p.owner.stopChannels("upstream_closed", nil)
			return
		}
	}
}
func (p *dataPipe) enqueue(payload []byte) {
	if p.oversized(len(payload)) {
		return
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	// Count zero-byte messages as one byte so metadata cannot grow unbounded.
	cost := max(1, len(payload))
	if p.pending+cost > p.owner.cfg.MaxPending {
		p.owner.s.worker.dataCounters.overflow.Add(1)
		if p.reliable {
			p.mu.Unlock()
			p.close("datachannel_pending_overflow", ErrDataChannelPendingOverflow)
			return
		}
		for p.pending+cost > p.owner.cfg.MaxPending && len(p.queue) > 0 {
			p.pending -= max(1, len(p.queue[0]))
			p.queue[0] = nil
			p.queue = p.queue[1:]
			p.owner.s.worker.dataCounters.dropped.Add(1)
		}
	}
	p.queue = append(p.queue, append([]byte(nil), payload...))
	p.pending += cost
	p.mu.Unlock()
	p.signal()
}
func (p *dataPipe) write() {
	// BufferedAmount has no hard cap in Pion. This sole writer enforces a
	// MaxPending high water mark before EACH write (including in-flight data).
	// Polling also handles unreliable abandonment without a low callback.
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-p.owner.s.ctx.Done():
			return
		case <-p.wake:
		case <-tick.C:
		}
		for {
			p.mu.Lock()
			if p.closed {
				p.mu.Unlock()
				return
			}
			if len(p.queue) == 0 || p.dc.BufferedAmount()+uint64(max(1, len(p.queue[0]))) > uint64(p.owner.cfg.MaxPending) {
				p.mu.Unlock()
				break
			}
			msg := p.queue[0]
			p.queue[0] = nil
			p.queue = p.queue[1:]
			p.pending -= max(1, len(msg))
			p.mu.Unlock()
			if _, err := p.dc.WriteDataChannel(msg, false); err != nil {
				p.close("channel_write_failed", err)
				return
			}
		}
	}
}

// Compile-time proof that component DTLS can carry SCTP directly.
var _ net.Conn = (*dtls.Conn)(nil)

// x/net's generic Message codec also accepts text. This adapter accepts only
// the binary frames in the frozen interface and keeps MaxPayloadBytes active.
var dataWSCodec = websocket.Codec{Unmarshal: func(data []byte, kind byte, v any) error {
	if kind != websocket.BinaryFrame {
		return errors.New("mediaworker: upstream frame must be binary")
	}
	*(v.(*[]byte)) = data
	return nil
}}
