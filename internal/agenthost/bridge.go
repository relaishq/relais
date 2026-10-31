package agenthost

import (
	"context"

	legacy "github.com/relaishq/relais/pkg/agent"
	agent "github.com/relaishq/relais/pkg/agent/v2"
)

// BridgeHooks is an internal, nonblocking notification seam. Implementations
// enqueue owned records only; callbacks never perform socket I/O. The host calls
// these after publication and outside its locks. No other agent opts into it.
type BridgeHooks interface {
	Accepted()
	Checkpointed(BridgeCheckpoint)
	Closed()
}

// BridgeCheckpoint describes the captured pair, never a later live revision.
// Output contains an owned opaque host receipt image from the same capture.
type BridgeCheckpoint struct {
	State    agent.State
	Progress legacy.Progress
	Sequence uint64
	Output   []byte
}

// BridgeScope identifies one authorized assignment. Epoch alone is not lineage.
type BridgeScope struct {
	SessionID   string `json:"session_id"`
	BotInstance string `json:"bot_instance"`
	Epoch       uint64 `json:"epoch"`
	Generation  uint64 `json:"generation"`
	LineageID   string `json:"lineage_id"`
}

// BridgeRestarter runs only on the actor, using a settled durable conversation.
// Like Restore/Save, it must use owned local state and never wait on transport.
type BridgeRestarter interface {
	Restart(context.Context, agent.State, BridgeScope, string) (agent.State, error)
}

// BridgeDeferral reports bounded candidate pressure without closing its channel.
type BridgeDeferral interface{ Deferred(agent.TaskResult) }
