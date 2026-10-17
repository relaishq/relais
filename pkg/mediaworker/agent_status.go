package mediaworker

import (
	"encoding/json"
	"time"

	legacy "github.com/relaishq/relais/pkg/agent"
	agent "github.com/relaishq/relais/pkg/agent/v2"
)

// AgentSessionStatus is opt-in private diagnostic metadata. Reading it never
// calls the app, serializes move-only audio, or starts store I/O.
type AgentSessionStatus struct {
	// PendingBytes includes actual host move reservations and bookkeeping, not wire bytes.
	PendingBytes    int64             `json:"pending_bytes"`
	ID              string            `json:"id"`
	Name            string            `json:"name"`
	State           legacy.State      `json:"state"`
	App             json.RawMessage   `json:"app,omitempty"`
	Progress        legacy.Progress   `json:"progress"`
	RecordingBytes  int               `json:"recording_bytes"`
	RequestBytes    int               `json:"request_bytes"`
	PlaybackBytes   int               `json:"playback_bytes"`
	PlaybackPackets int               `json:"playback_packets"`
	Tasks           []AgentTaskStatus `json:"tasks,omitempty"`
	CheckpointKnown bool              `json:"checkpoint_known"`
	CheckpointAgeMS float64           `json:"checkpoint_age_ms"`
	SnapshotAgeMS   float64           `json:"snapshot_age_ms"`
}
type AgentTaskStatus struct {
	ID        agent.TaskID `json:"id"`
	Turn      agent.Turn   `json:"turn"`
	Phase     string       `json:"phase"`
	Cursor    uint64       `json:"cursor"`
	Published uint64       `json:"published"`
}

func (w *Worker) agentSessionStatus(id string) (*AgentSessionStatus, error) {
	s := w.session(id)
	if s == nil {
		return nil, ErrUnknownSession
	}
	s.moveMu.Lock()
	pendingBytes := s.moveReserved
	s.moveMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	pair := s.state.Agent
	status := &AgentSessionStatus{PendingBytes: pendingBytes, ID: id, State: legacy.State{Version: pair.State.Version, Bytes: append([]byte(nil), pair.State.Bytes...)}, Progress: pair.Progress, CheckpointKnown: s.snapshotStored.Load()}
	if json.Valid(pair.State.Bytes) {
		status.App = append(json.RawMessage(nil), pair.State.Bytes...)
	}
	if !s.lastCheckpoint.IsZero() {
		status.CheckpointAgeMS = float64(max(0, time.Since(s.lastCheckpoint))) / float64(time.Millisecond)
	}
	// CapturedAt and storeClock share the store's wall clock. Extrapolate with
	// local elapsed time and clamp if the store clock stepped backwards.
	if !s.state.Checkpoint.CapturedAt.IsZero() && !s.storeClockAt.IsZero() {
		now := s.storeClock.Add(time.Since(s.storeClockAt))
		status.SnapshotAgeMS = float64(max(0, now.Sub(s.state.Checkpoint.CapturedAt))) / float64(time.Millisecond)
	}
	if v2 := pair.V2; v2 != nil {
		status.Name = v2.Name
		status.RecordingBytes = v2.MoveOnlyLength
		status.PlaybackBytes = v2.Playback.Length
		status.PlaybackPackets = v2.Playback.Pending
		for _, task := range v2.Tasks {
			status.RequestBytes += task.RequestLength
			status.Tasks = append(status.Tasks, AgentTaskStatus{ID: task.ID, Turn: task.Turn, Phase: task.Phase, Cursor: task.Cursor, Published: task.Published})
		}
	}
	return status, nil
}
