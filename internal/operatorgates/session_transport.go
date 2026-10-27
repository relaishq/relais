package operatorgates

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"time"

	"github.com/relaishq/relais/internal/operatortransport"
	"github.com/relaishq/relais/pkg/operator"
)

// Peer is implemented by local pipes and in-process protocol adapters.
type Peer interface {
	Send(context.Context, operator.Frame) error
	Receive(context.Context) (operator.Frame, error)
	Poll() (operator.Frame, error, bool)
	Close() error
	Finish(context.Context, func(error) operator.Frame) error
}

// CheckInbox fences unsolicited/late frames before the serial request path.
// Concurrent pure reads use Issue/Accept through their owning event loop.
func (s *Session) CheckInbox(p Peer) error {
	f, err, ready := p.Poll()
	if !ready {
		return nil
	}
	code := "protocol-error"
	if err == nil && f.Type == "cancel" {
		code = "interrupted"
	}
	s.Halt(code)
	return errors.New(code)
}

// Exchange is the production serial request path used by gate implementations. A lost
// pipe never leads to a second submission. Timeout preserves pending identity.
func (s *Session) Exchange(ctx context.Context, p Peer, op operator.Operation, target, action string, args json.RawMessage, script []byte, end time.Duration, classify func(operator.Reply) operator.Certainty) (operator.Certainty, error) {
	if ctx.Err() != nil {
		s.Halt("interrupted")
		return operator.Unknown, ctx.Err()
	}
	if err := s.CheckInbox(p); err != nil {
		return operator.Unknown, err
	}
	req, err := s.Issue(op, target, action, args, script, end)
	if err != nil {
		return operator.Unknown, err
	}
	wait, cancel := context.WithTimeout(ctx, s.requests[req.ID].end-s.clock.Elapsed())
	defer cancel()
	if err = p.Send(wait, operator.Frame{Type: "request", Session: s.ID, Request: &req}); err != nil {
		s.TransportLost()
		return operator.Unknown, err
	}
	for {
		f, err := p.Receive(wait)
		if err != nil {
			s.TransportLost()
			return operator.Unknown, err
		}
		if f.Type == "cancel" {
			s.Halt("interrupted")
			return operator.Unknown, errors.New("interrupted")
		}
		if f.Type != "result" {
			s.Halt("protocol-error")
			return operator.Unknown, errors.New("protocol-error")
		}
		resolved, err := operatortransport.ResolveReply(s.spool, *f.Result)
		if err != nil {
			s.Halt("invalid-blob")
			return operator.Unknown, err
		}
		f.Result = &resolved
		evidence := operator.Unknown
		if f.Result.ID == req.ID {
			evidence = classify(*f.Result)
		}
		certainty, err := s.Accept(f.Session, *f.Result, evidence)
		if err != nil || f.Result.ID == req.ID {
			return certainty, err
		}
	}
}

// Serve owns one session. Later gate modules register public implementations;
// an absent implementation remains explicitly not-run and cannot pass.
type GateRunner func(context.Context, *Session, Peer, operator.Gate) operator.GateResult

func Serve(ctx context.Context, p Peer, hello operator.Hello, run GateRunner, spool ...*os.Root) error {
	defer p.Close()
	if err := p.Send(ctx, operator.Frame{Type: "hello", Hello: &hello}); err != nil {
		return err
	}
	frame, err := p.Receive(ctx)
	if err != nil {
		return err
	}
	if frame.Type != "init" {
		return errors.New("expected-init")
	}
	s, err := NewSession(frame.Session, *frame.Init, NewRealClock())
	if err != nil {
		return err
	}
	if len(spool) > 0 {
		s.SetBlobRoot(spool[0])
	}
	results := []operator.GateResult{}
	for _, g := range frame.Init.Scenario.Gates {
		r := operator.GateResult{Gate: g, Status: operator.NotRun, Reason: "not-implemented"}
		_ = s.CheckInbox(p)
		if ctx.Err() != nil {
			s.Halt("interrupted")
		}
		if s.Latched() {
			r.Reason = "prior-gate-stopped"
		} else if end, ok := s.BeginGate(); !ok {
			r.Reason = "gate-deadline"
		} else if run != nil {
			gateCtx, cancel := context.WithTimeout(ctx, end-s.clock.Elapsed())
			r = s.CompleteGate(run(gateCtx, s, p, g), end)
			cancel()
		}
		results = append(results, r)
	}
	final := s.Final(results)
	for i := range final.Trace {
		if err := p.Send(ctx, operator.Frame{Type: "trace", Session: s.ID, Trace: &final.Trace[i]}); err != nil {
			return err
		}
	}
	return p.Finish(ctx, func(fault error) operator.Frame {
		if fault != nil {
			s.Halt("protocol-error")
		}
		final := s.Final(results)
		return operator.Frame{Type: "final", Session: s.ID, Final: &final}
	})
}
