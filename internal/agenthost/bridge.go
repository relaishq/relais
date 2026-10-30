package agenthost

import (
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
