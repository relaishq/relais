package main

import (
	"time"

	"github.com/relais/pkg/controlplane"
	"github.com/relais/pkg/mediaworker"
)

// Explicit outer DTOs omit addresses. Events retain measurement data, but
// never transport error text or arbitrary string labels from the private API.
type publicStatus struct {
	Workers   []workerStatus `json:"workers"`
	Calls     []callStatus   `json:"calls"`
	Takeovers []eventStatus  `json:"takeovers"`
	LostCount uint64         `json:"lost_count"`
}
type workerStatus struct {
	Name          string    `json:"name"`
	Draining      bool      `json:"draining"`
	Recovering    bool      `json:"recovering"`
	Dead          bool      `json:"dead"`
	LastHeartbeat time.Time `json:"last_heartbeat"`
	Calls         int       `json:"call_count"`
}
type callStatus struct {
	ID            string     `json:"id"`
	Owner         string     `json:"owner"`
	Epoch         uint64     `json:"lease_epoch"`
	LastMove      *time.Time `json:"last_move"`
	MoveCount     uint64     `json:"move_count"`
	TakeoverCount uint64     `json:"takeover_count"`
	LastMoveKind  string     `json:"last_move_kind"`
}
type eventStatus struct {
	ID                 string           `json:"id"`
	Kind               string           `json:"kind"`
	From               string           `json:"from"`
	To                 string           `json:"to"`
	Start              time.Time        `json:"start"`
	End                time.Time        `json:"end"`
	DetectedAt         time.Time        `json:"detected_at,omitempty"`
	LastHeartbeat      time.Time        `json:"last_heartbeat,omitempty"`
	Lost               bool             `json:"lost,omitempty"`
	LossCause          string           `json:"loss_cause,omitempty"`
	Error              string           `json:"error,omitempty"`
	SnapshotAge        time.Duration    `json:"snapshot_age,omitempty"`
	CheckpointStoredAt time.Time        `json:"checkpoint_stored_at"`
	CheckpointAge      time.Duration    `json:"checkpoint_age,omitempty"`
	Checkpoint         checkpointStatus `json:"checkpoint"`
	CheckpointPolicy   string           `json:"checkpoint_policy,omitempty"`
	SequenceMargin     uint16           `json:"sequence_margin,omitempty"`
	SRTCPIndexMargin   uint32           `json:"srtcp_index_margin,omitempty"`
	Result             handoverStatus   `json:"result"`
}

func enum(value string, allowed ...string) string {
	if value == "" {
		return ""
	}
	for _, item := range allowed {
		if item == value {
			return value
		}
	}
	return "unknown"
}
func sanitizeStatus(in controlplane.Status) publicStatus {
	out := publicStatus{Workers: []workerStatus{}, Calls: []callStatus{}, Takeovers: []eventStatus{}, LostCount: in.LostCount}
	for _, w := range in.Workers {
		out.Workers = append(out.Workers, workerStatus{Name: workerID(w.Name), Draining: w.Draining, Recovering: w.Recovering, Dead: w.Dead, LastHeartbeat: w.LastHeartbeat, Calls: w.Calls})
	}
	for _, c := range in.Calls {
		out.Calls = append(out.Calls, callStatus{ID: c.ID, Owner: workerID(c.Owner), Epoch: c.Epoch, LastMove: c.LastMove, MoveCount: c.MoveCount, TakeoverCount: c.TakeoverCount, LastMoveKind: enum(c.LastMoveKind, "move", "takeover")})
	}
	for _, e := range in.Takeovers {
		result := sanitizeResult(e.ID, e.Result)
		event := eventStatus{
			ID: e.ID,
			Kind: enum(e.Kind,
				"move",
				"takeover"),
			From:          workerID(e.From),
			To:            workerID(e.To),
			Start:         e.Start,
			End:           e.End,
			DetectedAt:    e.DetectedAt,
			LastHeartbeat: e.LastHeartbeat,
			Lost:          e.Lost,
			LossCause: enum(e.LossCause,
				"ownership_expired",
				"recovery_failed",
				"checkpoint_missing",
				"checkpoint_unsafe"),
			SnapshotAge:        e.SnapshotAge,
			CheckpointStoredAt: e.CheckpointStoredAt,
			CheckpointAge:      e.CheckpointAge,
			Checkpoint: checkpointStatus{
				CapturedAt:          e.Checkpoint.CapturedAt,
				AgeAtCapture:        e.Checkpoint.AgeAtCapture,
				Attempts:            e.Checkpoint.Attempts,
				Successes:           e.Checkpoint.Successes,
				Failures:            e.Checkpoint.Failures,
				RTPPacketRate:       e.Checkpoint.RTPPacketRate,
				SRTCPPacketRate:     e.Checkpoint.SRTCPPacketRate,
				TakeoverAge:         e.Checkpoint.TakeoverAge,
				TakeoverSnapshotAge: e.Checkpoint.TakeoverSnapshotAge,
				TakeoverStoredAt:    e.Checkpoint.TakeoverStoredAt,
			},
			CheckpointPolicy: enum(e.CheckpointPolicy,
				"scaled",
				"definitive-loss"),
			SequenceMargin:   e.SequenceMargin,
			SRTCPIndexMargin: e.SRTCPIndexMargin,
			Result:           result,
		}
		if e.Error != "" {
			event.Error = "operation failed"
		}
		out.Takeovers = append(out.Takeovers, event)
	}
	return out
}

// The nested measurement views are also allowlists. A future field added to
// the private result structs must not silently widen the public API.
type checkpointStatus struct {
	CapturedAt          time.Time     `json:"captured_at"`
	AgeAtCapture        time.Duration `json:"age_at_capture"`
	Attempts            uint64        `json:"attempts"`
	Successes           uint64        `json:"successes"`
	Failures            uint64        `json:"failures"`
	RTPPacketRate       float64       `json:"rtp_packet_rate"`
	SRTCPPacketRate     float64       `json:"srtcp_packet_rate"`
	TakeoverAge         time.Duration `json:"takeover_age"`
	TakeoverSnapshotAge time.Duration `json:"takeover_snapshot_age"`
	TakeoverStoredAt    time.Time     `json:"takeover_stored_at"`
}
type duplicateWindow struct {
	SSRC  uint32
	First uint64
	Last  uint64
}
type handoverStatus struct {
	DuplicateWindows        []duplicateWindow
	InputMayBeDuplicated    bool
	InputDuplicationWindow  time.Duration
	RelayReplayPackets      int
	RelayReplayDuration     time.Duration
	RelayReplayDrops        int
	RelayReplaySendFailures int
	RelayReplayRecoveryPLI  bool
	RelayReplayComplete     bool
	SessionID               string
	StateBytes              int
	HeldPackets             int
	HoldExpired             bool
	RolledBack              bool
	Duration                time.Duration
	Drain                   time.Duration
	Export                  time.Duration
	Resume                  time.Duration
}

func sanitizeResult(id string, r mediaworker.HandoverResult) handoverStatus {
	out := handoverStatus{
		InputMayBeDuplicated:    r.InputMayBeDuplicated,
		InputDuplicationWindow:  r.InputDuplicationWindow,
		RelayReplayPackets:      r.RelayReplayPackets,
		RelayReplayDuration:     r.RelayReplayDuration,
		RelayReplayDrops:        r.RelayReplayDrops,
		RelayReplaySendFailures: r.RelayReplaySendFailures,
		RelayReplayRecoveryPLI:  r.RelayReplayRecoveryPLI,
		RelayReplayComplete:     r.RelayReplayComplete,
		SessionID:               id,
		StateBytes:              r.StateBytes,
		HeldPackets:             r.HeldPackets,
		HoldExpired:             r.HoldExpired,
		RolledBack:              r.RolledBack,
		Duration:                r.Duration,
		Drain:                   r.Drain,
		Export:                  r.Export,
		Resume:                  r.Resume,
	}
	for _, w := range r.DuplicateWindows {
		out.DuplicateWindows = append(out.DuplicateWindows, duplicateWindow{SSRC: w.SSRC, First: w.First, Last: w.Last})
	}
	return out
}
