package operatorgates

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/operator"
)

type blockedExchangePeer struct {
	requests chan operator.Request
	release  chan struct{}
	request  operator.Request
}

func (p *blockedExchangePeer) Send(_ context.Context, f operator.Frame) error {
	p.request = *f.Request
	p.requests <- p.request
	return nil
}
func (p *blockedExchangePeer) Receive(ctx context.Context) (operator.Frame, error) {
	select {
	case <-ctx.Done():
		return operator.Frame{}, ctx.Err()
	case <-p.release:
		r := operator.Reply{ID: p.request.ID, Transport: operator.Succeeded, ExitCode: new(int)}
		return operator.Frame{Type: "result", Session: "session", Result: &r}, nil
	}
}
func (p *blockedExchangePeer) Poll() (operator.Frame, error, bool) {
	return operator.Frame{}, nil, false
}
func (p *blockedExchangePeer) Close() error { return nil }
func (p *blockedExchangePeer) Finish(context.Context, func(error) operator.Frame) error {
	return nil
}

func TestExchangeQueueHonorsDeadline(t *testing.T) {
	for _, mode := range []string{"context", "absolute-end"} {
		t.Run(mode, func(t *testing.T) {
			s, _ := fixtureSession(t)
			s.clock = NewRealClock()
			s.capabilities[operator.MailboxRead] = true
			p := &blockedExchangePeer{requests: make(chan operator.Request, 2), release: make(chan struct{})}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			first := make(chan struct{})
			go func() {
				defer close(first)
				_, _ = s.Exchange(ctx, p, operator.HostExecute, "shared", "status", nil, nil, time.Minute, func(operator.Reply) operator.Certainty { return operator.Succeeded })
			}()
			defer func() { close(p.release); <-first }()
			select {
			case <-p.requests:
			case <-ctx.Done():
				t.Fatal("first exchange did not start")
			}
			end := time.Minute
			wait := ctx
			if mode == "context" {
				var stop context.CancelFunc
				wait, stop = context.WithTimeout(ctx, 500*time.Millisecond)
				defer stop()
			} else {
				end = s.clock.Elapsed() + 500*time.Millisecond
			}
			done := make(chan error, 1)
			go func() {
				c, err := s.Exchange(wait, p, operator.MailboxRead, "caller", "mailbox.read", nil, nil, end, func(operator.Reply) operator.Certainty { return operator.Succeeded })
				if c != operator.Unknown {
					err = errors.New("expired waiter returned definite certainty")
				}
				done <- err
			}()
			select {
			case err := <-done:
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal("MUTATION: exchange-queue-deadline", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("MUTATION: exchange-queue-deadline waiter blocked behind first request")
			}
			select {
			case q := <-p.requests:
				t.Fatal("MUTATION: exchange-queue-deadline waiter submitted", q)
			default:
			}
		})
	}
}
