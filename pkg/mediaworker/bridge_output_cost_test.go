package mediaworker

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"
	"unsafe"

	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/stretchr/testify/require"
)

// Independent inventory of the frozen owned image. Payloads in bridge records
// are immutable shared references, unlike task/adapter byte copies.
func bridgeFrozenImageBytes(image agentMoveExport) int64 {
	n := int64(len(image.payload)) + int64(cap(image.audio))*int64(unsafe.Sizeof(agent.Audio{}))
	n += int64(cap(image.tasks)) * int64(unsafe.Sizeof(taskMove{}))
	for _, task := range image.tasks {
		n += int64(cap(task.Request))
		for _, results := range [][]agent.TaskResult{task.Results, task.Audio} {
			n += int64(cap(results)) * int64(unsafe.Sizeof(agent.TaskResult{}))
			for _, result := range results {
				n += int64(cap(result.Chunk.Data))
			}
		}
	}
	b := image.bridge
	n += int64(cap(b.Settled)) * int64(unsafe.Sizeof(bridgeSettledRange{}))
	n += int64(cap(b.Records))*int64(unsafe.Sizeof(bridgePacketRecord{})) + int64(cap(b.Queue))*int64(unsafe.Sizeof(bridgeQueued{}))
	for _, state := range []*bridgeOutputState{b.State, b.Durable} {
		if state != nil {
			n += int64(unsafe.Sizeof(*state)) + int64(cap(state.Turns))*int64(unsafe.Sizeof(bridgeTurn{})) + int64(cap(state.Exceptions))*int64(unsafe.Sizeof(bridgeReceiptRange{}))
		}
	}
	return n
}

// Populate maximum mailbox occupancy with many small owned result headers,
// rather than a single large byte slice, through production emission admission.
func growBridgeMailboxFragments(t *testing.T, rig acceptedEventRig) {
	t.Helper()
	s := rig.s
	rig.app.handle = nil
	rig.app.state.HasMoveOnly = true
	rig.app.state.MoveOnlyLength = s.agent.v2.bridge.limits.AdapterMoveOnlyBytes
	s.agent.v2.instance = &bridgeEnvelopeAgent{acceptedEventAgent: rig.app}
	_, err := (acceptedEvents{s}).submit(agent.Event{Kind: agent.TimerFired, ID: "grow"}, eventLive)
	require.NoError(t, err)
	terminal := 0
	for _, task := range s.agent.v2.tasks {
		terminal += task.terminalBytes
	}
	task := s.agent.v2.tasks[0]
	for seq := uint64(1); ; seq++ {
		chunk := agent.Chunk{Data: []byte{0, 0, 0}}
		r := agent.TaskResult{TaskID: task.intent.ID, Sequence: seq, Turn: task.intent.Turn, Outcome: agent.OK, Chunk: chunk}
		if terminal+task.mailboxBytes+taskResultBytes(r) > s.agent.v2.bridge.limits.ObserverMailboxBytes {
			break
		}
		task.lastEmission = time.Time{}
		reply := make(chan error, 1)
		s.acceptEmission(taskEmission{id: task.intent.ID, ctx: context.Background(), attempt: task.attempt, sequence: seq, chunk: chunk, reply: reply})
		require.NoError(t, <-reply)
	}
	require.Greater(t, len(task.results), 50)
}

func TestBridgeOutputCostBoundsEncodedAndFrozenState(t *testing.T) {
	rng := rand.New(rand.NewPCG(14, 254))
	for trial := range 32 {
		t.Run(fmt.Sprint(trial), func(t *testing.T) {
			rig, scope := newBridgeRig(t, false)
			s := rig.s
			// Long valid scope tokens need no escaping; their actual JSON is charged.
			if trial%4 < 2 {
				scope.BotInstance = strings.Repeat("x", 64)
				scope.LineageID = strings.Repeat("y", 64)
				s.agent.v2.bridge.state = nil // bind a fresh test fixture at maximum token lengths
				require.NoError(t, s.worker.BindBridgeOutput(scope))
				_, err := s.worker.BeginBridgeOutput(scope, 1)
				require.NoError(t, err)
			}
			size := []int{160, 1200}[trial%2]
			capCount := fillBridgeToCap(t, s, scope, size)
			require.Positive(t, capCount)
			maximumBridgeObservers(t, rig)
			if trial%8 == 7 {
				growBridgeMailboxFragments(t, rig)
			} else {
				growBridgeActorEnvelope(t, rig)
			}
			// Raw byte fields always use base64, even if every byte would need six
			// bytes of JSON string escaping. Task detail is an actual JSON string.
			for _, task := range s.agent.v2.tasks {
				for i := range task.request {
					task.request[i] = byte(rng.Uint32())
				}
				for i := range task.results {
					result := &task.results[i]
					before := taskResultBytes(*result)
					if len(result.Chunk.Data) < 1024 {
						continue
					}
					result.Detail = strings.Repeat("\x00<>&\"\\\u2028", 8)
					growth := taskResultBytes(*result) - before
					result.Chunk.Data = result.Chunk.Data[:len(result.Chunk.Data)-growth]
					for j := range result.Chunk.Data {
						result.Chunk.Data[j] = byte(rng.Uint32())
					}
					require.LessOrEqual(t, taskResultBytes(*result), before)
				}
			}
			// Keep full working/history on every boundary case; randomize lengths on
			// the remaining trials without changing the future-state allowance.
			if trial%4 > 1 {
				rig.app.state.Bytes = make([]byte, rng.IntN(s.agent.v2.registration.Bounds.MaxStateBytes+1))
				rig.app.state.History = make([]byte, rng.IntN(8193))
			}
			_, err := (acceptedEvents{s}).submit(agent.Event{Kind: agent.TimerFired}, eventLive)
			require.NoError(t, err)
			// A random prefix is sent but retained, exercising both ledger-only and
			// queued records. Boundary trials preserve the complete queue at the cap.
			if trial%4 > 1 {
				for range rng.IntN(capCount + 1) {
					paceBridge(t, s)
				}
			}
			s.mu.Lock()
			total, scratch := s.bridgeMoveCost(s.moveReserved, s.moveRequestBytes.Load())
			require.NoError(t, s.reserveV2ExportScratch())
			require.Equal(t, scratch, s.exportScratch.bytes.Load())
			require.Equal(t, total, s.moveReserved+s.agent.v2.bridge.memory.Load()+s.exportScratch.bytes.Load())
			s.agent.paused = true
			s.mu.Unlock()
			image, err := s.saveV2MoveOnly(context.Background())
			require.NoError(t, err)
			// Include actual binary components and capture's working/history copies,
			// independently of bridgeMoveCost's cap-based fixed term.
			conn, ok := s.dtlsConn.ConnectionState()
			require.True(t, ok)
			dtls, err := conn.MarshalBinary()
			require.NoError(t, err)
			parts, err := splitSessionComponents(snapshot{State: s.state, DTLSConnection: dtls}, s.state.Agent.History, nil)
			require.NoError(t, err)
			copies := bridgeFrozenImageBytes(image) + int64(len(parts.Setup)+len(parts.Transport)+len(parts.Work)+len(parts.History)+len(dtls)+len(s.state.Agent.State.Bytes)+2*len(s.state.Agent.History))
			data, err := s.checkpoint.capture(checkpointExact, &movePayload{Bridge: image.bridge, Tasks: image.tasks, Audio: image.audio, Bytes: image.payload})
			require.NoError(t, err, "export at advertised cap")
			require.NoError(t, validateV2Wire(data))
			fragment := max(len(parts.Setup), len(parts.Transport), len(parts.Work), len(parts.History))
			temporary := bridgeBase64Bytes(int64(fragment)) + 2
			for _, task := range image.tasks {
				temporary = max(temporary, bridgeBase64Bytes(int64(len(task.Request)))+2)
				for _, result := range task.Results {
					temporary = max(temporary, int64(taskResultBytes(result)))
				}
			}
			temporary = max(temporary, bridgeBase64Bytes(int64(len(image.payload)))+2)
			reservation := copies + 3*int64(len(data)) + 2*temporary
			require.GreaterOrEqual(t, scratch, reservation, "bound must cover actual frozen copies and encoded-buffer reservation")
			require.GreaterOrEqual(t, total, int64(len(data)))
			require.LessOrEqual(t, total, s.bridgeLookaheadBudget())
			t.Logf("cap=%d size=%d records=%d encoded=%d frozen_copies=%d image_reservation_bound=%d model_scratch=%d model_total=%d", capCount, size, len(image.bridge.Records), len(data), copies, reservation, scratch, total)
		})
	}
}

func TestBridgeOutputCostTerms(t *testing.T) {
	rig, scope := newBridgeRig(t, false)
	s := rig.s
	owned, encoded := s.bridgeFixedMoveCost()
	t.Logf("actor=%d fixed_owned=%d fixed_encoded=%d fixed_scratch=%d temporary=%d live_record=%d frozen_record=%d budget=%d", s.bridgeActorMoveCost(0, 0), owned, encoded, owned+3*encoded, s.bridgeEncodingTemporaryBytes(), bridgeRecordCharge, bridgeFrozenRecordBytes, s.bridgeLookaheadBudget())
	for _, size := range []int{0, 160, 1200} {
		r := bridgePacketRecord{Scope: scope, Duration: 20 * time.Millisecond, Length: size, Stream: 1}
		r.Receipt = BridgeOutputReceipt{BridgeOutputScope: scope, Sequence: 1, Turn: 1, Revision: 1, Outcome: "admitted", Consumed: true}
		if size == 0 {
			r.Receipt.Outcome = "sent"
		} else {
			r.Payload = bridgePacket(size)
		}
		data, err := json.Marshal(r)
		require.NoError(t, err)
		require.Equal(t, len(data), bridgeRecordJSONBytes(r))
		t.Logf("packet_bytes=%d record_json=%d queue_json=%d cost=%d", size, len(data), bridgeRecordQueueJSONBytes(r), bridgePacketExportCost(r))
	}
}

func TestBridgeOutputActorReserveIsFloor(t *testing.T) {
	rig, scope := newBridgeRig(t, false)
	s := rig.s
	before, err := s.worker.BridgeOutputCredit(scope)
	require.NoError(t, err)
	total := s.bridgeExportCostLocked()
	s.agent.v2.bridge.limits.ActorReserveBytes = 64 << 10
	after, err := s.worker.BridgeOutputCredit(scope)
	require.NoError(t, err)
	require.Equal(t, before.PacketSlots, after.PacketSlots)
	require.Equal(t, before.PayloadBytes, after.PayloadBytes)
	require.Equal(t, total, s.bridgeExportCostLocked(), "a smaller overlapping floor cannot add credit")
	actor := s.bridgeActorMoveCost(0, 0)
	s.agent.v2.bridge.limits.ActorReserveBytes = int(actor) + 1024
	require.Equal(t, total+1024, s.bridgeExportCostLocked())
}

func TestBridgeOutputWorstCaseEncodingBounds(t *testing.T) {
	scope := BridgeOutputScope{SessionID: strings.Repeat("s", 64), BotInstance: strings.Repeat("b", 64), LineageID: strings.Repeat("l", 64), Epoch: ^uint64(0), Generation: ^uint64(0)}
	for size := range 1201 {
		raw := make([]byte, size)
		for i := range raw {
			raw[i] = 0xff
		}
		data, err := json.Marshal(raw)
		require.NoError(t, err)
		require.Equal(t, int64(len(data)), bridgeBase64Bytes(int64(size))+2)
	}
	for _, size := range []int{0, 1, 2, 3, 160, 1200} {
		r := bridgePacketRecord{Scope: scope, Duration: 120 * time.Millisecond, Length: 1200, Stream: ^uint64(0), Payload: make([]byte, size)}
		r.Receipt = BridgeOutputReceipt{BridgeOutputScope: scope, Sequence: ^uint64(0), Turn: ^agent.Turn(0), Revision: ^uint64(0), Outcome: "refused", Reason: "invalid_audio", Consumed: true}
		for i := range r.Digest {
			r.Digest[i] = 255
		}
		data, err := json.Marshal(r)
		require.NoError(t, err)
		require.Equal(t, len(data), bridgeRecordJSONBytes(r))
		// Pending credits must bound every maximal record plus payload expansion.
		require.GreaterOrEqual(t, bridgeCreditRecordCost(scope, ^uint64(0))+5*int64(size), bridgePacketExportCost(r))
	}
	for _, text := range []string{strings.Repeat("\x00", 128), strings.Repeat("<>&", 40), strings.Repeat("\"\\\u2028\u2029", 20)} {
		data, err := json.Marshal(text)
		require.NoError(t, err)
		require.LessOrEqual(t, len(data), 6*len(text)+2)
		result := agent.TaskResult{TaskID: "result", Detail: text, Chunk: agent.Chunk{Data: make([]byte, 1200)}}
		data, err = json.Marshal(result)
		require.NoError(t, err)
		require.GreaterOrEqual(t, taskResultBytes(result), len(data), "mailbox admission already pays for escaping and base64")
	}
}

func TestBridgeOutputDirectComponentEncodingPreservesBytes(t *testing.T) {
	rig, scope := newBridgeRig(t, false)
	maximumBridgeObservers(t, rig)
	growBridgeActorEnvelope(t, rig)
	fillBridgeToCap(t, rig.s, scope, 160)
	s := rig.s
	image, err := s.saveV2MoveOnly(context.Background())
	require.NoError(t, err)
	conn, ok := s.dtlsConn.ConnectionState()
	require.True(t, ok)
	dtls, err := conn.MarshalBinary()
	require.NoError(t, err)
	value := snapshot{Version: s.state.Version, State: s.state, DTLSConnection: dtls, AgentMoveOnly: &movePayload{Bridge: image.bridge, Tasks: image.tasks, Audio: image.audio, Bytes: image.payload}, split: s.checkpoint.componentExportState()}
	nested, err := json.Marshal(value)
	require.NoError(t, err)
	direct, err := encodeComponentSnapshot(value)
	require.NoError(t, err)
	require.Equal(t, nested, direct)
	t.Logf("identical direct/nested exact encoding: %d bytes", len(direct))
}

type bridgeExportWriter struct {
	*httptest.ResponseRecorder
	worker   *Worker
	maxWrite int
	held     bool
}

func (w *bridgeExportWriter) Write(p []byte) (int, error) {
	w.maxWrite = max(w.maxWrite, len(p))
	w.held = w.held && w.worker.moveBytes.Load() > 0
	return w.ResponseRecorder.Write(p)
}
func TestBridgeOutputHTTPExportUsesBoundedEncoding(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rig, scope := newBridgeRig(t, false)
		s := rig.s
		maximumBridgeObservers(t, rig)
		growBridgeActorEnvelope(t, rig)
		count := fillBridgeToCap(t, s, scope, 1200)
		s.worker.cfg.Agent.CallbackTimeout = 10 * time.Millisecond
		s.mu.Lock()
		s.agent.v2.playback.initialized = true
		s.agent.v2.playback.nextAt = s.taskClockLocked().Add(time.Hour)
		s.mu.Unlock()
		startBridgeActor(t, s)
		synctest.Wait()
		writer := &bridgeExportWriter{ResponseRecorder: httptest.NewRecorder(), worker: s.worker, held: true}
		request := httptest.NewRequest(http.MethodPost, "/sessions/"+s.id+"/export", nil)
		s.worker.PrivateHandler().ServeHTTP(writer, request)
		require.Equal(t, http.StatusOK, writer.Code)
		require.True(t, writer.held, "reservation must remain held through the reply")
		require.LessOrEqual(t, writer.maxWrite, 1024, "base64 reply must not create a whole-export buffer")
		var reply ExportReply
		require.NoError(t, json.Unmarshal(writer.Body.Bytes(), &reply))
		cut, err := decodeSnapshot(reply.State)
		require.NoError(t, err)
		require.Len(t, cut.AgentMoveOnly.Bridge.Queue, count)
		expected, err := json.Marshal(reply)
		require.NoError(t, err)
		require.Equal(t, string(expected)+"\n", writer.Body.String())
		require.Zero(t, s.worker.moveBytes.Load(), "close and reply release all holds")
		t.Logf("packets=%d exact=%d wire=%d max_write=%d", count, len(reply.State), writer.Body.Len(), writer.maxWrite)
	})
}
