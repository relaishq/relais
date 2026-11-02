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
	if e := b.worker.CheckpointBridgeStop(ctx, old); e != nil {
		return false, ErrBotUnavailable
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
	trusted := false
	if active, ok := b.active[old.SessionID]; ok && active.scope == old {
		r = active.runner
		trusted = true
	}
	if r == nil && b.runner != nil && b.runner.bot == old.BotInstance {
		r = b.runner
	}
	b.mu.Unlock()
	rpcCtx, cancel := context.WithDeadline(ctx, until)
	defer cancel()
	if r != nil && r.bot == old.BotInstance {
		// A successor registry can learn the floor, but only the exact attachment
		// supervisor may shorten the wait. A reused identity is not stop evidence.
		if r.revoke(rpcCtx, candidate) == nil && trusted {
			return true, nil
		}
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

// replacement retains the owner callback identity across registry reconnects.
// An empty next scope means old must still pass the revocation gate.
type replacement struct {
	old, next mediaworker.BridgeOutputScope
	runner    string
}

func (b *Bridge) clearRevocationLocked(old mediaworker.BridgeOutputScope) {
	if request := b.revocations[old.SessionID]; request != nil && request.OldEpoch == old.Epoch && request.OldGeneration == old.Generation && request.Bot == old.BotInstance {
		delete(b.revocations, old.SessionID)
	}
}

// Restart restores the settled conversation and authorizes one successor. An
// uncertain attempt keeps its identity until settlement, even if its bot exits.
func (b *Bridge) Restart(ctx context.Context, old mediaworker.BridgeOutputScope) (mediaworker.BridgeOutputScope, error) {
	b.mu.Lock()
	attempt, retry := b.successors[old.SessionID]
	r := b.runner
	b.mu.Unlock()
	if retry {
		old = attempt.old
		next := attempt.next
		if next.SessionID != "" {
			if current, e := b.worker.BridgeOutputBinding(old.SessionID); e == nil && current == next {
				if r == nil {
					return next, ErrBotUnavailable
				}
				if e = b.worker.RestartBridge(ctx, old, next, attempt.runner); e != nil {
					return next, e
				}
				b.mu.Lock()
				b.clearRevocationLocked(old)
				if r.bot == next.BotInstance {
					delete(b.successors, old.SessionID)
					b.mu.Unlock()
					return next, nil
				}
				// Settle the previous lineage before revoking it. Retain this old scope
				// if the new revocation wait or stop checkpoint is itself interrupted.
				attempt = replacement{old: next}
				b.successors[old.SessionID] = attempt
				b.mu.Unlock()
				old = next
			} else if r != nil && r.bot != next.BotInstance {
				// No local successor was published; retain the stopped old generation.
				b.mu.Lock()
				attempt.next = mediaworker.BridgeOutputScope{}
				b.successors[old.SessionID] = attempt
				b.mu.Unlock()
			}
		}
	}
	epoch, err := b.worker.BridgeOutputEpoch(old.SessionID)
	if err != nil {
		return mediaworker.BridgeOutputScope{}, err
	}
	if old.Generation == ^uint64(0) {
		return mediaworker.BridgeOutputScope{}, errProtocol
	}
	if _, err = b.Revoke(ctx, old, epoch, old.Generation+1); err != nil {
		return mediaworker.BridgeOutputScope{}, err
	}
	b.mu.Lock()
	r = b.runner
	if r == nil {
		b.mu.Unlock()
		return mediaworker.BridgeOutputScope{}, ErrBotUnavailable
	}
	attempt, ok := b.successors[old.SessionID]
	if !ok || attempt.next.SessionID == "" {
		attempt = replacement{old: old, next: mediaworker.BridgeOutputScope{SessionID: old.SessionID, BotInstance: r.bot, Epoch: epoch, Generation: old.Generation + 1, LineageID: nonce()}, runner: r.id}
		b.successors[old.SessionID] = attempt
	}
	b.mu.Unlock()
	if err = b.worker.RestartBridge(ctx, attempt.old, attempt.next, attempt.runner); err != nil {
		return attempt.next, err
	}
	b.mu.Lock()
	b.clearRevocationLocked(attempt.old)
	delete(b.successors, old.SessionID)
	b.mu.Unlock()
	return attempt.next, nil
}
