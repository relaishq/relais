package controlplane

import (
	"context"
	"slices"
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
	if err := message.Validate(); err != nil {
		return agent.ControlReceipt{}, err
	}
	message.Data = slices.Clone(message.Data)
	ctx, cancel := context.WithTimeout(ctx, privateapi.Timeout)
	defer cancel()
	c, err := p.lookup(id)
	if err != nil {
		return agent.ControlReceipt{}, err
	}
	if err := c.lockControl(ctx); err != nil {
		return agent.ControlReceipt{}, err
	}
	defer c.unlockControl()
	// Leave most of the takeover budget available for recovery. Transport may
	// time out after acceptance; the same message can safely be retried.
	ctx, holdCancel := context.WithTimeout(ctx, takeoverBudget/3)
	defer holdCancel()
	c.controlMu.Lock()
	c.controlCancel = holdCancel
	c.controlMu.Unlock()
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

func (c *call) lockControl(ctx context.Context) error {
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		c.controlMu.Lock()
		if c.controlWaiters == 0 && c.mu.TryLock() {
			c.controlActive = true
			c.controlMu.Unlock()
			return nil
		}
		c.controlMu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
func (c *call) unlockControl() {
	c.controlMu.Lock()
	c.controlActive, c.controlCancel = false, nil
	c.mu.Unlock()
	c.controlMu.Unlock()
}

// Waiting operators exclude new Control holders. Continuous client retries
// cannot starve a move/drain. Takeover also cancels a current Control RPC and
// bounds lock acquisition by its existing end-to-end recovery budget.
func (c *call) lockOperator(ctx context.Context, takeover bool) error {
	ctx, cancel := context.WithTimeout(ctx, takeoverBudget)
	defer cancel()
	c.controlMu.Lock()
	if c.mu.TryLock() {
		c.controlMu.Unlock()
		return nil
	}
	if !c.controlActive && !takeover {
		c.controlMu.Unlock()
		return ErrMoveInProgress
	}
	c.controlWaiters++
	if takeover && c.controlCancel != nil {
		c.controlCancel()
	}
	c.controlMu.Unlock()
	defer func() { c.controlMu.Lock(); c.controlWaiters--; c.controlMu.Unlock() }()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
		c.controlMu.Lock()
		locked := c.mu.TryLock()
		otherOperator := !c.controlActive
		c.controlMu.Unlock()
		if locked {
			return nil
		}
		if otherOperator && !takeover {
			return ErrMoveInProgress
		}
	}
}
