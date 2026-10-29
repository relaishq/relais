package operatorgates

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/relaishq/relais/internal/operatorcaller"
	"github.com/relaishq/relais/internal/operatorgates/observe"
	"github.com/relaishq/relais/internal/operatortransport"
	"github.com/relaishq/relais/pkg/operator"
)

// ResourcePorts shares the session's request IDs, deadlines and fatal latch.
type ResourcePorts struct {
	Session  *Session
	Peer     Peer
	End      time.Duration
	Manifest operatorcaller.Manifest
	start    *operatorcaller.StartResult
	stop     *operatorcaller.StopResult
}
type CallerRequest struct {
	Manifest operatorcaller.Manifest `json:"manifest"`
}
type CallerProof struct {
	Ready    bool                      `json:"ready"`
	Start    operatorcaller.StartState `json:"start,omitempty"`
	Inactive bool                      `json:"inactive"`
}

func (p *ResourcePorts) exchange(ctx context.Context, op operator.Operation, args any, end time.Duration, classify func(operator.Reply) operator.Certainty) operator.Certainty {
	raw, err := json.Marshal(args)
	if err != nil {
		return operator.Unknown
	}
	if deadline, ok := ctx.Deadline(); ok {
		end = min(end, p.Session.clock.Elapsed()+max(0, time.Until(deadline)))
	}
	c, err := p.Session.Exchange(ctx, p.Peer, op, "caller", string(op), raw, nil, min(end, p.End), classify)
	if err != nil {
		if err.Error() == "unsupported-capability" {
			return operator.Failed
		}
		return operator.Unknown
	}
	return c
}
func (p *ResourcePorts) elapsed(end time.Time) time.Duration {
	return p.Session.clock.Elapsed() + max(0, end.Sub(p.Session.clock.Wall()))
}
func (p *ResourcePorts) Ready(ctx context.Context, end time.Time) operator.Certainty {
	return p.exchange(ctx, operator.CallerReady, CallerRequest{p.Manifest}, p.elapsed(end), func(r operator.Reply) operator.Certainty {
		if r.NotSubmitted {
			return operator.Failed
		}
		var proof CallerProof
		if observe.Decode(r.Payload, &proof, observe.Contract{"ready"}) != nil {
			return operator.Unknown
		}
		if !proof.Ready {
			return operator.Failed
		}
		return operator.Succeeded
	})
}
func (p *ResourcePorts) Start(ctx context.Context, id operatorcaller.Identity, phase operatorcaller.Phase, end time.Time) operatorcaller.StartResult {
	if p.start != nil {
		return *p.start
	}
	r := operatorcaller.StartResult{State: operatorcaller.NotStarted, Certainty: operator.Failed, Unit: id.Unit()}
	if id != p.Manifest.Identity || phase != id.Phase {
		return r
	}
	// Reserve before delivery; an unknown start is never submitted again.
	p.start = &operatorcaller.StartResult{State: operatorcaller.StartUnknown, Certainty: operator.Unknown, Unit: id.Unit()}
	c := p.exchange(ctx, operator.CallerStart, CallerRequest{p.Manifest}, p.elapsed(end), func(r operator.Reply) operator.Certainty {
		if r.NotSubmitted {
			return operator.Failed
		}
		var proof CallerProof
		if observe.Decode(r.Payload, &proof, observe.Contract{"start"}) != nil || proof.Start != operatorcaller.Started {
			return operator.Unknown
		}
		return operator.Succeeded
	})
	r.Certainty = c
	switch c {
	case operator.Succeeded:
		r.State = operatorcaller.Started
	case operator.Unknown:
		r.State = operatorcaller.StartUnknown
	}
	p.start = &r
	return r
}
func (p *ResourcePorts) NeverStarted() bool {
	return p.start == nil || p.start.State == operatorcaller.NotStarted && p.start.Certainty == operator.Failed
}
func (p *ResourcePorts) Stop(ctx context.Context, id operatorcaller.Identity, end time.Time) operatorcaller.StopResult {
	if p.stop != nil {
		return *p.stop
	}
	r := operatorcaller.StopResult{Unit: id.Unit(), Certainty: operator.Unknown}
	p.stop = &r
	r.Certainty = p.exchange(ctx, operator.CallerStop, CallerRequest{p.Manifest}, p.elapsed(end), func(reply operator.Reply) operator.Certainty {
		if reply.NotSubmitted {
			return operator.Failed
		}
		var proof CallerProof
		if observe.Decode(reply.Payload, &proof, observe.Contract{"inactive"}) != nil || !proof.Inactive {
			return operator.Unknown
		}
		r.Inactive = true
		return operator.Succeeded
	})
	p.stop = &r
	return r
}
func (p *ResourcePorts) Read(ctx context.Context, key string) (operatorcaller.ReadResult, error) {
	var value operatorcaller.MailboxValue
	c := p.exchange(ctx, operator.MailboxRead, operatorcaller.MailboxArgs{Key: key}, p.End, func(r operator.Reply) operator.Certainty {
		if r.NotSubmitted {
			return operator.Failed
		}
		if observe.DecodeLimit(r.Payload, &value, observe.Contract{"found"}, operator.MaxBlobBytes) != nil || value.Found == nil || !*value.Found && (len(value.Data) > 0 || value.Blob != nil) || value.Blob != nil && len(value.Data) > 0 {
			return operator.Unknown
		}
		if value.Blob != nil {
			if p.Session.spool == nil {
				return operator.Unknown
			}
			var err error
			value.Data, err = operatortransport.ReadBlob(p.Session.spool, *value.Blob, operator.MaxBlobBytes)
			if err != nil {
				return operator.Unknown
			}
		}
		return operator.Succeeded
	})
	if c != operator.Succeeded {
		return operatorcaller.ReadResult{}, errors.New("mailbox-result-unknown")
	}
	return operatorcaller.ReadResult{Found: *value.Found, Data: value.Data}, nil
}
func (p *ResourcePorts) Create(ctx context.Context, key string, data []byte) operatorcaller.CreateState {
	state := operatorcaller.CreateUnknown
	p.exchange(ctx, operator.MailboxCreate, operatorcaller.MailboxArgs{Key: key, Data: data}, p.End, func(r operator.Reply) operator.Certainty {
		if r.NotSubmitted {
			return operator.Failed
		}
		var value operatorcaller.MailboxValue
		if observe.Decode(r.Payload, &value, observe.Contract{"state"}) != nil {
			return operator.Unknown
		}
		if value.State != operatorcaller.Committed && value.State != operatorcaller.Conflict {
			return operator.Unknown
		}
		state = value.State
		return operator.Succeeded
	})
	if p.Session.Latched() {
		return operatorcaller.CreateUnknown
	}
	return state
}
