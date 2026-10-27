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
	})
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
	return SessionStoreCapability{Session: s, Peer: p}
}
