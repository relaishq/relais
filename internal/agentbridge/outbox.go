package agentbridge

import (
	"context"
	"encoding/json"
	"slices"
	"time"

	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/mediaworker"
)

const outboxBytes = 11 << 10

type outboxRecord struct {
	ID      uint64      `json:"id"`
	Event   agent.Event `json:"event"`
	First   uint64      `json:"first,omitempty"`
	Last    uint64      `json:"last,omitempty"`
	Notices uint64      `json:"notices,omitempty"`
}
type outboxImage struct {
	Scope     mediaworker.BridgeOutputScope `json:"scope"`
	SessionID string                        `json:"session_id"`
	Revision  uint64                        `json:"revision"`
	Records   []outboxRecord                `json:"records,omitempty"`
	GapID     uint64                        `json:"gap_id,omitempty"`
	First     uint64                        `json:"first,omitempty"`
	Last      uint64                        `json:"last,omitempty"`
	NoticeID  uint64                        `json:"notice_id,omitempty"`
	Notices   uint64                        `json:"notices,omitempty"`
	encoded   []byte
}
type adapterState struct {
	checkpoint
	Outbox uint64 `json:"outbox"`
}
type adapterUpdate struct {
	Arrived    time.Time                     `json:"arrived,omitempty"`
	Scope      mediaworker.BridgeOutputScope `json:"scope"`
	Attached   bool                          `json:"attached,omitempty"`
	RunnerID   string                        `json:"runner_id,omitempty"`
	Checkpoint *checkpoint                   `json:"checkpoint,omitempty"`
	Delivered  uint64                        `json:"delivered,omitempty"`
}

// Build once in Handle, publish only after its Save commits. Save only reads
// the cached length. Neither writer progress nor a failed callback mutates it.
func (a *adapter) stageOutbox(e agent.Event, delivered uint64) error {
	next := *a.current
	next.Revision++
	next.Records = slices.Clone(next.Records)
	if delivered > 0 {
		next.Records = slices.DeleteFunc(next.Records, func(r outboxRecord) bool { return r.ID <= delivered })
		if next.GapID <= delivered {
			next.GapID = 0
			next.First = 0
			next.Last = 0
		}
		if next.NoticeID <= delivered {
			next.NoticeID = 0
			next.Notices = 0
		}
	} else {
		record := outboxRecord{ID: next.Revision, Event: e}
		if e.Input != nil {
			input := *e.Input
			input.Payload = slices.Clone(input.Payload)
			record.Event.Input = &input
		}
		if e.Control != nil {
			control := *e.Control
			control.Data = slices.Clone(control.Data)
			record.Event.Control = &control
		}
		candidate := next
		candidate.Records = append(slices.Clone(next.Records), record)
		data, err := json.Marshal(candidate)
		if err != nil {
			return err
		}
		if (e.Input == nil || len(e.Input.Payload) <= 1200) && len(candidate.Records) <= 32 && len(data) <= outboxBytes-512 && (e.Input == nil || next.GapID == 0) {
			next = candidate
		} else if e.Input != nil {
			// Re-reporting an overlapping gap is safe: it describes lost input, not an effect to replay.
			if next.GapID == 0 {
				next.First = e.Input.Progress.Index
			}
			next.GapID = next.Revision
			next.Last = e.Input.Progress.Index
		} else {
			next.NoticeID = next.Revision
			next.Notices++
		}
	}
	data, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if len(data) > outboxBytes {
		return agent.ErrCapacity
	}
	next.encoded = data
	a.current = &next
	return nil
}
func (a *adapter) nextOutbox(after uint64, scopes ...mediaworker.BridgeOutputScope) (outboxRecord, bool) {
	a.outboxMu.Lock()
	image := a.accepted
	a.outboxMu.Unlock()
	if len(scopes) > 0 && image.Scope != scopes[0] {
		return outboxRecord{}, false
	}
	var next outboxRecord
	for _, r := range image.Records {
		if r.ID > after {
			next = r
			break
		}
	}
	if image.GapID > after && (next.ID == 0 || image.GapID < next.ID) {
		next = outboxRecord{ID: image.GapID, First: image.First, Last: image.Last}
	}
	if image.NoticeID > after && (next.ID == 0 || image.NoticeID < next.ID) {
		next = outboxRecord{ID: image.NoticeID, Notices: image.Notices}
	}
	return next, next.ID > 0
}
func (a *adapter) SaveMoveOnly(context.Context) ([]byte, error) {
	return slices.Clone(a.current.encoded), nil
}
func (a *adapter) RestoreMoveOnly(_ context.Context, data []byte) error {
	if len(data) != a.restoreLength || len(data) > outboxBytes {
		return agent.ErrCapacity
	}
	var image outboxImage
	if err := json.Unmarshal(data, &image); err != nil {
		return err
	}
	if image.Scope != a.state.Scope || image.SessionID != a.id || image.Revision != a.current.Revision || len(image.Records) > 32 || image.GapID > image.Revision || image.NoticeID > image.Revision || (image.GapID > 0 && image.Last < image.First) {
		return errProtocol
	}
	var last uint64
	for _, r := range image.Records {
		if r.ID <= last || r.ID > image.Revision {
			return errProtocol
		}
		last = r.ID
		switch r.Event.Kind {
		case agent.AudioInput:
			if r.Event.Input == nil || len(r.Event.Input.Payload) > 1200 {
				return errProtocol
			}
		case agent.Control:
			if r.Event.Control == nil || r.Event.Control.Validate() != nil {
				return errProtocol
			}
		case agent.PlaybackFinished:
			if r.Event.Playback == nil {
				return errProtocol
			}
		default:
			return errProtocol
		}
	}
	image.encoded = slices.Clone(data)
	a.current = &image
	a.outboxMu.Lock()
	a.accepted = &image
	a.outboxMu.Unlock()
	return nil
}

// Scope remapping is local actor work. Only a continuing authorized lineage
// reaches this path; replacement creates an empty image instead.
func (a *adapter) bindOutboxScope(scope mediaworker.BridgeOutputScope) error {
	next := *a.current
	next.Scope = scope
	next.Revision++
	data, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if len(data) > outboxBytes {
		return agent.ErrCapacity
	}
	next.encoded = data
	a.current = &next
	return nil
}
