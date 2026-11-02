package agentbridge

import (
	"time"

	"github.com/relaishq/relais/pkg/mediaworker"
)

type permitRequest struct {
	Type       string `json:"type"`
	SessionID  string `json:"session_id"`
	Epoch      uint64 `json:"epoch"`
	Generation uint64 `json:"generation"`
	Nonce      string `json:"nonce"`
}
type activeBot struct {
	runner *runner
	scope  mediaworker.BridgeOutputScope
	done   <-chan struct{}
}

func (b *Bridge) permit(r *runner, request permitRequest) error {
	if !identity(request.Nonce) || !identity(request.SessionID) || request.Epoch == 0 || request.Generation == 0 {
		return errProtocol
	}
	b.mu.Lock()
	active, ok := b.active[request.SessionID]
	b.mu.Unlock()
	remaining := time.Duration(0)
	if ok && active.runner == r && active.scope.BotInstance == r.bot && active.scope.Epoch == request.Epoch && active.scope.Generation == request.Generation {
		select {
		case <-active.done:
		default:
			remaining = b.worker.BridgePermitRemaining(active.scope)
		}
	}
	return sendJSON(r.conn, struct {
		permitRequest
		Remaining int64 `json:"remaining_ms"`
	}{permitRequest: permitRequest{Type: "permit", SessionID: request.SessionID, Epoch: request.Epoch, Generation: request.Generation, Nonce: request.Nonce}, Remaining: remaining.Milliseconds()})
}
