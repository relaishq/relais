package agentbridge

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"time"

	"github.com/relaishq/relais/internal/taskrun"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/mediaworker"
	"golang.org/x/net/websocket"
)

// Config intentionally supports only literal loopback addresses. No DNS,
// redirects, proxies or remote development tokens are accepted.
type Config struct{ URL, Token string }

func (c Config) Validate() error {
	u, err := url.Parse(c.URL)
	if err != nil {
		return errProtocol
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "ws" || ip == nil || !ip.IsLoopback() || u.Port() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "/bridge" || len(c.Token) < 16 || len(c.Token) > 128 {
		return fmt.Errorf("bridge: use ws://<literal-loopback>:<port>/bridge and a 16–128 byte development token")
	}
	for _, r := range c.Token {
		if r < 33 || r > 126 {
			return errProtocol
		}
	}
	return nil
}

func (b *Bridge) run(parent context.Context, run *agent.TaskRun) error {
	value, ok := b.agents.Load(string(run.Spec().Request))
	if !ok || b.worker == nil {
		return errProtocol
	}
	a := value.(*adapter)
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	attachCtx, stopAttach := context.WithTimeout(ctx, attachBudget)
	attachment, scope, resume, err := b.connection(attachCtx, run, a)
	stopAttach()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &taskrun.Failure{Outcome: agent.Capacity, Detail: "no_capacity_silence"}
	}
	conn := attachment.conn
	defer close(attachment.done)
	defer attachment.release()
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	state, err := b.worker.BridgeOutputResume(scope)
	if err != nil {
		return err
	}
	local := a.attachmentState.Load()
	if local == nil {
		return errProtocol
	}

	if err = sendJSON(conn, map[string]any{
		"type": "attached", "protocol": 1, "scope": scope, "epoch": scope.Epoch, "generation": scope.Generation, "lineage_id": scope.LineageID,
		"resume": resume, "next_output_seq": state.NextSequence, "checkpoint_seq": local.Sequence, "working": local.Working, "history": local.History,
		"checkpoint_age_ms": nil, "snapshot_age_ms": nil, "outcome_unknown": false, "duplicate_windows": []any{},
		"output_receipts": state, "provider_starts": false,
		"limits": map[string]int{"input_packets": 32, "json_bytes": 32768, "binary_bytes": 1200, "working_bytes": a.maxWorking - adapterMetadataBytes, "history_bytes": a.maxHistory},
	}); err != nil {
		return err
	}

	_ = conn.SetDeadline(time.Time{})
	replies := make(chan any, 1)
	creditTicker := time.NewTicker(20 * time.Millisecond)
	defer creditTicker.Stop()
	writerDone := make(chan error, 1)
	go func() { err := b.write(ctx, conn, a, scope, replies, creditTicker.C); writerDone <- err; cancel() }()
	defer func() { cancel(); _ = conn.Close(); <-writerDone }()
	candidates := make(chan []byte, 1)
	emitterDone := make(chan error, 1)
	go func() { err := emitUpdates(ctx, run, a, candidates); emitterDone <- err; cancel() }()
	defer func() { cancel(); <-emitterDone }()
	for {
		var raw []byte
		if err = websocket.Message.Receive(conn, &raw); err != nil {
			return err
		}
		var reply any
		if len(raw) > 0 && raw[0] == 2 {
			if len(raw) < 18 || len(raw) > 17+1200 {
				return errProtocol
			}
			turn, seq := agent.Turn(binary.BigEndian.Uint64(raw[1:9])), binary.BigEndian.Uint64(raw[9:17])
			receipt, e := b.worker.SubmitBridgeOutput(ctx, scope, turn, seq, raw[17:])
			if e != nil {
				return e
			}
			reply = typed("output_receipt", receipt)
		} else {
			var m struct {
				Type      string     `json:"type"`
				RequestID string     `json:"request_id"`
				Turn      agent.Turn `json:"turn"`
				Sequence  uint64     `json:"seq"`
				Last      uint64     `json:"last_seq"`
			}
			if err = json.Unmarshal(raw, &m); err != nil {
				return errProtocol
			}
			switch m.Type {
			case "output_begin":
				stream, e := b.worker.BeginBridgeOutput(scope, m.Turn)
				if e != nil {
					return e
				}
				reply = map[string]any{"type": "output_begun", "request_id": m.RequestID, "turn": m.Turn, "stream_id": stream}
			case "audio_end":
				if err = b.worker.EndBridgeOutput(scope, m.Turn, m.Last); err != nil {
					return err
				}
			case "output_status":
				status, e := b.worker.BridgeOutputStatus(scope, m.Sequence)
				if e != nil {
					return e
				}
				reply = typed("output_status_reply", status)
			case "checkpoint":
				if e := a.candidate(raw); e != nil {
					reply = map[string]any{"type": "refused", "seq": m.Sequence, "reason": "state_bounds"}
					break
				}
				select {
				case candidates <- raw:
				case <-ctx.Done():
					return ctx.Err()
				}
			case "replay_horizon":
				if err = b.worker.AdvanceBridgeReplayHorizon(scope, m.Sequence); err != nil {
					return err
				}
			case "pong":
			default:
				return errProtocol
			}
		}
		if reply != nil {
			select {
			case replies <- reply:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}
func typed(kind string, value any) any {
	return wireMessage{kind, value}
}

type wireMessage struct {
	kind  string
	value any
}

func (m wireMessage) MarshalJSON() ([]byte, error) {
	data, err := json.Marshal(m.value)
	if err != nil {
		return nil, err
	}
	kind, err := json.Marshal(m.kind)
	if err != nil {
		return nil, err
	}
	return append(append(append(data[:len(data)-1], []byte(`,"type":`)...), kind...), '}'), nil
}

func sendJSON(conn *websocket.Conn, value any) error {
	_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
	return websocket.JSON.Send(conn, value)
}

func (b *Bridge) write(ctx context.Context, conn *websocket.Conn, a *adapter, scope mediaworker.BridgeOutputScope, replies <-chan any, creditTicks <-chan time.Time) error {
	send := func(value any) error { return sendJSON(conn, value) }
	var lastCredit mediaworker.BridgeOutputCredit
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case r := <-replies:
			if message, ok := r.(wireMessage); ok && message.kind == "output_receipt" {
				if receipt, ok := message.value.(mediaworker.BridgeOutputReceipt); ok && !receipt.Consumed && receipt.Reason == "moving" {
					// The source did not admit this identity. End the old socket without
					// declaring a failed packet; the exact destination cut reconciles it.
					return mediaworker.ErrBridgeOutputMoving
				}
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := send(r); err != nil {
				return err
			}
		case <-a.wake:
			record, ok := a.nextOutbox(a.delivered.Load())
			if !ok {
				continue
			}
			e := record.Event
			if record.Notices > 0 {
				if err := send(map[string]any{"type": "notice_gap", "count": record.Notices}); err != nil {
					return err
				}
			} else if e.Kind == "" {
				if err := send(map[string]any{"type": "input_gap", "first_seq": record.First, "last_seq": record.Last}); err != nil {
					return err
				}
			} else if e.Input != nil {
				in := e.Input
				packet := make([]byte, 18+len(in.Payload))
				packet[0] = 1
				binary.BigEndian.PutUint32(packet[1:5], in.Progress.SSRC)
				binary.BigEndian.PutUint64(packet[5:13], in.Progress.Index)
				binary.BigEndian.PutUint32(packet[13:17], in.Progress.Timestamp)
				copy(packet[18:], in.Payload)
				_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
				if err := websocket.Message.Send(conn, packet); err != nil {
					return err
				}
			} else if e.Control != nil {
				if err := send(typed("control", e.Control)); err != nil {
					return err
				}
			} else if e.Playback != nil {
				outcome, err := b.worker.BridgePlaybackOutcome(scope, e.Playback.Turn)
				if err != nil {
					return err
				}
				if err = send(typed("playback_finished", outcome)); err != nil {
					return err
				}
			}
			a.delivered.Store(record.ID)
			select {
			case a.deliveredWake <- struct{}{}:
			default:
			}
			if _, ok := a.nextOutbox(record.ID); ok {
				select {
				case a.wake <- struct{}{}:
				default:
				}
			}
		case ack := <-a.acks:
			var c checkpoint
			if err := json.Unmarshal(ack.State.Bytes, &c); err != nil {
				return err
			}
			if c.Sequence > 0 {
				if err := send(map[string]any{"type": "checkpointed", "seq": c.Sequence, "checkpoint_seq": ack.Sequence, "scope": scope, "working": c.Working, "history": ack.State.History}); err != nil {
					return err
				}
			}
			if err := send(map[string]any{"type": "output_checkpointed", "checkpoint_seq": ack.Sequence, "scope": scope, "captured": json.RawMessage(ack.Output)}); err != nil {
				return err
			}
		case <-creditTicks:
			credit, err := b.worker.BridgeOutputCredit(scope)
			if err != nil {
				return err
			}
			// Host revisions also advance on polls. Suppress unchanged values
			// without changing the host's revision/restore contract.
			comparison := credit
			comparison.Revision = lastCredit.Revision
			if comparison == lastCredit {
				continue
			}
			if err = send(typed("output_credit", credit)); err != nil {
				return err
			}
			lastCredit = credit
		}
	}
}

// Outbox delivery is retired only by a later accepted actor transaction. The
// socket writer never changes the accepted image. Emit's 100 ms floor remains
// outside both the socket writer and the host output admission path.
func emitUpdates(ctx context.Context, run *agent.TaskRun, a *adapter, candidates <-chan []byte) error {
	sequence := run.After() + 1 // Identity reserves the first emission.
	attached := false
	for {
		var update adapterUpdate
		if !attached {
			update.Attached = true
			attached = true
		} else {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case raw := <-candidates:
				var c checkpoint
				if err := json.Unmarshal(raw, &c); err != nil {
					return err
				}
				update.Checkpoint = &c
			case <-a.deliveredWake:
			}
		}
		update.Delivered = a.delivered.Load()
		raw, err := json.Marshal(update)
		if err != nil {
			return err
		}
		sequence++
		if err = run.Emit(ctx, sequence, agent.Chunk{Data: raw}); err != nil {
			return err
		}
	}
}
