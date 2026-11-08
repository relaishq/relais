package operatorgates

import (
	"context"
	"time"

	"github.com/relaishq/relais/internal/operatorgates/observe"
	"github.com/relaishq/relais/pkg/operator"
)

// SessionStoreCapability binds optional resource actions to the same pending
// table and fatal latch as host operations. No retry or resource SDK lives here.
type SessionStoreCapability struct {
	Session *Session
	Peer    Peer
	Guard   *LeaseGuard
}

func (s SessionStoreCapability) Observe(ctx context.Context, end time.Duration) (StoreObservation, error) {
	var observation StoreObservation
	certainty, err := s.Session.Exchange(ctx, s.Peer, operator.StoreObserve, "store", "observe", nil, nil, end, func(reply operator.Reply) operator.Certainty {
		if observe.Decode(reply.Payload, &observation, observe.Contract{"primary", "available", "durability"}) != nil {
			return operator.Unknown
		}
		return operator.Succeeded
	})
	if err != nil || certainty == operator.Unknown {
		return observation, fault("command-result-unknown")
	}
	if certainty != operator.Succeeded {
		return observation, fault("precondition-failed")
	}
	return observation, nil
}
func (s SessionStoreCapability) Failover(ctx context.Context, end time.Duration) error {
	if s.Session.clockBinding != nil {
		return s.FailoverWithLeases(ctx, end, s.Guard)
	}
	return s.failover(ctx, end, nil)
}

func (s SessionStoreCapability) FailoverWithLeases(ctx context.Context, end time.Duration, guard *LeaseGuard) error {
	if guard == nil {
		return fault("clock-lease-missing")
	}
	if err := guard.Refresh(ctx, end); err != nil {
		return s.Session.preflightFailure(err, "clocks")
	}
	if err := guard.Check(); err != nil {
		return err
	}
	return s.failover(ctx, end, []ExchangeHooks{{BeforeSend: guard.Check}})
}

func (s SessionStoreCapability) failover(ctx context.Context, end time.Duration, hooks []ExchangeHooks) error {
	certainty, err := s.Session.Exchange(ctx, s.Peer, operator.StoreFailover, "store", "failover", nil, nil, end, func(reply operator.Reply) operator.Certainty {
		if reply.NotSubmitted {
			return operator.Failed
		}
		var proof struct {
			Completed bool `json:"completed"`
		}
		if observe.Decode(reply.Payload, &proof, observe.Contract{"completed"}) != nil || !proof.Completed {
			return operator.Unknown
		}
		return operator.Succeeded
	}, hooks...)
	if len(hooks) > 0 && certainty == operator.Failed && isLeaseRefusal(err) && !s.Session.Latched() {
		return err
	}
	if err != nil || certainty == operator.Unknown {
		return fault("command-result-unknown")
	}
	if certainty != operator.Succeeded {
		return fault("precondition-failed")
	}
	return nil
}

// OptionalStore returns nil unless both operations were negotiated. Callers
// then report unsupported-capability without submitting a partial test.
func (s *Session) OptionalStore(p Peer) StoreCapability {
	if !s.capabilities[operator.StoreObserve] || !s.capabilities[operator.StoreFailover] {
		return nil
	}
	return SessionStoreCapability{Session: s, Peer: p, Guard: s.liveLeaseGuard}
}
