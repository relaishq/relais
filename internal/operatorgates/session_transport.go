package operatorgates

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
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
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err, ready := p.Poll()
	if !ready {
		return nil
	}
	code := "protocol-error"
	if err == nil && f.Type == "cancel" {
		code = "interrupted"
	}
	if code == "protocol-error" {
		id := "0"
		if f.Result != nil {
			id = f.Result.ID
		}
		s.unknownRequest(id, "malformed-reply")
	}
	s.halt(code)
	return errors.New(code)
}

// Exchange is the production serial request path used by gate implementations. A lost
// pipe never leads to a second submission. Timeout preserves pending identity.
func (s *Session) Exchange(ctx context.Context, p Peer, op operator.Operation, target, action string, args json.RawMessage, script []byte, end time.Duration, classify func(operator.Reply) operator.Certainty) (operator.Certainty, error) {
	s.mu.Lock()
	end = min(end, s.Budget.end)
	if s.activeEnd != nil {
		end = min(end, *s.activeEnd)
	}
	s.mu.Unlock()
	wait, cancel := context.WithTimeout(ctx, end-s.clock.Elapsed())
	defer cancel()
	// Queue time consumes this exchange's original budget. Expired waiters
	// never reserve an identity or submit behind a longer running request.
	select {
	case s.exchange <- struct{}{}:
		defer func() { <-s.exchange }()
	case <-wait.Done():
	}
	if wait.Err() != nil {
		s.RecordUncertainty(operator.Uncertainty{Kind: string(op), RequestID: "0", Target: target, Action: action, DeadlineUnixMS: strconv.FormatInt(s.clock.Wall().Add(end-s.clock.Elapsed()).UnixMilli(), 10), RemainingMS: (end - s.clock.Elapsed()).Milliseconds(), Rule: transportRule(wait.Err(), "cancelled")})
		s.Halt("interrupted")
		return operator.Unknown, wait.Err()
	}
	if err := s.CheckInbox(p); err != nil {
		return operator.Unknown, err
	}
	req, err := s.Issue(op, target, action, args, script, end)
	if err != nil {
		return operator.Unknown, err
	}
	// Queueing and preflight reads consume the original budget. Refuse locally
	// before writing a host request that cannot meet the submission floor.
	if op == operator.HostExecute && req.RemainingMS < operator.HostSubmissionMinimum.Milliseconds() {
		refusal := operator.Reply{ID: req.ID, Transport: operator.Failed, NotSubmitted: true, RefusalCode: "deadline-before-submission"}
		return s.Accept(s.ID, refusal, classify(refusal))
	}
	requestWait, stop := context.WithTimeout(wait, s.requests[req.ID].end-s.clock.Elapsed())
	defer stop()
	wait = requestWait
	if err = p.Send(wait, operator.Frame{Type: "request", Session: s.ID, Request: &req}); err != nil {
		s.noteUnknownRequest(req.ID, transportRule(err, "send-failed"))
		s.TransportLost()
		return operator.Unknown, err
	}
	for {
		f, err := p.Receive(wait)
		if err != nil {
			s.noteUnknownRequest(req.ID, transportRule(err, "lost-reply"))
			s.TransportLost()
			return operator.Unknown, err
		}
		if f.Type == "cancel" {
			s.noteUnknownRequest(req.ID, "cancelled")
			s.Halt("interrupted")
			return operator.Unknown, errors.New("interrupted")
		}
		if f.Type != "result" || f.Result == nil {
			s.noteUnknownRequest(req.ID, "malformed-reply")
			s.Halt("protocol-error")
			return operator.Unknown, errors.New("protocol-error")
		}
		resolved, err := operatortransport.ResolveReply(s.spool, *f.Result)
		if err != nil {
			s.noteUnknownRequest(req.ID, "invalid-blob")
			s.Halt("invalid-blob")
			return operator.Unknown, err
		}
		f.Result = &resolved
		evidence := operator.Unknown
		if f.Result.ID == req.ID && (f.Result.Transport == operator.Succeeded || f.Result.NotSubmitted && f.Result.Transport == operator.Failed) {
			evidence = classify(*f.Result)
		}
		if evidence == operator.Unknown && f.Result.Transport == operator.Succeeded && f.Result.ExitCode != nil && *f.Result.ExitCode == 0 && f.Result.UncertaintyRule == "" && !json.Valid(f.Result.Payload) {
			f.Result.UncertaintyRule = "malformed-reply"
		}
		certainty, err := s.Accept(f.Session, *f.Result, evidence)
		if err != nil || f.Result.ID == req.ID {
			return certainty, err
		}
	}
}

func transportRule(err error, fallback string) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	return fallback
}

// GateRunner remains the single-gate test seam.
type GateRunner func(context.Context, *Session, Peer, operator.Gate) operator.GateResult
type SequenceRunner func(context.Context, *Session, Peer) []operator.GateResult

func Serve(ctx context.Context, p Peer, hello operator.Hello, run GateRunner, spool ...*os.Root) error {
	return serveSession(ctx, p, hello, func(ctx context.Context, s *Session, p Peer) []operator.GateResult {
		results := []operator.GateResult{}
		for _, g := range s.gates {
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
		return results
	}, false, spool...)
}

// ServeSequence runs the complete public sequence after one init. Trace frames
// carry the full trace; final omits its duplicate copy to keep long sessions
// inside the unchanged 256 KiB control-frame limit.
func ServeSequence(ctx context.Context, p Peer, hello operator.Hello, run SequenceRunner, spool ...*os.Root) error {
	return serveSession(ctx, p, hello, run, true, spool...)
}
func serveSession(ctx context.Context, p Peer, hello operator.Hello, run SequenceRunner, streamTrace bool, spool ...*os.Root) error {
	defer p.Close()
	if err := p.Send(ctx, operator.Frame{Type: "hello", Hello: &hello}); err != nil {
		return err
	}
	frame, err := p.Receive(ctx)
	if err != nil {
		return err
	}
	if frame.Type != "init" || frame.Init == nil {
		return errors.New("expected-init")
	}
	s, err := NewSession(frame.Session, *frame.Init, NewRealClock())
	if err != nil {
		return err
	}
	s.SetUncertaintySink(func(u operator.Uncertainty) {
		raw, _ := json.Marshal(u)
		fmt.Fprintln(os.Stderr, "command-result-unknown", string(raw))
	})
	if len(spool) > 0 {
		s.SetBlobRoot(spool[0])
	}
	results := run(ctx, s, p)
	final := s.Final(results)
	streamed := len(final.Trace)
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
		if streamTrace {
			final.Trace = final.Trace[streamed:]
		}
		return operator.Frame{Type: "final", Session: s.ID, Final: &final}
	})
}
