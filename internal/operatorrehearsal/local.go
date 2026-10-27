package operatorrehearsal

import (
	"context"
	"errors"
	"sync"

	"github.com/relaishq/relais/pkg/operator"
)

// LocalPeer plugs directly into operatorgates.Serve. It shares Execute with
// Adapter.Serve; neither path has its own gate loop, latch or result policy.
type LocalPeer struct {
	adapter *Adapter
	session string
	init    operator.Init
	inbox   chan operator.Frame
	done    chan struct{}
	once    sync.Once
	final   *operator.Result
}

func NewLocalPeer(a *Adapter, session string, init operator.Init) *LocalPeer {
	return &LocalPeer{adapter: a, session: session, init: init, inbox: make(chan operator.Frame, 1), done: make(chan struct{})}
}
func (p *LocalPeer) Send(ctx context.Context, f operator.Frame) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	select {
	case <-p.done:
		return errors.New("local-peer-closed")
	default:
	}
	var reply operator.Frame
	switch f.Type {
	case "hello":
		if f.Hello == nil || f.Hello.Protocol != operator.ProtocolVersion {
			return errors.New("invalid-hello")
		}
		reply = operator.Frame{Type: "init", Session: p.session, Init: &p.init}
	case "request":
		if f.Session != p.session || f.Request == nil {
			return errors.New("invalid-request")
		}
		r := p.adapter.Execute(ctx, *f.Request)
		reply = operator.Frame{Type: "result", Session: p.session, Result: &r}
	case "trace":
		return nil
	default:
		return errors.New("invalid-local-frame")
	}
	select {
	case p.inbox <- reply:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-p.done:
		return errors.New("local-peer-closed")
	}
}
func (p *LocalPeer) Receive(ctx context.Context) (operator.Frame, error) {
	select {
	case f := <-p.inbox:
		return f, nil
	case <-ctx.Done():
		return operator.Frame{}, ctx.Err()
	case <-p.done:
		return operator.Frame{}, errors.New("local-peer-closed")
	}
}
func (p *LocalPeer) Poll() (operator.Frame, error, bool) {
	select {
	case f := <-p.inbox:
		return f, nil, true
	default:
		return operator.Frame{}, nil, false
	}
}
func (p *LocalPeer) Close() error { p.once.Do(func() { close(p.done) }); return nil }
func (p *LocalPeer) Finish(ctx context.Context, build func(error) operator.Frame) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	f := build(nil)
	if f.Type != "final" || f.Final == nil {
		return errors.New("invalid-local-final")
	}
	p.final = f.Final
	return nil
}

// Final is available after the synchronous session Serve returns.
func (p *LocalPeer) Final() *operator.Result { return p.final }
