package mediaworker

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/srtp/v3"
	"github.com/relais/pkg/agent"
	"github.com/stretchr/testify/require"
)

type countingAgent struct {
	count    uint64
	process  func(context.Context, agent.Input) error
	save     func(uint64) error
	restore  error
	oversize bool
}

func (a *countingAgent) Process(ctx context.Context, in agent.Input) (agent.Output, error) {
	if a.process != nil {
		if err := a.process(ctx, in); err != nil {
			return agent.Output{}, err
		}
	}
	a.count++
	return agent.Output{Audio: in.Payload, Changed: true}, nil
}
func (a *countingAgent) Save(context.Context) (agent.State, error) {
	if a.save != nil {
		if err := a.save(a.count); err != nil {
			return agent.State{}, err
		}
	}
	data := make([]byte, 8)
	binary.BigEndian.PutUint64(data, a.count)
	if a.oversize && a.count > 0 {
		data = make([]byte, agent.DefaultMaxStateBytes+1)
	}
	return agent.State{Version: 1, Bytes: data}, nil
}
func (a *countingAgent) Restore(_ context.Context, state agent.State) error {
	if state.Version != 1 {
		return agent.ErrVersion
	}
	if a.restore != nil {
		return a.restore
	}
	if len(state.Bytes) != 8 {
		return errors.New("bad counter")
	}
	a.count = binary.BigEndian.Uint64(state.Bytes)
	return nil
}
func (*countingAgent) Resume(context.Context, agent.ResumeNotice) error { return nil }

func agentPacketSession(t *testing.T, a agent.Agent, timeout time.Duration) (*Worker, *session, *srtp.Context) {
	t.Helper()
	w, err := New(Config{Agent: AgentConfig{Factory: func(string) agent.Agent { return a }, CallbackTimeout: timeout, QueueCapacity: 2}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, w.Close()) })
	s := sessionFromState(w, sessionState{ID: "agent-unit", ICE: iceState{RemoteAddr: w.LocalAddr()}, SRTP: srtpState{Inbound: make(map[uint32]uint64)}, Audio: trackState{MID: "0", SSRC: 123, PayloadType: 111, InitialSeq: 1200}, Video: trackState{MID: "1", SSRC: 124, PayloadType: 96, InitialSeq: 1200}})
	require.NoError(t, s.initAgent(false, ResumeOptions{}))
	profile := srtp.ProtectionProfileAeadAes128Gcm
	keys := testSessionKeys(t, profile)
	s.srtpIn = testContext(t, keys.RemoteMasterKey, keys.RemoteMasterSalt, profile)
	s.srtpOut = testContext(t, keys.LocalMasterKey, keys.LocalMasterSalt, profile)
	caller := testContext(t, keys.RemoteMasterKey, keys.RemoteMasterSalt, profile)
	w.mu.Lock()
	w.sessions[s.id] = s
	w.running.Add(1)
	w.mu.Unlock()
	go func() { defer w.running.Done(); s.agentLoop() }()
	return w, s, caller
}

func TestAgentOversizeAndSaveFailureCloseSession(t *testing.T) {
	for _, kind := range []string{"oversize", "save"} {
		t.Run(kind, func(t *testing.T) {
			a := &countingAgent{oversize: kind == "oversize"}
			if kind == "save" {
				a.save = func(n uint64) error {
					if n > 0 {
						return errors.New("injected Save failure")
					}
					return nil
				}
			}
			w, s, caller := agentPacketSession(t, a, time.Second)
			s.handleRTP(testEncrypt(t, caller, 456, 1))
			require.Eventually(t, func() bool { return w.SessionCount() == 0 }, time.Second, time.Millisecond)
			stats := w.AgentStats()
			if kind == "save" {
				require.EqualValues(t, 1, stats.SaveFailures)
			} else {
				require.EqualValues(t, 1, stats.OversizedStates)
			}
			require.EqualValues(t, 0, s.state.Audio.Packets, "invalid state cannot emit audio")
		})
	}
}

func TestAgentUnknownVersionAndFailedRestoreNeverAdopt(t *testing.T) {
	for _, kind := range []string{"version", "restore", "oversize", "missing"} {
		t.Run(kind, func(t *testing.T) {
			source, err := New(Config{Agent: AgentConfig{Factory: func(string) agent.Agent { return &countingAgent{} }}})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, source.Close()) })
			call, _ := dialDTLSCaller(t, source)
			data, err := source.ExportSession(call.id)
			require.NoError(t, err)
			var snap snapshot
			require.NoError(t, json.Unmarshal(data, &snap))
			switch kind {
			case "version":
				snap.State.Agent.State.Version = 99
			case "oversize":
				snap.State.Agent.State.Bytes = make([]byte, agent.DefaultMaxStateBytes+1)
			case "missing":
				snap.State.Agent = agentState{}
			}
			data, err = json.Marshal(snap)
			require.NoError(t, err)
			target, err := New(Config{Agent: AgentConfig{Factory: func(string) agent.Agent {
				a := &countingAgent{}
				if kind == "restore" {
					a.restore = errors.New("corrupt state")
				}
				return a
			}}})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, target.Close()) })
			_, err = target.ResumeSession(data, ResumeOptions{})
			switch kind {
			case "version", "missing":
				require.ErrorIs(t, err, agent.ErrVersion)
				require.EqualValues(t, 1, target.AgentStats().UnknownVersions)
			case "oversize":
				require.ErrorIs(t, err, agent.ErrStateTooLarge)
				require.EqualValues(t, 1, target.AgentStats().OversizedStates)
			case "restore":
				require.ErrorIs(t, err, agent.ErrRestore)
				require.EqualValues(t, 1, target.AgentStats().RestoreFailures)
			}
			require.Zero(t, target.SessionCount())
		})
	}
}

func TestAgentSlowCallbackDropsInputWithoutStallingVideo(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var first atomic.Bool
	a := &countingAgent{process: func(context.Context, agent.Input) error {
		if first.CompareAndSwap(false, true) {
			close(entered)
			<-release
		}
		return nil
	}}
	w, s, caller := agentPacketSession(t, a, 20*time.Millisecond)
	s.handleRTP(testEncrypt(t, caller, 456, 1))
	<-entered
	for seq := uint16(2); seq < 10; seq++ {
		s.handleRTP(testEncrypt(t, caller, 456, seq))
	}
	require.Eventually(t, func() bool { return w.AgentStats().CallbackDeadlines > 0 }, time.Second, time.Millisecond)
	raw, err := (&rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 96, SSRC: 789, SequenceNumber: 1}, Payload: []byte{0x10, 1}}).Marshal()
	require.NoError(t, err)
	encrypted, err := caller.EncryptRTP(nil, raw, nil)
	require.NoError(t, err)
	s.handleRTP(encrypted)
	s.mu.Lock()
	videoPackets := s.state.Video.Packets
	s.mu.Unlock()
	require.EqualValues(t, 1, videoPackets, "video continued while audio callback ignored deadline")
	require.Greater(t, w.AgentStats().InputDrops, uint64(0))
	close(release)
	require.Eventually(t, func() bool {
		state, p, err := w.SessionAgent(s.id)
		return err == nil && p.Consumed == 2 && binary.BigEndian.Uint64(state.Bytes) == 2
	}, time.Second, time.Millisecond, "late result discarded, previous state restored, bounded queued inputs continue")
	require.EqualValues(t, 1, w.AgentStats().CallbackDeadlines)
}

func TestAgentShutdownDoesNotWaitForUncooperativeCallback(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	a := &countingAgent{process: func(context.Context, agent.Input) error { close(entered); <-release; return nil }}
	w, s, caller := agentPacketSession(t, a, 10*time.Millisecond)
	s.handleRTP(testEncrypt(t, caller, 456, 1))
	<-entered
	done := make(chan error, 1)
	go func() { done <- w.Close() }()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("callback blocked shutdown")
	}
	close(release)
}

func TestAgentDefaultEchoRestoresLegacySnapshot(t *testing.T) {
	w := newTestWorker(t)
	call, _ := dialDTLSCaller(t, w)
	data, err := w.ExportSession(call.id)
	require.NoError(t, err)
	var snap snapshot
	require.NoError(t, json.Unmarshal(data, &snap))
	var legacy map[string]any
	require.NoError(t, json.Unmarshal(data, &legacy))
	legacy["Version"] = 6
	legacyState := legacy["State"].(map[string]any)
	legacyState["Version"] = 6
	delete(legacyState, "Agent")
	data, err = json.Marshal(legacy)
	require.NoError(t, err)
	_, err = w.ResumeSession(data, ResumeOptions{})
	require.NoError(t, err)
	state, _, err := w.SessionAgent(call.id)
	require.NoError(t, err)
	echoState, err := (&agent.Echo{}).Save(context.Background())
	require.NoError(t, err)
	require.Equal(t, echoState, state)
}

func TestAgentPlannedFlushDrainsAcceptedQueue(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var first atomic.Bool
	a := &countingAgent{process: func(context.Context, agent.Input) error {
		if first.CompareAndSwap(false, true) {
			close(entered)
			<-release
		}
		return nil
	}}
	w, s, caller := agentPacketSession(t, a, time.Second)
	s.handleRTP(testEncrypt(t, caller, 456, 1))
	<-entered
	s.handleRTP(testEncrypt(t, caller, 456, 2))
	s.handleRTP(testEncrypt(t, caller, 456, 3))
	flushed := make(chan error, 1)
	go func() { flushed <- s.flushAgent() }()
	require.Eventually(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.agent.paused }, time.Second, time.Millisecond)
	// Audio arriving after the export barrier is explicitly dropped, never
	// accepted into the state that the source can no longer transfer.
	s.handleRTP(testEncrypt(t, caller, 456, 4))
	close(release)
	require.NoError(t, <-flushed)
	state, p, err := w.SessionAgent(s.id)
	require.NoError(t, err)
	require.EqualValues(t, 3, binary.BigEndian.Uint64(state.Bytes))
	require.EqualValues(t, 3, p.Consumed)
	require.EqualValues(t, 3, p.Index)
	require.EqualValues(t, 1, w.AgentStats().InputDrops)
}

func TestAgentResumeNoticeCarriesDuplicateWindow(t *testing.T) {
	w := newTestWorker(t)
	call, _ := dialDTLSCaller(t, w)
	data, err := w.ExportSession(call.id)
	require.NoError(t, err)
	var snap snapshot
	require.NoError(t, json.Unmarshal(data, &snap))
	snap.State.Agent.State = agent.State{Version: 1, Bytes: make([]byte, 8)}
	data, err = json.Marshal(snap)
	require.NoError(t, err)
	notices := make(chan agent.ResumeNotice, 1)
	target, err := New(Config{Agent: AgentConfig{Factory: func(string) agent.Agent { return &noticeAgent{notices: notices} }}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, target.Close()) })
	windows := []agent.DuplicateWindow{{SSRC: 456, First: 900, Last: 925}}
	_, err = target.ResumeSession(data, ResumeOptions{Kind: agent.Takeover, CheckpointAge: 350 * time.Millisecond, SnapshotAge: 400 * time.Millisecond, InputMayBeDuplicated: true, DuplicateWindows: windows})
	require.NoError(t, err)
	notice := <-notices
	require.Equal(t, agent.Takeover, notice.Kind)
	require.Equal(t, 350*time.Millisecond, notice.CheckpointAge)
	require.Equal(t, 400*time.Millisecond, notice.SnapshotAge)
	require.True(t, notice.InputMayBeDuplicated)
	require.Equal(t, windows, notice.DuplicateWindows)
}

type noticeAgent struct {
	countingAgent
	notices chan agent.ResumeNotice
}

func (a *noticeAgent) Resume(_ context.Context, notice agent.ResumeNotice) error {
	a.notices <- notice
	return nil
}

func TestAgentInitialSaveDeadlineIsBounded(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	a := &blockedSaveAgent{entered: entered, release: release}
	w, err := New(Config{Agent: AgentConfig{Factory: func(string) agent.Agent { return a }, CallbackTimeout: 20 * time.Millisecond}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, w.Close()) })
	done := make(chan error, 1)
	go func() { _, _, err := w.CreateSession(context.Background(), testOffer(testOfferAttrs{})); done <- err }()
	<-entered
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.DeadlineExceeded)
	case <-time.After(time.Second):
		t.Fatal("initial Save ignored callback deadline")
	}
	require.Zero(t, w.SessionCount())
	require.EqualValues(t, 1, w.AgentStats().SaveFailures)
	close(release)
}

type blockedSaveAgent struct {
	countingAgent
	entered, release chan struct{}
}

func (a *blockedSaveAgent) Save(ctx context.Context) (agent.State, error) {
	close(a.entered)
	<-a.release
	return a.countingAgent.Save(ctx)
}

func TestAgentFailedEarlyExportKeepsAcceptingAudio(t *testing.T) {
	w, err := New(Config{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, w.Close()) })
	id, _, err := w.CreateSession(context.Background(), testOffer(testOfferAttrs{}))
	require.NoError(t, err)
	_, err = w.ExportSession(id)
	require.ErrorIs(t, err, ErrNotEstablished)
	s := w.session(id)
	require.NotNil(t, s)
	s.mu.Lock()
	paused := s.agent.paused
	s.mu.Unlock()
	require.False(t, paused)
}

// A custom application may consume many inputs while producing no audio. Its
// first outbound packet must retain the original unsent-stream wrap runway;
// source sequence gaps must not consume that runway before the stream starts.
func TestAgentSilentInputDoesNotSpendOutboundIndexes(t *testing.T) {
	a := &suppressedFirstAgent{}
	w, s, caller := agentPacketSession(t, a, time.Second)
	s.mu.Lock()
	s.state.Audio.InitialSeq = 32767
	s.mu.Unlock()
	s.handleRTP(testEncrypt(t, caller, 456, 1000))
	require.Eventually(t, func() bool { _, p, err := w.SessionAgent(s.id); return err == nil && p.Consumed == 1 }, time.Second, time.Millisecond)
	s.mu.Lock()
	packets := s.state.Audio.Packets
	s.mu.Unlock()
	require.Zero(t, packets)
	s.handleRTP(testEncrypt(t, caller, 456, 33763))
	require.Eventually(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.state.Audio.Packets == 1 }, time.Second, time.Millisecond)
	s.mu.Lock()
	index := s.state.Audio.HighestSentIndex
	s.mu.Unlock()
	require.EqualValues(t, 32767, index, "a silent caller gap must not move the first output to index 65530")
	s.handleRTP(testEncrypt(t, caller, 456, 33764))
	require.Eventually(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.state.Audio.Packets == 2 }, time.Second, time.Millisecond)
	s.mu.Lock()
	index = s.state.Audio.HighestSentIndex
	s.mu.Unlock()
	require.EqualValues(t, 32768, index)
}

type suppressedFirstAgent struct{ countingAgent }

func (a *suppressedFirstAgent) Process(ctx context.Context, in agent.Input) (agent.Output, error) {
	output, err := a.countingAgent.Process(ctx, in)
	if a.count == 1 {
		output.Audio = nil
	}
	return output, err
}

func TestAgentEchoDoesNotPauseOrDropAtExport(t *testing.T) {
	w, s, caller := agentPacketSession(t, &agent.Echo{}, time.Second)
	require.NoError(t, s.flushAgent())
	require.False(t, s.agent.paused)
	s.handleRTP(testEncrypt(t, caller, 456, 1))
	s.mu.Lock()
	packets := s.state.Audio.Packets
	s.mu.Unlock()
	require.EqualValues(t, 1, packets)
	require.Zero(t, w.AgentStats().InputDrops)
}

func TestAgentIdleDoesNotRequestSnapshots(t *testing.T) {
	_, s, _ := agentPacketSession(t, &countingAgent{}, time.Second)
	select {
	case <-s.snapshotWanted:
		t.Fatal("idle agent requested a checkpoint")
	case <-time.After(250 * time.Millisecond):
	}
}

func TestAgentEchoRejectsStatelessCustomState(t *testing.T) {
	require.ErrorIs(t, (&agent.Echo{}).Restore(context.Background(), agent.State{Version: 1}), agent.ErrVersion)
}

func TestAgentHTTPVersionErrorIsDeterministic(t *testing.T) {
	err := fmt.Errorf("%w: %w", agent.ErrRestore, agent.ErrVersion)
	for range 100 {
		rw := httptest.NewRecorder()
		workerAPIError(rw, err)
		var failure struct{ Code string }
		require.NoError(t, json.Unmarshal(rw.Body.Bytes(), &failure))
		require.Equal(t, "agent_version", failure.Code)
	}
}

func TestAgentMetricsSurviveSessionRemoval(t *testing.T) {
	a := &countingAgent{oversize: true}
	w, s, caller := agentPacketSession(t, a, time.Second)
	s.handleRTP(testEncrypt(t, caller, 456, 1))
	require.Eventually(t, func() bool { return w.SessionCount() == 0 }, time.Second, time.Millisecond)
	require.Contains(t, metricsBody(w), "relais_worker_agent_oversized_states_total 1\n")
	require.Contains(t, metricsBody(w), "relais_worker_agent_save_failures_total 0\n")
}
