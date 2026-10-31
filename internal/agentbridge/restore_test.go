package agentbridge

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/agenthost"
	"github.com/relaishq/relais/internal/componentcodec"
	"github.com/relaishq/relais/internal/redistestenv"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/relaishq/relais/pkg/storage"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/websocket"
)

// A canceled producer can deliver a late callback after replacement and move.
func TestRestoreRejectsQueuedObsoleteCheckpoint(t *testing.T) {
	a := newAdapter(t)
	ctx := context.Background()
	raw := []byte(`{"scope":{"session_id":"session","bot_instance":"old","epoch":1,"generation":1,"lineage_id":"old"},"checkpoint":{"seq":99,"version":1,"working":"b2Jzb2xldGU="},"delivered":99}`)
	state, err := a.Save(ctx)
	require.NoError(t, err)
	var saved map[string]any
	require.NoError(t, json.Unmarshal(state.Bytes, &saved))
	saved["scope"] = map[string]any{"session_id": "session", "bot_instance": "successor", "epoch": 2, "generation": 2, "lineage_id": "new"}
	saved["seq"] = 7
	saved["working"] = []byte("durable conversation")
	state.Bytes, err = json.Marshal(saved)
	require.NoError(t, err)
	require.NoError(t, a.Restore(ctx, state))
	require.NoError(t, a.bindOutboxScope(a.state.Scope))
	frozen, err := a.SaveMoveOnly(ctx)
	require.NoError(t, err)
	state, err = a.Save(ctx)
	require.NoError(t, err)
	moved := newAdapter(t)
	require.NoError(t, moved.Restore(ctx, state))
	require.NoError(t, moved.RestoreMoveOnly(ctx, frozen))
	_, err = moved.Handle(ctx, agent.Event{Kind: agent.TaskMessage, Result: &agent.TaskResult{Final: true, Chunk: agent.Chunk{Data: raw}}})
	require.NoError(t, err)
	require.EqualValues(t, 7, moved.state.Sequence, "queued old generation must not overwrite the restored baseline")
	require.Equal(t, []byte("durable conversation"), moved.state.Working)
	require.Empty(t, moved.state.BotOutcome, "old final callback cannot change successor status")
}

func conversationStores(t *testing.T, run func(*testing.T, sessionstore.Store)) {
	t.Helper()
	t.Run("memory", func(t *testing.T) { run(t, sessionstore.NewMemory()) })
	t.Run("redis", func(t *testing.T) {
		addr := redistestenv.Address(t)
		if addr == "" {
			if os.Getenv("RELAIS_TEST_REDIS_REQUIRE") == "1" {
				t.Fatal("dedicated Redis required")
			}
			t.Skip("dedicated Redis not configured")
		}
		store, err := sessionstore.NewRedis(context.Background(), storage.RedisConfig{Addr: addr, Prefix: "bridge-conversation:" + rand.Text() + ":"}, make([]byte, 32))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, store.Close()) })
		run(t, store)
	})
}

type conversationBot struct {
	conn     *websocket.Conn
	attached struct {
		Scope    mediaworker.BridgeOutputScope `json:"scope"`
		Sequence uint64                        `json:"checkpoint_seq"`
		Working  []byte                        `json:"working"`
		History  []byte                        `json:"history"`
		Resume   string                        `json:"resume"`
	}
	messages chan map[string]json.RawMessage
}

func (b *conversationBot) until(t *testing.T, kind string, seq uint64) map[string]json.RawMessage {
	t.Helper()
	deadline := time.NewTimer(4 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case m := <-b.messages:
			require.NotNil(t, m, "socket closed before %s", kind)
			var got string
			_ = json.Unmarshal(m["type"], &got)
			var number uint64
			_ = json.Unmarshal(m["seq"], &number)
			if got == "checkpointed" {
				require.NotEqualValues(t, 99, number, "obsolete acknowledgement was relabeled as the successor")
			}
			if got == kind && (seq == 0 || number == seq) {
				return m
			}
		case <-deadline.C:
			t.Fatalf("no %s for seq %d", kind, seq)
			return nil
		}
	}
}
func (b *conversationBot) send(t *testing.T, m any) {
	t.Helper()
	require.NoError(t, websocket.JSON.Send(b.conn, m))
}

// The supervisor confirms revocation and retires the old session before it
// accepts the successor assignment. Registry and bot sockets are independent.
func conversationRunner(t *testing.T, ctx context.Context, endpoint string, botID ...string) <-chan *conversationBot {
	t.Helper()
	bot, runnerID := "bot", "runner"
	if len(botID) > 0 {
		bot, runnerID = botID[0], "replacement-runner"
	}
	bots := make(chan *conversationBot, 8)
	c, err := dialBot(ctx, endpoint+"/registry")
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	require.NoError(t, websocket.JSON.Send(c, map[string]any{"type": "hello", "protocol": 1, "runner_id": runnerID, "bot_instance": bot, "agent_name": "bridge", "config_revision": "1", "capacity": 1}))
	var mu sync.Mutex
	var current *conversationBot
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			var m map[string]json.RawMessage
			if websocket.JSON.Receive(c, &m) != nil {
				return
			}
			var kind string
			_ = json.Unmarshal(m["type"], &kind)
			switch kind {
			case "reserve":
				_ = websocket.JSON.Send(c, map[string]any{"type": "reserved", "reservation_id": m["reservation_id"], "accepted": true})
			case "revoke":
				mu.Lock()
				old := current
				mu.Unlock()
				if old != nil {
					_ = old.conn.Close()
				}
				m["type"] = json.RawMessage(`"revoked"`)
				_ = websocket.JSON.Send(c, m)
			case "assign":
				conn, e := dialBot(ctx, endpoint)
				if e != nil {
					return
				}
				bot := &conversationBot{conn: conn, messages: make(chan map[string]json.RawMessage, 128)}
				if attachFake(conn, m, 0) != nil || websocket.JSON.Receive(conn, &bot.attached) != nil {
					_ = conn.Close()
					return
				}
				mu.Lock()
				current = bot
				mu.Unlock()
				bots <- bot
				go func() {
					defer close(bot.messages)
					for {
						var raw []byte
						if websocket.Message.Receive(conn, &raw) != nil {
							return
						}
						if len(raw) > 0 && raw[0] == 1 {
							continue
						}
						var message map[string]json.RawMessage
						if json.Unmarshal(raw, &message) != nil {
							return
						}
						select {
						case bot.messages <- message:
						case <-ctx.Done():
							return
						}
					}
				}()
			}
		}
	}()
	t.Cleanup(func() {
		_ = c.Close()
		mu.Lock()
		if current != nil {
			_ = current.conn.Close()
		}
		mu.Unlock()
		<-done
	})
	return bots
}
func nextConversationBot(t *testing.T, bots <-chan *conversationBot) *conversationBot {
	t.Helper()
	select {
	case b := <-bots:
		return b
	case <-time.After(4 * time.Second):
		t.Fatal("successor did not attach")
		return nil
	}
}
func TestConversationRestartRestoresExactAcknowledgedPair(t *testing.T) {
	conversationStores(t, func(t *testing.T, store sessionstore.Store) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		bridge, endpoint, _ := authorityStoreCall(t, ctx, store)
		bots := conversationRunner(t, ctx, endpoint)
		first := nextConversationBot(t, bots)
		_, gateErr := bridge.worker.BridgeOutputCheckpointed(first.attached.Scope)
		require.NoError(t, gateErr, "attached requires a durable lineage")
		working, history := []byte("completed user text"), []byte("four whole exchanges")
		first.send(t, checkpoint{Type: "checkpoint", Version: 1, Sequence: 7, Working: working, History: history})
		ack := first.until(t, "checkpointed", 7)
		var captured checkpoint
		require.NoError(t, json.Unmarshal(mustJSON(t, ack), &captured))
		require.Equal(t, working, captured.Working)
		require.Equal(t, history, captured.History)
		old := first.attached.Scope
		next, err := bridge.Restart(ctx, old)
		require.NoError(t, err)
		second := nextConversationBot(t, bots)
		require.Equal(t, next, second.attached.Scope)
		require.Equal(t, "bot_restart", second.attached.Resume)
		require.EqualValues(t, 7, second.attached.Sequence)
		require.Equal(t, working, second.attached.Working)
		require.Equal(t, history, second.attached.History)
		require.NotEqual(t, old.LineageID, next.LineageID)
		require.Equal(t, old.Generation+1, next.Generation)
		require.Eventually(t, func() bool { return bridge.worker.BridgePermitRemaining(next) > 0 }, 2*time.Second, 10*time.Millisecond)
		require.Zero(t, bridge.worker.BridgePermitRemaining(old))
		_, err = bridge.worker.InterruptBridgeOutput(ctx, old, "old-interrupt", 99, true)
		require.ErrorIs(t, err, mediaworker.ErrBridgeOutputScope)
		bridge.mu.Lock()
		require.Nil(t, bridge.revocations[old.SessionID])
		bridge.mu.Unlock()
		value, _ := bridge.agents.Load(next.SessionID)
		adapter := value.(*adapter)
		obsolete := checkpoint{Scope: old, Sequence: 99, Version: 1, Working: []byte("obsolete")}
		adapter.acks <- agenthost.BridgeCheckpoint{Sequence: 999, State: agent.State{Version: 1, Bytes: mustJSON(t, obsolete)}, Output: []byte(`{}`)}
		second.send(t, checkpoint{Type: "checkpoint", Version: 1, Sequence: 8, Working: []byte("next user"), History: history})
		second.until(t, "checkpointed", 8)
		// The per-scope stop was reset. The replacement can be exported again.
		image, err := bridge.worker.ExportSession(next.SessionID)
		require.NoError(t, err)
		require.NotEmpty(t, image)
	})
}
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}

func TestConversationIdlePartialFinalAudioFinishesWithStateChannelOpen(t *testing.T) {
	conversationStores(t, func(t *testing.T, store sessionstore.Store) {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		bridge, endpoint, _ := authorityStoreCall(t, ctx, store)
		bot := nextConversationBot(t, conversationRunner(t, ctx, endpoint))
		bot.send(t, map[string]any{"type": "output_begin", "request_id": "partial", "turn": 0})
		bot.until(t, "output_begun", 0)
		for i, payload := range [][]byte{transportOpus, {0xf0, 0xff, 0xfe}} {
			raw := binary.BigEndian.AppendUint64([]byte{2}, 0)
			raw = binary.BigEndian.AppendUint64(raw, uint64(i+1))
			require.NoError(t, websocket.Message.Send(bot.conn, append(raw, payload...)))
			bot.until(t, "output_receipt", uint64(i+1))
		}
		bot.send(t, map[string]any{"type": "audio_end", "turn": 0, "last_seq": 2})
		finished := bot.until(t, "playback_finished", 0)
		var outcome string
		require.NoError(t, json.Unmarshal(finished["outcome"], &outcome))
		require.Equal(t, "complete", outcome)
		// Completion arrives without another utterance or closing bridge.state/v1.
		bot.send(t, checkpoint{Type: "checkpoint", Version: 1, Sequence: 1, Working: []byte("idle finished")})
		bot.until(t, "checkpointed", 1)
		require.Equal(t, 1, bridge.worker.SessionCount())
	})
}

type uncertainConversationStore struct {
	sessionstore.Store
	sessionstore.ComponentStore
	mu                sync.Mutex
	armed, unreadable bool
	match             []byte
	noLoss            bool
	matchHost         bool
	committed         chan struct{}
	before            chan struct{}
	release           chan struct{}
}

func (s *uncertainConversationStore) CommitComponents(ctx context.Context, l sessionstore.Lease, c sessionstore.ComponentCommit) (sessionstore.ComponentTuple, error) {
	s.mu.Lock()
	match := s.match
	if match == nil {
		match = []byte(`"seq":8`)
	}
	candidate := c.Components.Work
	if s.matchHost {
		var transport componentcodec.TransportState
		if err := componentcodec.Unmarshal(c.Components.Transport, componentcodec.Transport, &transport); err != nil {
			s.mu.Unlock()
			return sessionstore.ComponentTuple{}, err
		}
		candidate = transport.Host
	}
	armed := s.armed && bytes.Contains(candidate, match)
	if armed {
		s.armed = false
	}
	before, release := s.before, s.release
	s.mu.Unlock()
	if armed && before != nil {
		close(before)
		select {
		case <-release:
		case <-ctx.Done():
			return sessionstore.ComponentTuple{}, ctx.Err()
		}
	}
	tuple, err := s.ComponentStore.CommitComponents(ctx, l, c)
	if armed && err == nil && !s.noLoss {
		s.mu.Lock()
		s.unreadable = true
		s.mu.Unlock()
		close(s.committed)
		return sessionstore.ComponentTuple{}, &sessionstore.TransientError{Op: "lost_reply", Err: context.DeadlineExceeded}
	}
	return tuple, err
}
func (s *uncertainConversationStore) ReadComponents(ctx context.Context, id string) (sessionstore.ComponentTuple, error) {
	s.mu.Lock()
	blocked := s.unreadable
	s.mu.Unlock()
	if blocked {
		return sessionstore.ComponentTuple{}, &sessionstore.TransientError{Op: "settlement", Err: context.DeadlineExceeded}
	}
	return s.ComponentStore.ReadComponents(ctx, id)
}
func TestConversationRestartWaitsForUncertainCheckpointSettlement(t *testing.T) {
	conversationStores(t, func(t *testing.T, base sessionstore.Store) {
		store := &uncertainConversationStore{Store: base, ComponentStore: base.(sessionstore.ComponentStore), committed: make(chan struct{})}
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()
		bridge, endpoint, _ := authorityStoreCall(t, ctx, store)
		bots := conversationRunner(t, ctx, endpoint)
		bot := nextConversationBot(t, bots)
		bot.send(t, checkpoint{Type: "checkpoint", Version: 1, Sequence: 7, Working: []byte("acknowledged")})
		bot.until(t, "checkpointed", 7)
		store.mu.Lock()
		store.armed = true
		store.mu.Unlock()
		bot.send(t, checkpoint{Type: "checkpoint", Version: 1, Sequence: 8, Working: []byte("settled newer"), History: []byte("matching history")})
		select {
		case <-store.committed:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		_, err := bridge.Restart(ctx, bot.attached.Scope)
		require.ErrorIs(t, err, ErrBotUnavailable)
		require.Zero(t, bridge.worker.BridgePermitRemaining(bot.attached.Scope))
		require.Empty(t, bots, "unresolved write cannot authorize attached")
		store.mu.Lock()
		store.unreadable = false
		store.mu.Unlock()
		_, err = bridge.Restart(ctx, bot.attached.Scope)
		require.NoError(t, err)
		next := nextConversationBot(t, bots)
		require.EqualValues(t, 8, next.attached.Sequence)
		require.Equal(t, []byte("settled newer"), next.attached.Working)
		require.Equal(t, []byte("matching history"), next.attached.History)
	})
}

func TestConversationSuccessorCannotAttachBeforeLineageCommit(t *testing.T) {
	conversationStores(t, func(t *testing.T, base sessionstore.Store) {
		store := &uncertainConversationStore{Store: base, ComponentStore: base.(sessionstore.ComponentStore), match: []byte(`"generation":2`), noLoss: true, before: make(chan struct{}), release: make(chan struct{})}
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		bridge, endpoint, _ := authorityStoreCall(t, ctx, store)
		bots := conversationRunner(t, ctx, endpoint)
		bot := nextConversationBot(t, bots)
		store.mu.Lock()
		store.armed = true
		store.mu.Unlock()
		done := make(chan error, 1)
		go func() { _, err := bridge.Restart(ctx, bot.attached.Scope); done <- err }()
		select {
		case <-store.before:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		require.Empty(t, bots, "a locally published lineage is not an attachment grant")
		scope, err := bridge.worker.BridgeOutputBinding(bot.attached.Scope.SessionID)
		require.NoError(t, err)
		require.Zero(t, bridge.worker.BridgePermitRemaining(scope))
		_, err = bridge.worker.BridgeOutputCheckpointed(scope)
		require.Error(t, err)
		close(store.release)
		require.NoError(t, <-done)
		successor := nextConversationBot(t, bots)
		_, err = bridge.worker.BridgeOutputCheckpointed(successor.attached.Scope)
		require.NoError(t, err)
	})
}

func TestRestoreRemapsContinuingLineageAtApplication(t *testing.T) {
	a := newAdapter(t)
	scope := mediaworker.BridgeOutputScope{SessionID: a.id, BotInstance: "bot", Epoch: 2, Generation: 1, LineageID: "continuing"}
	a.state.Scope = scope
	old := scope
	old.Epoch = 1
	a.state.Resume = "bot_restart"
	attached := mustJSON(t, adapterUpdate{Scope: old, Attached: true})
	_, err := a.Handle(context.Background(), agent.Event{Kind: agent.TaskMessage, Result: &agent.TaskResult{Chunk: agent.Chunk{Data: attached}}})
	require.NoError(t, err)
	require.Empty(t, a.state.Resume, "a later move must report planned_move, not another bot restart")
	data := mustJSON(t, adapterUpdate{Scope: old, Checkpoint: &checkpoint{Scope: old, Version: 1, Sequence: 8, Working: []byte("queued across move")}})
	_, err = a.Handle(context.Background(), agent.Event{Kind: agent.TaskMessage, Result: &agent.TaskResult{Chunk: agent.Chunk{Data: data}}})
	require.NoError(t, err)
	require.Equal(t, scope, a.state.Scope, "exact-move callbacks cannot lower the routing epoch")
	require.EqualValues(t, 8, a.state.Sequence)
}

func TestRestartReconciliationKeepsAlreadyAcceptedSuccessorControls(t *testing.T) {
	a := newAdapter(t)
	ctx := context.Background()
	durable, err := a.Save(ctx)
	require.NoError(t, err)
	scope := mediaworker.BridgeOutputScope{SessionID: a.id, BotInstance: "bot", Epoch: 1, Generation: 2, LineageID: "successor"}
	_, err = a.Restart(ctx, durable, agenthost.BridgeScope(scope), "runner")
	require.NoError(t, err)
	a.Accepted()
	_, err = a.Handle(ctx, agent.Event{Kind: agent.Control, Control: &agent.ControlMessage{ID: "new-control", Sequence: 1, Name: "context"}})
	require.NoError(t, err)
	a.Accepted()
	before, err := a.SaveMoveOnly(ctx)
	require.NoError(t, err)
	_, err = a.Restart(ctx, durable, agenthost.BridgeScope(scope), "runner")
	require.NoError(t, err)
	after, err := a.SaveMoveOnly(ctx)
	require.NoError(t, err)
	require.Equal(t, before, after, "baseline reconciliation must retain accepted work from this successor")
	scope.Generation++
	scope.LineageID = "replacement"
	_, err = a.Restart(ctx, durable, agenthost.BridgeScope(scope), "runner")
	require.NoError(t, err)
	require.Empty(t, a.current.Records, "a distinct replacement still invalidates old work")
}

func TestConversationRestartRecoversAfterPendingSuccessorBotChanges(t *testing.T) {
	conversationStores(t, func(t *testing.T, base sessionstore.Store) {
		store := &uncertainConversationStore{Store: base, ComponentStore: base.(sessionstore.ComponentStore), match: []byte(`"generation":2`), committed: make(chan struct{})}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		bridge, endpoint, _ := authorityStoreCall(t, ctx, store)
		first := nextConversationBot(t, conversationRunner(t, ctx, endpoint))
		first.send(t, checkpoint{Type: "checkpoint", Version: 1, Sequence: 7, Working: []byte("durable conversation"), History: []byte("matching history")})
		first.until(t, "checkpointed", 7)
		store.mu.Lock()
		store.armed = true
		store.mu.Unlock()
		pending, err := bridge.Restart(ctx, first.attached.Scope)
		require.Error(t, err)
		current, err := bridge.worker.BridgeOutputBinding(first.attached.Scope.SessionID)
		require.NoError(t, err)
		require.Equal(t, pending, current)
		bridge.mu.Lock()
		registry := bridge.runner.conn
		bridge.mu.Unlock()
		require.NoError(t, registry.Close())
		require.Eventually(t, func() bool { bridge.mu.Lock(); defer bridge.mu.Unlock(); return bridge.runner == nil }, time.Second, time.Millisecond)
		bots := conversationRunner(t, ctx, endpoint, "new-bot")
		require.Eventually(t, func() bool {
			bridge.mu.Lock()
			defer bridge.mu.Unlock()
			return bridge.runner != nil && bridge.runner.bot == "new-bot"
		}, time.Second, time.Millisecond)
		store.mu.Lock()
		store.unreadable = false
		store.armed, store.noLoss, store.matchHost = true, true, true
		store.match = []byte(`"z":true`)
		store.before, store.release = make(chan struct{}), make(chan struct{})
		store.mu.Unlock()
		// Interrupt the next stop write too. Retrying the original request must
		// still follow the promoted old scope, rather than generation one.
		attemptCtx, cancelAttempt := context.WithCancel(ctx)
		interrupted := make(chan error, 1)
		go func() { _, err := bridge.Restart(attemptCtx, first.attached.Scope); interrupted <- err }()
		select {
		case <-store.before:
			cancelAttempt()
			close(store.release)
			require.Error(t, <-interrupted)
		case err := <-interrupted:
			cancelAttempt()
			close(store.release)
			t.Fatalf("restart returned before the promoted scope stop checkpoint: %v", err)
		case <-ctx.Done():
			cancelAttempt()
			close(store.release)
			t.Fatal(ctx.Err())
		}
		successor, err := bridge.Restart(ctx, first.attached.Scope)
		require.NoError(t, err, "a different runner bot must not strand the locally committed successor")
		attached := nextConversationBot(t, bots)
		require.Equal(t, successor, attached.attached.Scope)
		require.Equal(t, "new-bot", successor.BotInstance)
		require.Greater(t, successor.Generation, pending.Generation)
		require.Equal(t, []byte("durable conversation"), attached.attached.Working)
		require.Equal(t, []byte("matching history"), attached.attached.History)
		require.Zero(t, bridge.worker.BridgePermitRemaining(pending))
	})
}

func TestCheckpointCannotSetHostResumeReason(t *testing.T) {
	for _, resume := range []string{"", "bot_restart"} {
		t.Run(resume, func(t *testing.T) {
			a := newAdapter(t)
			scope := mediaworker.BridgeOutputScope{SessionID: a.id, BotInstance: "bot", Epoch: 1, Generation: 1, LineageID: "lineage"}
			a.state.Scope = scope
			a.state.Resume = resume
			raw := mustJSON(t, adapterUpdate{Scope: scope, Checkpoint: &checkpoint{Version: 1, Sequence: 1, Resume: "forged", Working: []byte("user text")}})
			_, err := a.Handle(context.Background(), agent.Event{Kind: agent.TaskMessage, Result: &agent.TaskResult{Chunk: agent.Chunk{Data: raw}}})
			require.NoError(t, err)
			require.Equal(t, resume, a.state.Resume, "resume reason belongs to the host")
		})
	}
}
