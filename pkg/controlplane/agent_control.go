package controlplane

import (
	"context"
	"time"

	"github.com/relaishq/relais/internal/privateapi"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/sessionstore"
)

// ControlSender is optional: legacy/fake workers keep the existing Worker API.
// AgentControl must retain typed uncertain outcomes and never retry internally.
type ControlSender interface {
	AgentControl(context.Context, string, agent.ControlMessage) (agent.ControlReceipt, error)
}

// AgentControl resolves authoritative ownership under the same per-call lock
// used by moves, takeovers and hangup. Client input never supplies a target.
func (p *Plane) AgentControl(ctx context.Context, id string, message agent.ControlMessage) (agent.ControlReceipt, error) {
	ctx, cancel := context.WithTimeout(ctx, privateapi.Timeout)
	defer cancel()
	c, err := p.lookup(id)
	if err != nil {
		return agent.ControlReceipt{}, err
	}
	// A move can span remote I/O. Lock acquisition must honor the request budget.
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for !c.mu.TryLock() {
		select {
		case <-ctx.Done():
			return agent.ControlReceipt{}, ctx.Err()
		case <-ticker.C:
		}
	}
	defer c.mu.Unlock()
	if c.terminal || c.hungUp.Load() {
		return agent.ControlReceipt{}, sessionstore.ErrEnded
	}
	lease, err := p.store.Get(ctx, id)
	if err != nil {
		return agent.ControlReceipt{}, err
	}
	p.mu.Lock()
	owner := p.owner(lease.Worker)
	var sender ControlSender
	unavailable := owner != nil && (owner.dead || owner.recovering)
	if owner != nil && !owner.dead && !owner.recovering {
		sender, _ = owner.worker.(ControlSender)
	}
	p.mu.Unlock()
	if owner == nil {
		return agent.ControlReceipt{}, ErrUnknownCall
	}
	if unavailable {
		return agent.ControlReceipt{}, sessionstore.ErrLeaseLost
	}
	if sender == nil {
		return agent.ControlReceipt{}, agent.ErrControlUnsupported
	}
	return sender.AgentControl(ctx, id, message)
}
