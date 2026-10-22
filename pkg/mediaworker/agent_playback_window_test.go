package mediaworker

import (
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/pion/srtp/v3"

	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/stretchr/testify/require"
)

// Probe: a streaming TTS that emits one Opus packet per Emit. Each result is a
// separate span in the persisted window. Does an ordinary reply overflow the
// 2 KiB host metadata budget and end the call?
func streamingSession(t *testing.T, packet []byte, n int) (*session, *v2UnitAgent, agent.TaskID) {
	a := &v2UnitAgent{state: agent.State{Version: 1}}
	_, s := v2UnitSession(t, a)
	require.NoError(t, s.initAgent(false, ResumeOptions{}))
	id := deriveTaskID(s.id, "unit/v1", "started/v2-unit", "tts")
	spec := agent.TaskSpec{Step: "tts", Kind: "tts/v1", Turn: 1, Class: agent.ReadOnly, Resume: agent.Retry}
	intent := taskIntent{ID: id, Step: spec.Step, Kind: spec.Kind, Turn: 1, Class: agent.ReadOnly, Resume: agent.Retry, Phase: taskStarted, Digest: taskDigest(spec)}
	task := &localTask{intent: intent, terminalBytes: taskTerminalBytes(intent), launched: true}
	s.agent.v2.tasks = []*localTask{task}
	for seq := 1; seq <= n; seq++ {
		r := agent.TaskResult{TaskID: id, Sequence: uint64(seq), Turn: 1, Outcome: agent.OK, Chunk: agent.Chunk{Audio: &agent.EncodedAudio{Format: "audio/opus", Packets: [][]byte{packet}}}}
		task.results = append(task.results, r)
		task.mailboxBytes += taskResultBytes(r)
		task.intent.Published = uint64(seq)
		require.NoError(t, s.registerAudioLocked(r))
	}
	s.publishTaskIntents(s.agent.latest.V2)
	s.publishAgent()
	return s, a, id
}

// Probe: cost of pruneTasks under the media lock with a long buffered reply.
func TestPlaybackPruneCost(t *testing.T) {
	s, _, id := streamingSession(t, []byte{0xf8, 0xff, 0xfe}, 1)
	task := s.agent.v2.tasks[0]
	task.results = nil
	for seq := 1; seq <= 760; seq++ {
		r := agent.TaskResult{TaskID: id, Sequence: uint64(seq), Turn: 1, Outcome: agent.OK, Chunk: agent.Chunk{Audio: &agent.EncodedAudio{Format: "audio/opus", Packets: [][]byte{{0xf8}, {0xf8}, {0xf8}, {0xf8}, {0xf8}}}}}
		s.agent.v2.playback.payloads[audioKey{id, uint64(seq)}] = playbackPayload{r.Chunk.Audio, make([]time.Duration, 5)}
		task.audio = append(task.audio, r)
		for o := range 5 {
			s.agent.v2.playback.queue = append(s.agent.v2.playback.queue, agent.Audio{Turn: 1, Source: agent.AudioRef{TaskID: id, Sequence: uint64(seq), PacketOffset: uint32(o)}})
		}
	}
	task.intent.Cursor = 760
	task.intent.Published = 760
	s.publishTaskIntents(s.agent.latest.V2)
	s.publishAgent()
	require.NoError(t, s.checkpoint.restore(checkpointRestore{pair: &s.state.Agent}))
	start := time.Now()
	for range 10 {
		s.pruneTasks()
	}
	elapsed := time.Since(start) / 10
	t.Logf("pruneTasks with 760 live results / 3800 queued refs: %v per call", elapsed)
	require.Less(t, elapsed, 8*time.Millisecond, "prune must not scan every reference for every retained result")
}

func TestPlaybackIdleSleepsUntilWake(t *testing.T) {
	a := &v2UnitAgent{state: agent.State{Version: 1}}
	_, s := v2UnitSession(t, a)
	require.NoError(t, s.initAgent(false, ResumeOptions{}))
	delay, err := s.paceOne()
	require.NoError(t, err)
	require.GreaterOrEqual(t, delay, time.Hour)
	s.agent.paused = true
	delay, err = s.paceOne()
	require.NoError(t, err)
	require.GreaterOrEqual(t, delay, time.Hour)
}
func TestPlaybackOutputClockDoesNotAccumulateWakeDelay(t *testing.T) {
	s, _, r := audioUnitSession(t, 80)
	installUnitPlayback(t, s, r)
	profile := srtp.ProtectionProfileAeadAes128Gcm
	keys := testSessionKeys(t, profile)
	s.srtpOut = testContext(t, keys.LocalMasterKey, keys.LocalMasterSalt, profile)
	sink := listenTestUDP(t)
	s.state.ICE.RemoteAddr = sink.LocalAddr().(*net.UDPAddr).AddrPort()
	base := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	s.agent.v2.playback.nextAt = base
	s.agent.v2.playback.initialized = true
	for range 80 {
		s.storeClock = s.agent.v2.playback.nextAt.Add(2 * time.Millisecond)
		s.storeClockAt = time.Now()
		_, err := s.paceOne()
		require.NoError(t, err)
	}
	require.InDelta(t, float64(1600*time.Millisecond), float64(s.agent.v2.playback.nextAt.Sub(base)), float64(time.Millisecond), "small wake delay must not accumulate into speech drift")
}
func TestPlaybackRestoreAdvancesRTPByStoreAge(t *testing.T) {
	a := &v2UnitAgent{state: agent.State{Version: 1}}
	_, s := v2UnitSession(t, a)
	require.NoError(t, s.initAgent(false, ResumeOptions{}))
	base := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	s.agent.latest.V2.Playback = playbackState{Initialized: true, NextAt: base, NextTS: 1234}
	s.storeClock = base.Add(time.Second)
	s.storeClockAt = time.Now()
	require.NoError(t, s.restorePlayback(nil))
	require.InDelta(t, float64(49234), float64(s.agent.v2.playback.nextTS), 48)
}

func TestPlaybackCrashKeepsFinishedNotification(t *testing.T) {
	a := &v2UnitAgent{state: agent.State{Version: 1}}
	_, source := v2UnitSession(t, a)
	require.NoError(t, source.initAgent(false, ResumeOptions{}))
	turn := agent.Turn(7)
	a.effects = agent.Effects{NotifyPlaybackFinished: &turn}
	_, err := (acceptedEvents{source}).submit(agent.Event{Kind: agent.Started}, eventLive)
	require.NoError(t, err)
	raw, err := json.Marshal(source.state)
	require.NoError(t, err)
	var state sessionState
	require.NoError(t, json.Unmarshal(raw, &state))
	b := &v2UnitAgent{state: agent.State{Version: 1}}
	_, target := v2UnitSessionFromState(t, b, state)
	require.NoError(t, target.initAgent(true, ResumeOptions{}))
	require.Equal(t, []agent.Turn{7}, target.state.Agent.V2.Playback.Notify)
	require.NoError(t, target.deliverClockEvent())
	require.Equal(t, agent.PlaybackFinished, b.lastEvent.Kind)
	require.EqualValues(t, 7, b.lastEvent.Playback.Turn)
	require.Empty(t, target.state.Agent.V2.Playback.Notify)
}
