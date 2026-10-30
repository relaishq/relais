package agentbridge

import (
	"context"
	"errors"
	"time"

	"github.com/relaishq/relais/pkg/mediaworker"
)

// revocation is comparable so an acknowledgement must echo every argument.
type revocation struct {
	Type          string `json:"type"`
	SessionID     string `json:"session_id"`
	Bot           string `json:"bot_instance"`
	OldEpoch      uint64 `json:"old_epoch"`
	OldGeneration uint64 `json:"old_generation"`
	NewEpoch      uint64 `json:"new_epoch"`
	NewGeneration uint64 `json:"new_generation"`
	RequestID     string `json:"request_id"`
}

func (r *runner) receiveRevoked(ack revocation) {
	r.replyMu.Lock()
	defer r.replyMu.Unlock()
	ack.Type = "revoke"
	if r.revoking == nil || ack != *r.revoking {
		return
	}
	select {
	case r.revoked <- struct{}{}:
	default:
	}
}

func (r *runner) revoke(ctx context.Context, request revocation) error {
	// One revocation per runner; never hold replyMu while writing or waiting.
	select {
	case r.revokeSlot <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-r.done:
		return errProtocol
	}
	defer func() { <-r.revokeSlot }()
	request.Type = "revoke"
	r.replyMu.Lock()
	r.revoking = &request
	for len(r.revoked) > 0 {
		<-r.revoked
	}
	r.replyMu.Unlock()
	defer func() { r.replyMu.Lock(); r.revoking = nil; r.replyMu.Unlock() }()
	retry := time.NewTicker(250 * time.Millisecond)
	defer retry.Stop()
	for {
		if err := sendJSON(r.conn, request); err != nil {
			return err
		}
		select {
		case <-r.revoked:
			return nil
		case <-retry.C:
		case <-r.done:
			return errProtocol
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// ErrBotUnavailable means bot work remains paused. It never authorizes a
// replacement, a fresh conversation, or a fallback to Echo.
var ErrBotUnavailable = errors.New("bridge: bot unavailable")

// Revoke stops grants before contacting the runner. It is the permission gate
// for replacement; publishing/checkpointing a successor lineage is separate.
// A failed RPC can only complete this gate after the pool's full permit cap.
func (b *Bridge) Revoke(ctx context.Context, old mediaworker.BridgeOutputScope, epoch, generation uint64) (confirmed bool, err error) {
	if epoch < old.Epoch || (epoch == old.Epoch && generation <= old.Generation) || generation == 0 {
		return false, errProtocol
	}
	current, e := b.worker.BridgeOutputBinding(old.SessionID)
	if e != nil || current != old {
		return false, ErrBotUnavailable
	}
	localEpoch, e := b.worker.BridgeOutputEpoch(old.SessionID)
	if e != nil || localEpoch != epoch {
		return false, ErrBotUnavailable
	}
	until, e := b.worker.StopBridgePermits(old)
	if e != nil {
		return false, e
	}
	b.mu.Lock()
	for token, p := range b.pending {
		if p.scope.SessionID == old.SessionID {
			delete(b.pending, token)
		}
	}
	request := b.revocations[old.SessionID]
	candidate := revocation{Type: "revoke", SessionID: old.SessionID, Bot: old.BotInstance, OldEpoch: old.Epoch, OldGeneration: old.Generation, NewEpoch: epoch, NewGeneration: generation}
	if request != nil {
		candidate.RequestID = request.RequestID
		if candidate != *request {
			b.mu.Unlock()
			return false, errProtocol
		}
	} else {
		candidate.RequestID = nonce()
		b.revocations[old.SessionID] = &candidate
	}
	// Only the supervisor bound to this attachment can confirm its revocation.
	// A newly registered runner cannot acknowledge stopping an older process.
	var r *runner
	if active, ok := b.active[old.SessionID]; ok && active.scope == old {
		r = active.runner
	}
	b.mu.Unlock()
	rpcCtx, cancel := context.WithDeadline(ctx, until)
	defer cancel()
	if r != nil && r.bot == old.BotInstance && r.revoke(rpcCtx, candidate) == nil {
		return true, nil
	}
	timer := time.NewTimer(time.Until(until))
	defer timer.Stop()
	select {
	case <-timer.C:
		return false, nil
	case <-ctx.Done():
		return false, ErrBotUnavailable
	}
}
