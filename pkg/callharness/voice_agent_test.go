package callharness

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"math/bits"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/relaishq/relais/examples/voice-agent/voice"
	"github.com/relaishq/relais/internal/agenthost"
	"github.com/relaishq/relais/internal/workerprobe"
	"github.com/relaishq/relais/pkg/agent/opusio"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

type voiceFixtureProvider struct {
	audio         []byte
	cut           string
	entered       chan struct{}
	stt, llm, tts atomic.Int64
	recorded      atomic.Int64
	oggHash       atomic.Value
}

func (*voiceFixtureProvider) Available() bool      { return true }
func (*voiceFixtureProvider) Warm(context.Context) {}
func (p *voiceFixtureProvider) pause(ctx context.Context, phase string) error {
	if p.cut != phase {
		return nil
	}
	select {
	case p.entered <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return ctx.Err()
}
func (p *voiceFixtureProvider) Transcribe(ctx context.Context, _ string, b []byte) (string, error) {
	p.stt.Add(1)
	p.oggHash.Store(sha256.Sum256(b))
	reader, err := opusio.NewReader(bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	for {
		_, err = reader.Next(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		p.recorded.Add(1)
	}
	if err := p.pause(ctx, "transcribing"); err != nil {
		return "", err
	}
	return "Say hello.", nil
}
func (p *voiceFixtureProvider) Reply(ctx context.Context, _, _ string, _ []voice.Exchange) (string, error) {
	p.llm.Add(1)
	if err := p.pause(ctx, "replying"); err != nil {
		return "", err
	}
	return "Hello from Relais.", nil
}
func (p *voiceFixtureProvider) Speech(ctx context.Context, _, _ string) (io.ReadCloser, error) {
	p.tts.Add(1)
	if err := p.pause(ctx, "synthesizing"); err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(p.audio)), nil
}
func voiceState(w *mediaworker.Worker, id string) (voice.State, bool) {
	saved, _, err := w.SessionAgent(id)
	if err != nil {
		return voice.State{}, false
	}
	var s voice.State
	err = json.Unmarshal(saved.Bytes, &s)
	return s, err == nil
}
func waitVoice(t *testing.T, w *mediaworker.Worker, id string, predicate func(voice.State) bool) voice.State {
	t.Helper()
	var s voice.State
	require.Eventually(t, func() bool { var ok bool; s, ok = voiceState(w, id); return ok && predicate(s) }, 8*time.Second, 5*time.Millisecond)
	t.Logf("voice state: phase=%s outcome=%s task=%s cursor=%d", s.Phase, s.Outcome, s.Task, s.Cursor)
	return s
}
func voiceControl(t *testing.T, sys *agentHarnessSystem, id string, sequence uint64, name string) {
	t.Helper()
	waitControlCheckpoint(t, sys, id, agent.ControlMessage{ID: name + "-" + time.Duration(sequence).String(), Sequence: sequence, Name: name})
}
func voiceSystem(t *testing.T, store sessionstore.Store, cut string) (*agentHarnessSystem, [2]*voiceFixtureProvider) {
	t.Helper()
	audio, err := os.ReadFile("../../examples/voice-agent/voice/testdata/speech.ogg")
	require.NoError(t, err)
	var providers [2]*voiceFixtureProvider
	regs := make([]agenthost.Registration, 2)
	for i := range regs {
		providers[i] = &voiceFixtureProvider{audio: audio, entered: make(chan struct{}, 2)}
		if i == 0 {
			providers[i].cut = cut
		}
		regs[i] = exampleRegistration(voice.Registration(providers[i]))
	}
	limit := mediaworker.SnapshotLimits{MaxBytes: 24576, BytesPerSecond: 270336, BurstBytes: 98304}
	return startAgentHarness(t, store, agentHarnessSettings{v2: regs, snapshotLimits: []mediaworker.SnapshotLimits{limit, limit}, remoteTarget: true, callbackBudget: 2 * time.Second}), providers
}
func voiceStoreCut(t *testing.T, store sessionstore.Store, id, phase string) {
	t.Helper()
	require.Eventually(t, func() bool {
		b, err := store.GetState(context.Background(), id)
		if err != nil {
			return false
		}
		var snapshot v2HarnessSnapshot
		if json.Unmarshal(b, &snapshot) != nil {
			return false
		}
		var s voice.State
		return json.Unmarshal(snapshot.State.Agent.State.Bytes, &s) == nil && s.Phase == phase
	}, 3*time.Second, 5*time.Millisecond)
}

// Full-turn cut tests use actual routed Control, the production example's
// callbacks/tasks, fixture Ogg and the network pacer on memory and Redis. They
// deliberately add no work to the short harness.
func TestVoiceAgentMoveCuts(t *testing.T) {
	if testing.Short() {
		t.Skip("voice provider/playback matrix")
	}
	for _, cut := range []string{"none", "recording", "transcribing", "replying", "synthesizing", "playing"} {
		t.Run(cut, func(t *testing.T) {
			forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
				sys, p := voiceSystem(t, store, cut)
				ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
				defer cancel()
				call, err := sys.h.Dial(ctx, CallOptions{})
				require.NoError(t, err)
				id := call.SessionID()
				received := receiveVoice(t, call)
				voiceControl(t, sys, id, 1, "ptt.start")
				sendLoopPackets(t, call, pacerFixtures(t, 5))
				waitVoice(t, sys.workers[0], id, func(s voice.State) bool {
					return bits.OnesCount64(s.Seen.Mask[0])+bits.OnesCount64(s.Seen.Mask[1]) >= 5
				})
				source := 0
				if cut == "recording" {
					_, err = sys.plane.Move(ctx, id, "1")
					require.NoError(t, err)
					source = 1
					sendLoopPackets(t, call, pacerFixtures(t, 5))
					waitVoice(t, sys.workers[1], id, func(s voice.State) bool {
						return bits.OnesCount64(s.Seen.Mask[0])+bits.OnesCount64(s.Seen.Mask[1]) >= 10
					})
				}
				voiceControl(t, sys, id, 2, "ptt.stop")
				var prefix []pacedPacket
				if cut == "transcribing" || cut == "replying" || cut == "synthesizing" {
					select {
					case <-p[0].entered:
					case <-ctx.Done():
						t.Fatal("provider cut not reached")
					}
				}
				if cut == "playing" {
					prefix = append(prefix, nextPaced(t, received))
				}
				var beforeMove voice.Timings
				if cut != "none" && cut != "recording" {
					prior, _ := voiceState(sys.workers[0], id)
					beforeMove = prior.Timings
					_, err = sys.plane.Move(ctx, id, "1")
					require.NoError(t, err)
					source = 1
				}
				want := pacerFixtures(t, 80)
				got := append(prefix, collectPackets(t, received, len(want)-len(prefix), 8*time.Second)...)
				for i := range want {
					require.Equal(t, want[i], got[i].packet.Payload, "packet %d", i)
				}
				s := waitVoice(t, sys.workers[source], id, func(s voice.State) bool { return s.Phase == "idle" })
				require.Equal(t, agent.OK, s.Outcome)
				require.Equal(t, "Say hello.", s.Transcript)
				require.Equal(t, "Hello from Relais.", s.Reply)
				require.Positive(t, s.ReleaseAt)
				require.Positive(t, s.Timings.TranscribeFinished)
				require.Positive(t, s.Timings.ReplyFinished)
				require.Positive(t, s.Timings.SpeechFinished)
				require.Positive(t, s.Timings.FirstQueued)
				require.GreaterOrEqual(t, s.Timings.ReplyStarted, s.Timings.TranscribeFinished)
				require.GreaterOrEqual(t, s.Timings.ReplyFinished, s.Timings.ReplyStarted)
				require.GreaterOrEqual(t, s.Timings.SpeechStarted, s.Timings.ReplyFinished)
				require.GreaterOrEqual(t, s.Timings.SpeechFinished, s.Timings.SpeechStarted)
				require.GreaterOrEqual(t, s.Timings.FirstQueued, s.Timings.SpeechFinished)
				require.EqualValues(t, 1600*time.Millisecond, s.Timings.SpeechAudio)
				require.Equal(t, len("Hello from Relais."), s.Timings.ReplyCharacters)
				require.Equal(t, len("Hello from Relais."), s.Timings.FirstSentenceCharacters)
				if beforeMove.TranscribeFinished > 0 {
					require.Equal(t, beforeMove.TranscribeFinished, s.Timings.TranscribeFinished)
				}
				if beforeMove.ReplyFinished > 0 {
					require.Equal(t, beforeMove.ReplyStarted, s.Timings.ReplyStarted)
					require.Equal(t, beforeMove.ReplyFinished, s.Timings.ReplyFinished)
				}
				if beforeMove.SpeechFinished > 0 {
					require.Equal(t, beforeMove.SpeechStarted, s.Timings.SpeechStarted)
					require.Equal(t, beforeMove.SpeechFinished, s.Timings.SpeechFinished)
				}
				if beforeMove.FirstQueued > 0 {
					require.Equal(t, beforeMove.FirstQueued, s.Timings.FirstQueued)
				}
				require.Len(t, s.History, 1)
				if cut == "transcribing" {
					require.Equal(t, p[0].oggHash.Load(), p[1].oggHash.Load(), "exact move must retain the same STT upload bytes")
				}
				if cut == "recording" {
					require.EqualValues(t, 10, p[0].recorded.Load()+p[1].recorded.Load())
				}
				stt, llm, tts := p[0].stt.Load()+p[1].stt.Load(), p[0].llm.Load()+p[1].llm.Load(), p[0].tts.Load()+p[1].tts.Load()
				require.EqualValues(t, 1+boolInt(cut == "transcribing"), stt)
				require.EqualValues(t, 1+boolInt(cut == "replying"), llm)
				require.EqualValues(t, 1+boolInt(cut == "synthesizing"), tts)
				_, err = call.Hangup(ctx)
				require.NoError(t, err)
			})
		})
	}
}
func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
func TestVoiceAgentTakeoverCuts(t *testing.T) {
	if testing.Short() {
		t.Skip("voice takeover matrix")
	}
	for _, cut := range []string{"recording", "transcribing", "replying", "synthesizing", "playing"} {
		t.Run(cut, func(t *testing.T) {
			forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
				sys, p := voiceSystem(t, store, cut)
				ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
				defer cancel()
				call, err := sys.h.Dial(ctx, CallOptions{})
				require.NoError(t, err)
				id := call.SessionID()
				received := receiveVoice(t, call)
				voiceControl(t, sys, id, 1, "ptt.start")
				sendLoopPackets(t, call, pacerFixtures(t, 5))
				waitVoice(t, sys.workers[0], id, func(s voice.State) bool {
					return bits.OnesCount64(s.Seen.Mask[0])+bits.OnesCount64(s.Seen.Mask[1]) >= 5
				})
				if cut != "recording" {
					voiceControl(t, sys, id, 2, "ptt.stop")
				}
				if cut == "playing" {
					nextPaced(t, received)
				} else if cut != "recording" {
					select {
					case <-p[0].entered:
					case <-ctx.Done():
						t.Fatal("provider cut not reached")
					}
				}
				voiceStoreCut(t, store, id, cut)
				require.NoError(t, workerprobe.Kill(sys.workers[0].LocalAddr()))
				state := waitVoice(t, sys.workers[1], id, func(s voice.State) bool { return s.Phase == "idle" && s.Outcome == agent.OutcomeUnknown })
				require.Equal(t, "turn_interrupted", state.Error)
				require.NotEmpty(t, state.Notice)
				if cut == "replying" || cut == "synthesizing" || cut == "playing" {
					require.Equal(t, "Say hello.", state.Transcript)
				}
				if cut == "synthesizing" || cut == "playing" {
					require.Equal(t, "Hello from Relais.", state.Reply)
				}
				saved, _, err := sys.workers[1].SessionAgent(id)
				require.NoError(t, err)
				require.NotEmpty(t, saved.Bytes)
				require.Zero(t, p[1].stt.Load()+p[1].llm.Load()+p[1].tts.Load(), "crash must not regenerate lost audio or repay for saved text")
				next := uint64(3)
				if cut == "recording" {
					next = 2
				}
				voiceControl(t, sys, id, next, "ptt.start")
				waitVoice(t, sys.workers[1], id, func(s voice.State) bool { return s.Phase == "recording" })
				_, err = call.Hangup(ctx)
				require.NoError(t, err)
			})
		})
	}
}
func TestVoiceAgentBargeIn(t *testing.T) {
	if testing.Short() {
		t.Skip("voice network barge-in")
	}
	forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
		sys, _ := voiceSystem(t, store, "")
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		call, err := sys.h.Dial(ctx, CallOptions{})
		require.NoError(t, err)
		received := receiveVoice(t, call)
		id := call.SessionID()
		voiceControl(t, sys, id, 1, "ptt.start")
		sendLoopPackets(t, call, pacerFixtures(t, 5))
		waitVoice(t, sys.workers[0], id, func(s voice.State) bool {
			return bits.OnesCount64(s.Seen.Mask[0])+bits.OnesCount64(s.Seen.Mask[1]) >= 5
		})
		voiceControl(t, sys, id, 2, "ptt.stop")
		nextPaced(t, received)
		previous, ok := voiceState(sys.workers[0], id)
		require.True(t, ok)
		voiceControl(t, sys, id, 3, "ptt.start")
		s := waitVoice(t, sys.workers[0], id, func(s voice.State) bool { return s.Phase == "recording" && s.Turn > previous.Turn })
		require.Empty(t, s.Reply)
		// Wait past in-flight network media, then require the old turn to stay silent.
		time.Sleep(150 * time.Millisecond)
		for len(received) > 0 {
			<-received
		}
		select {
		case <-received:
			t.Fatal("old turn played after barge-in")
		case <-time.After(150 * time.Millisecond):
		}
		_, err = call.Hangup(ctx)
		require.NoError(t, err)
	})
}

// Cancellation-aware receiver: a failed assertion must not wedge cleanup behind
// a full test observation channel. This never changes production audio pacing.
func receiveVoice(t *testing.T, call *Call) chan pacedPacket {
	received := make(chan pacedPacket, 2048)
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	call.pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		if track.Kind() != webrtc.RTPCodecTypeAudio {
			return
		}
		call.startReader(func() {
			for {
				packet, _, err := track.ReadRTP()
				if err != nil {
					return
				}
				select {
				case received <- pacedPacket{packet, time.Now()}:
				case <-done:
					return
				}
			}
		})
	})
	return received
}
func TestVoiceAgentMissingKey(t *testing.T) {
	if testing.Short() {
		t.Skip("voice network refusal")
	}
	forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
		reg := exampleRegistration(voice.Registration(voice.NewOpenAI("")))
		limits := mediaworker.SnapshotLimits{MaxBytes: 24576, BytesPerSecond: 270336, BurstBytes: 98304}
		sys := startAgentHarness(t, store, agentHarnessSettings{v2: []agenthost.Registration{reg, reg}, snapshotLimits: []mediaworker.SnapshotLimits{limits, limits}, callbackBudget: 2 * time.Second})
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		call, err := sys.h.Dial(ctx, CallOptions{})
		require.NoError(t, err)
		voiceControl(t, sys, call.SessionID(), 1, "ptt.start")
		s := waitVoice(t, sys.workers[0], call.SessionID(), func(s voice.State) bool { return s.Outcome == agent.Failed })
		require.Equal(t, "provider_unavailable", s.Error)
		require.Equal(t, "idle", s.Phase)
		require.Equal(t, 1, sys.workers[0].SessionCount())
		_, err = call.Hangup(ctx)
		require.NoError(t, err)
	})
}
