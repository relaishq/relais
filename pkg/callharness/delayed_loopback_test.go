package callharness

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/relaishq/relais/examples/delayed-loopback/ledger"
	"github.com/relaishq/relais/examples/delayed-loopback/loopback"
	"github.com/relaishq/relais/internal/agenthost"
	"github.com/relaishq/relais/internal/workerprobe"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/relaishq/relais/pkg/workerapp"
	"github.com/stretchr/testify/require"
)

func exampleRegistration(r workerapp.Registration) agenthost.Registration {
	return agenthost.Registration{Name: r.Name, Factory: r.Factory, Tasks: r.Tasks, Bounds: agenthost.Bounds(r.Limits)}
}
func loopState(t *testing.T, w *mediaworker.Worker, id string) (loopback.State, bool) {
	t.Helper()
	saved, _, err := w.SessionAgent(id)
	if err != nil {
		return loopback.State{}, false
	}
	var state loopback.State
	err = json.Unmarshal(saved.Bytes, &state)
	return state, err == nil
}
func waitLoopState(t *testing.T, w *mediaworker.Worker, id string, predicate func(loopback.State) bool) loopback.State {
	t.Helper()
	var state loopback.State
	require.Eventually(t, func() bool { var ok bool; state, ok = loopState(t, w, id); return ok && predicate(state) }, 6*time.Second, 2*time.Millisecond)
	return state
}
func sendLoopPackets(t *testing.T, call *Call, packets [][]byte) {
	t.Helper()
	for _, packet := range packets {
		require.NoError(t, call.audio.WriteSample(media.Sample{Data: packet, Duration: 20 * time.Millisecond}))
		time.Sleep(3 * time.Millisecond)
	}
}
func encodedLoopPackets(packets [][]byte) []byte {
	data := []byte{1}
	for _, p := range packets {
		data = binary.BigEndian.AppendUint16(data, uint16(len(p)))
		data = append(data, p...)
	}
	return data
}
func loopRegs(cut string) ([]agenthost.Registration, []chan []byte) {
	regs := make([]agenthost.Registration, 2)
	requests := []chan []byte{make(chan []byte, 8), make(chan []byte, 8)}
	for i := range regs {
		r := loopback.Registration()
		r.Tasks[loopback.TaskKind] = func(ctx context.Context, run *agent.TaskRun) error {
			requests[i] <- run.Spec().Request
			if i == 0 && cut == "processing" {
				<-ctx.Done()
				return ctx.Err()
			}
			return loopback.Play(ctx, run)
		}
		regs[i] = exampleRegistration(r)
	}
	return regs, requests
}
func waitLoopRequest(t *testing.T, ch <-chan []byte) []byte {
	t.Helper()
	select {
	case request := <-ch:
		return request
	case <-time.After(6 * time.Second):
		t.Fatal("no loopback task request")
		return nil
	}
}
func assertLoopPackets(t *testing.T, all []pacedPacket, want [][]byte) {
	t.Helper()
	require.Len(t, all, len(want))
	for i, p := range all {
		require.Equal(t, want[i], p.packet.Payload, "packet %d", i)
		if i > 0 {
			require.Equal(t, all[i-1].packet.SequenceNumber+1, p.packet.SequenceNumber)
		}
	}
}

// One real timer + packetless completion + exact recording move stays in short
// mode. Larger cut matrices are explicitly outside the PR timing budget.
func TestDelayedLoopbackFastRecordingMove(t *testing.T) { testLoopMove(t, "recording") }
func TestDelayedLoopbackMoveCuts(t *testing.T) {
	if testing.Short() {
		t.Skip("full cut matrix; recording/timer proof remains in short mode")
	}
	for _, cut := range []string{"processing", "playout"} {
		t.Run(cut, func(t *testing.T) { testLoopMove(t, cut) })
	}
}
func testLoopMove(t *testing.T, cut string) {
	forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
		regs, requests := loopRegs(cut)
		sys := startAgentHarness(t, store, agentHarnessSettings{v2: regs, remoteTarget: true, callbackBudget: 2 * time.Second})
		if cut == "playout" {
			require.NoError(t, workerprobe.SetHoldTaskResults(sys.workers[0].LocalAddr(), true))
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		call, err := sys.h.Dial(ctx, CallOptions{})
		require.NoError(t, err)
		received := receivePacer(call)
		want := pacerFixtures(t, 20)
		sendLoopPackets(t, call, want)
		waitLoopState(t, sys.workers[0], call.SessionID(), func(s loopback.State) bool { return s.Packets == 20 })
		var all []pacedPacket
		switch cut {
		case "processing":
			require.Equal(t, encodedLoopPackets(want), waitLoopRequest(t, requests[0]))
		case "playout":
			require.Equal(t, encodedLoopPackets(want), waitLoopRequest(t, requests[0]))
			readTaskSnapshot(t, store, call.SessionID(), func(s taskSnapshot) bool { return len(s.State.Agent.V2.Tasks) == 1 && s.State.Agent.V2.Tasks[0].Final })
			require.NoError(t, workerprobe.SetHoldTaskResults(sys.workers[0].LocalAddr(), false))
			all = append(all, nextPaced(t, received))
		}
		_, err = sys.plane.Move(ctx, call.SessionID(), "1")
		require.NoError(t, err)
		for len(all) < len(want) {
			all = append(all, nextPaced(t, received))
		}
		assertLoopPackets(t, all, want)
		state := waitLoopState(t, sys.workers[1], call.SessionID(), func(s loopback.State) bool { return s.Turn == 2 })
		require.NotNil(t, state.Resume)
		require.Equal(t, agent.OK, state.Resume.MoveOnly)
		if cut != "playout" {
			require.Equal(t, encodedLoopPackets(want), waitLoopRequest(t, requests[1]))
		} else {
			require.Empty(t, requests[1], "full reply moves without re-running its handler")
		}
		select {
		case packet := <-received:
			t.Fatalf("reply played twice: %v", packet.packet.SequenceNumber)
		case <-time.After(120 * time.Millisecond):
		}
		// Diagnostics use the existing PRIVATE status surface, and do not invoke Save.
		response := httptest.NewRecorder()
		sys.workers[1].PrivateHandler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/status?agent-session="+call.SessionID(), nil))
		require.Equal(t, 200, response.Code)
		var status mediaworker.WorkerStatus
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &status))
		require.NotNil(t, status.AgentSession)
		require.Equal(t, loopback.Name, status.AgentSession.Name)
		require.True(t, status.AgentSession.CheckpointKnown)
		require.GreaterOrEqual(t, status.AgentSession.SnapshotAgeMS, float64(0))
		require.NotEmpty(t, status.AgentSession.App)
		_, err = call.Hangup(ctx)
		require.NoError(t, err)
	})
}
func TestDelayedLoopbackTakeoverCuts(t *testing.T) {
	if testing.Short() {
		t.Skip("takeover cut matrix; crash metadata reset has a fast app test")
	}
	for _, cut := range []string{"recording", "processing", "playout"} {
		t.Run(cut, func(t *testing.T) {
			forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
				regs, requests := loopRegs(cut)
				sys := startAgentHarness(t, store, agentHarnessSettings{v2: regs, callbackBudget: 2 * time.Second})
				ctx, cancel := context.WithTimeout(context.Background(), 16*time.Second)
				defer cancel()
				call, err := sys.h.Dial(ctx, CallOptions{})
				require.NoError(t, err)
				received := receivePacer(call)
				want := pacerFixtures(t, 20)
				sendLoopPackets(t, call, want)
				waitLoopState(t, sys.workers[0], call.SessionID(), func(s loopback.State) bool { return s.Packets == 20 })
				switch cut {
				case "processing":
					waitLoopRequest(t, requests[0])
				case "playout":
					nextPaced(t, received)
				}
				// Require a durable snapshot AT the cut, rather than killing on stale app state.
				require.Eventually(t, func() bool {
					raw, err := store.GetState(ctx, call.SessionID())
					if err != nil {
						return false
					}
					var snapshot v2HarnessSnapshot
					if json.Unmarshal(raw, &snapshot) != nil {
						return false
					}
					var s loopback.State
					return json.Unmarshal(snapshot.State.Agent.State.Bytes, &s) == nil && s.Phase == cut && s.Turn == 1 && s.Packets == 20
				}, 2*time.Second, 2*time.Millisecond)
				require.NoError(t, workerprobe.Kill(sys.workers[0].LocalAddr()))
				resumed := waitLoopState(t, sys.workers[1], call.SessionID(), func(s loopback.State) bool { return s.Turn == 2 && s.Resume != nil })
				require.Equal(t, agent.OutcomeUnknown, resumed.Resume.MoveOnly)
				require.Equal(t, agent.OutcomeUnknown, resumed.Outcome)
				require.Equal(t, "recording", resumed.Phase)
				saved, _, err := sys.workers[1].SessionAgent(call.SessionID())
				require.NoError(t, err)
				var metadata loopback.State
				require.NoError(t, json.Unmarshal(saved.Bytes, &metadata))
				require.Zero(t, metadata.Cursor)
				require.Zero(t, metadata.Packets)
				// Drain packets sent by the dying owner. No old unsent ref may play on target.
				for len(received) > 0 {
					<-received
				}
				select {
				case p := <-received:
					t.Fatalf("old audio after unknown recovery: %v", p.packet.SequenceNumber)
				case <-time.After(120 * time.Millisecond):
				}
				require.Empty(t, requests[1], "lost ReadOnly input cannot regenerate itself")
				sendLoopPackets(t, call, want[:5])
				all := collectReviewPackets(t, received, 5, 5*time.Second)
				for i := range all {
					require.Equal(t, want[i], all[i].packet.Payload)
				}
				waitLoopState(t, sys.workers[1], call.SessionID(), func(s loopback.State) bool { return s.Turn == 3 })
				_, err = call.Hangup(ctx)
				require.NoError(t, err)
			})
		})
	}
}

func TestDelayedLoopbackLedgerRecovery(t *testing.T) {
	if testing.Short() {
		t.Skip("ledger recovery matrix; commit/reopen proof remains in fast app tests")
	}
	for _, crash := range []bool{false, true} {
		t.Run(fmt.Sprintf("crash=%v", crash), func(t *testing.T) {
			forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
				l, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.jsonl"))
				require.NoError(t, err)
				defer func() { require.NoError(t, l.Close()) }()
				server := httptest.NewServer(l)
				defer server.Close()
				committed := make(chan string, 2)
				var calls atomic.Int64
				regs := make([]agenthost.Registration, 2)
				for i := range regs {
					r := ledger.Registration(server.URL)
					handler := r.Tasks[ledger.TaskKind]
					r.Tasks[ledger.TaskKind] = func(ctx context.Context, run *agent.TaskRun) error {
						calls.Add(1)
						err := handler(ctx, run)
						if err != nil {
							return err
						}
						committed <- run.Key()
						if i == 0 {
							<-ctx.Done()
							return ctx.Err()
						}
						return nil
					}
					regs[i] = exampleRegistration(r)
				}
				sys := startAgentHarness(t, store, agentHarnessSettings{v2: regs, callbackBudget: 2 * time.Second})
				ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
				defer cancel()
				call, err := sys.h.Dial(ctx, CallOptions{})
				require.NoError(t, err)
				first := awaitLedgerKey(t, committed)
				taskCheckpointed(t, store, call.SessionID())
				if crash {
					require.NoError(t, workerprobe.Kill(sys.workers[0].LocalAddr()))
				} else {
					_, err = sys.plane.Move(ctx, call.SessionID(), "1")
					require.NoError(t, err)
				}
				require.Equal(t, first, awaitLedgerKey(t, committed))
				require.Eventually(t, func() bool {
					s, _, err := sys.workers[1].SessionAgent(call.SessionID())
					return err == nil && json.Valid(s.Bytes) && string(s.Bytes) != "" && containsLedgerOK(s.Bytes)
				}, 3*time.Second, 2*time.Millisecond)
				deliveries, commits := l.Counts()
				require.GreaterOrEqual(t, deliveries, 2)
				require.Equal(t, 1, commits)
				require.EqualValues(t, 2, calls.Load())
				_, err = call.Hangup(ctx)
				require.NoError(t, err)
			})
		})
	}
}
func containsLedgerOK(data []byte) bool {
	var s struct{ Outcome agent.Outcome }
	return json.Unmarshal(data, &s) == nil && s.Outcome == agent.OK
}
func awaitLedgerKey(t *testing.T, ch <-chan string) string {
	t.Helper()
	select {
	case key := <-ch:
		return key
	case <-time.After(5 * time.Second):
		t.Fatal("missing ledger commit")
		return ""
	}
}

func TestDelayedLoopbackCrashSmallerTarget(t *testing.T) {
	if testing.Short() {
		t.Skip("smaller-target takeover; immediate length reset has a fast app test")
	}
	forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
		regs, _ := loopRegs("recording")
		source := harnessSnapshotLimits(0)
		target := source
		target.MoveBytesPerSession = 4096
		sys := startAgentHarness(t, store, agentHarnessSettings{v2: regs, snapshotLimits: []mediaworker.SnapshotLimits{source, target}, callbackBudget: 2 * time.Second})
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()
		call, err := sys.h.Dial(ctx, CallOptions{})
		require.NoError(t, err)
		packets := make([][]byte, 12)
		for i := range packets {
			packets[i] = make([]byte, 1000)
			copy(packets[i], []byte{0xf8, 0xff, 0xfe})
		}
		sendLoopPackets(t, call, packets)
		require.Eventually(t, func() bool {
			raw, err := store.GetState(ctx, call.SessionID())
			if err != nil {
				return false
			}
			var snapshot v2HarnessSnapshot
			return json.Unmarshal(raw, &snapshot) == nil && snapshot.State.Agent.V2.MoveOnlyLength > target.MoveBytesPerSession
		}, time.Second, 2*time.Millisecond)
		require.NoError(t, workerprobe.Kill(sys.workers[0].LocalAddr()))
		resumed := waitLoopState(t, sys.workers[1], call.SessionID(), func(s loopback.State) bool { return s.Turn == 2 && s.Resume != nil })
		require.Equal(t, agent.OutcomeUnknown, resumed.Resume.MoveOnly)
		require.Zero(t, sys.workers[1].AgentStats().RestoreFailures)
		_, err = call.Hangup(ctx)
		require.NoError(t, err)
	})
}
