// Package operatortransport carries bounded JSON Lines over local pipes.
package operatortransport

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"sync"
	"time"

	"github.com/relaishq/relais/pkg/operator"
)

var ErrFrame = errors.New("invalid-protocol-frame")

type Codec struct {
	reader *bufio.Reader
	writer io.Writer
}

func NewCodec(r io.Reader, w io.Writer) *Codec {
	return &Codec{reader: bufio.NewReaderSize(r, operator.MaxFrameBytes), writer: w}
}
func (c *Codec) Read() (operator.Frame, error) {
	line, err := c.reader.ReadSlice('\n')
	if err != nil {
		if err == io.EOF && len(line) == 0 {
			return operator.Frame{}, io.EOF
		}
		return operator.Frame{}, ErrFrame
	}
	if len(line) > operator.MaxFrameBytes {
		return operator.Frame{}, ErrFrame
	}
	var f operator.Frame
	d := json.NewDecoder(bytes.NewReader(line))
	d.DisallowUnknownFields()
	if d.Decode(&f) != nil || d.Decode(new(any)) != io.EOF || !valid(f) {
		return operator.Frame{}, ErrFrame
	}
	return f, nil
}
func valid(f operator.Frame) bool {
	bodies := 0
	for _, yes := range []bool{f.Hello != nil, f.Init != nil, f.Request != nil, f.Result != nil, f.Trace != nil, f.Final != nil} {
		if yes {
			bodies++
		}
	}
	switch f.Type {
	case "hello":
		return bodies == 1 && f.Hello != nil && f.Hello.Protocol == operator.ProtocolVersion
	case "init":
		return bodies == 1 && f.Init != nil && f.Session != ""
	case "request":
		return bodies == 1 && f.Request != nil && f.Request.ID != "" && f.Session != ""
	case "result":
		return bodies == 1 && f.Result != nil && f.Result.ID != "" && f.Session != "" && f.Result.Transport.Valid() && validReplyBlob(*f.Result)
	case "trace":
		return bodies == 1 && f.Trace != nil && f.Session != ""
	case "final":
		return bodies == 1 && f.Final != nil && f.Session != ""
	case "cancel", "protocol_error":
		return bodies == 0 && (f.Code == "interrupted" || f.Code == "protocol-error")
	}
	return false
}
func encodeFrame(f operator.Frame) ([]byte, error) {
	if !valid(f) {
		return nil, ErrFrame
	}
	b, err := json.Marshal(f)
	if err != nil || len(b)+1 > operator.MaxFrameBytes {
		return nil, ErrFrame
	}
	return append(b, '\n'), nil
}
func (c *Codec) Write(f operator.Frame) error {
	b, err := encodeFrame(f)
	if err != nil {
		return err
	}
	return writeFrame(c.writer, b)
}
func writeFrame(w io.Writer, b []byte) error {
	n, err := w.Write(b)
	if err != nil {
		return err
	}
	if n != len(b) {
		return io.ErrShortWrite
	}
	return nil
}

type received struct {
	frame operator.Frame
	err   error
}

// Pipe has one reader and one writer. Close releases blocked pipe IO. Callers
// must supply real closers; cancellation closes this session, never replays it.
type Pipe struct {
	codec       *Codec
	reader      io.ReadCloser
	writer      io.WriteCloser
	incoming    chan received
	done        chan struct{}
	readStopped chan struct{}
	once        sync.Once
	// Publication and submission share this lock. A published fatal event fences
	// requests even when the session has not consumed that event yet.
	admission sync.Mutex
	fatal     error
	faulted   chan struct{}
	expected  map[string]bool
	inbound   map[string]bool
	encode    func(operator.Frame) ([]byte, error)
	now       func() time.Time
}

func NewPipe(r io.ReadCloser, w io.WriteCloser) *Pipe { return newPipe(r, w, encodeFrame) }
func newPipe(r io.ReadCloser, w io.WriteCloser, encode func(operator.Frame) ([]byte, error)) *Pipe {
	p := &Pipe{codec: NewCodec(r, w), reader: r, writer: w, incoming: make(chan received, 1), done: make(chan struct{}), readStopped: make(chan struct{}), faulted: make(chan struct{}), expected: map[string]bool{}, inbound: map[string]bool{}, encode: encode, now: time.Now}
	go func() {
		defer close(p.readStopped)
		for {
			f, err := p.codec.Read()
			err = p.publish(f, err)
			select {
			case p.incoming <- received{f, err}:
			case <-p.done:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	return p
}
func (p *Pipe) fence(err error) {
	if p.fatal == nil {
		p.fatal = err
		close(p.faulted)
	}
}
func (p *Pipe) publish(f operator.Frame, err error) error {
	p.admission.Lock()
	defer p.admission.Unlock()
	if err != nil || f.Type == "cancel" || f.Type == "protocol_error" {
		p.fence(ErrFrame)
		if err == nil {
			return ErrFrame
		}
		return err
	}
	key := f.Session + "/"
	switch f.Type {
	case "result":
		key += f.Result.ID
		terminal, known := p.expected[key]
		if !known || terminal {
			p.fence(ErrFrame)
			return ErrFrame
		}
		p.expected[key] = true
		if f.Result.Transport == operator.Unknown {
			p.fence(ErrFrame)
		}
	case "request":
		key += f.Request.ID
		if p.inbound[key] {
			p.fence(ErrFrame)
			return ErrFrame
		}
		if p.fatal != nil {
			return p.fatal
		}
		p.inbound[key] = true
	}
	return nil
}
func (p *Pipe) Close() error {
	p.once.Do(func() { close(p.done); _ = p.reader.Close(); _ = p.writer.Close() })
	return nil
}
func (p *Pipe) Receive(ctx context.Context) (operator.Frame, error) {
	select {
	case r := <-p.incoming:
		if r.err != nil {
			return operator.Frame{}, r.err
		}
		if r.frame.Type == "request" {
			p.admission.Lock()
			fatal := p.fatal
			p.admission.Unlock()
			if fatal != nil {
				return operator.Frame{}, fatal
			}
			budget, cancel, err := RequestContext(ctx, *r.frame.Request)
			if err == nil {
				err = budget.Err()
				cancel()
			}
			if err != nil {
				refusal := operator.Reply{ID: r.frame.Request.ID, Transport: operator.Failed, NotSubmitted: true, RefusalCode: "deadline-before-submission"}
				_ = p.Send(ctx, operator.Frame{Type: "result", Session: r.frame.Session, Result: &refusal})
				return operator.Frame{}, ErrExpired
			}
		}
		return r.frame, r.err
	case <-p.readStopped:
		select {
		case r := <-p.incoming:
			if r.err != nil {
				return operator.Frame{}, r.err
			}
			return operator.Frame{}, ErrFrame
		default:
			return operator.Frame{}, ErrFrame
		}
	case <-ctx.Done():
		_ = p.Close()
		return operator.Frame{}, ctx.Err()
	case <-p.done:
		return operator.Frame{}, io.ErrClosedPipe
	}
}
func (p *Pipe) Send(ctx context.Context, f operator.Frame) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if f.Type == "request" && f.Request != nil {
		bounded, cancel, err := RequestContext(ctx, *f.Request)
		if err != nil {
			return err
		}
		defer cancel()
		ctx = bounded
	}
	result := make(chan error, 1)
	go func() {
		b, err := p.encode(f)
		if err == nil {
			err = p.submit(ctx, f, b)
		}
		result <- err
	}()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		_ = p.Close()
		return ctx.Err()
	case <-p.done:
		return io.ErrClosedPipe
	}
}
func (p *Pipe) submit(ctx context.Context, f operator.Frame, b []byte) error {
	p.admission.Lock()
	defer p.admission.Unlock()
	return p.writeAdmitted(ctx, f, b)
}
func (p *Pipe) writeAdmitted(ctx context.Context, f operator.Frame, b []byte) error {
	// Encoding and writer scheduling consume the same budget. This is the last
	// admission check before the actual Write, under the publication lock.
	if err := ctx.Err(); err != nil {
		return err
	}
	if end, ok := ctx.Deadline(); ok && !time.Now().Before(end) {
		return context.DeadlineExceeded
	}
	select {
	case <-p.done:
		return io.ErrClosedPipe
	default:
	}
	if f.Type == "final" && p.fatal != nil && (f.Final.Status == operator.Passed || !f.Final.Latched || f.Final.Complete) {
		return ErrFrame
	}
	if f.Type == "request" {
		if p.fatal != nil {
			return p.fatal
		}
		if expired(*f.Request, p.now()) {
			return ErrExpired
		}
		key := f.Session + "/" + f.Request.ID
		if _, seen := p.expected[key]; seen {
			p.fence(ErrFrame)
			return ErrFrame
		}
		p.expected[key] = false
	}
	return writeFrame(p.writer, b)
}
func (p *Pipe) Poll() (operator.Frame, error, bool) {
	select {
	case r := <-p.incoming:
		return r.frame, r.err, true
	case <-p.done:
		return operator.Frame{}, io.ErrClosedPipe, true
	default:
		return operator.Frame{}, nil, false
	}
}

var ErrExpired = errors.New("deadline-before-submission")

func expired(r operator.Request, now time.Time) bool {
	end, err := strconv.ParseInt(r.DeadlineUnixMS, 10, 64)
	return err != nil || r.RemainingMS <= 0 || end <= now.UnixMilli()
}

// RequestContext is required at adapter effect admission too. Map the received
// remaining/absolute budget once; retain this context through execution/polling.
// An expired admission must return Failed + NotSubmitted with ErrExpired's code.
func RequestContext(parent context.Context, r operator.Request) (context.Context, context.CancelFunc, error) {
	now := time.Now()
	if expired(r, now) {
		return nil, nil, ErrExpired
	}
	end, _ := strconv.ParseInt(r.DeadlineUnixMS, 10, 64)
	remaining := min(r.RemainingMS, end-now.UnixMilli(), int64((20 * time.Minute).Milliseconds()))
	ctx, cancel := context.WithTimeout(parent, time.Duration(remaining)*time.Millisecond)
	if err := ctx.Err(); err != nil {
		cancel()
		return nil, nil, err
	}
	return ctx, cancel, nil
}

// Faulted is closed when the reader has published a fatal protocol event.
func (p *Pipe) Faulted() <-chan struct{} { return p.faulted }

// Finish builds the final artifact from the fatal state under the same lock
// that publishes faults and admits writes. Faults cannot hide between the
// session's final snapshot and its publication.
func (p *Pipe) Finish(ctx context.Context, build func(error) operator.Frame) error {
	result := make(chan error, 1)
	go func() {
		p.admission.Lock()
		defer p.admission.Unlock()
		f := build(p.fatal)
		if f.Type != "final" {
			result <- ErrFrame
			return
		}
		b, err := p.encode(f)
		if err == nil {
			err = p.writeAdmitted(ctx, f, b)
		}
		result <- err
	}()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		_ = p.Close()
		return ctx.Err()
	case <-p.done:
		return io.ErrClosedPipe
	}
}
