package callharness

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/relaishq/relais/examples/voice-agent/voice"
	"github.com/relaishq/relais/internal/agenthost"
	"github.com/relaishq/relais/pkg/agent/opusio"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/stretchr/testify/require"
)

type segmentLiveProvider struct {
	liveVoiceProvider
	timingMu  sync.Mutex
	released  time.Time
	stages    map[string]time.Duration
	speeches  atomic.Int64
	second    chan struct{}
	rendering atomic.Bool
}

func (p *segmentLiveProvider) mark(stage string) {
	p.timingMu.Lock()
	defer p.timingMu.Unlock()
	p.stages[stage] = time.Since(p.released)
}
func (p *segmentLiveProvider) Transcribe(ctx context.Context, key string, b []byte) (string, error) {
	s, e := p.liveVoiceProvider.Transcribe(ctx, key, b)
	if e == nil {
		p.mark("stt_done")
	}
	return s, e
}
func (p *segmentLiveProvider) Reply(ctx context.Context, key, text string, h []voice.Exchange) (string, error) {
	s, e := p.liveVoiceProvider.Reply(ctx, key, text, h)
	if e == nil {
		p.mark("llm_done")
	}
	return s, e
}
func (p *segmentLiveProvider) Speech(ctx context.Context, key, text string) (io.ReadCloser, error) {
	n := p.speeches.Add(1)
	if err := p.reserve(voice.SpeechModel, "sentence pipeline TTS attempt", 0.015); err != nil {
		return nil, err
	}
	body, err := p.base.Speech(ctx, key, text)
	if err != nil {
		return nil, err
	}
	return &segmentLiveBody{ReadCloser: body, done: func() {
		if n == 1 {
			p.mark("first_segment_done")
		}
		if n == 2 {
			p.rendering.Store(false)
		}
	}, start: func() {
		if n == 2 {
			p.rendering.Store(true)
			close(p.second)
		}
	}}, nil
}

type segmentLiveBody struct {
	io.ReadCloser
	start, done func()
	once        sync.Once
	end         sync.Once
}

func (b *segmentLiveBody) Read(p []byte) (int, error) {
	b.once.Do(b.start)
	n, e := b.ReadCloser.Read(p)
	if e == io.EOF {
		b.end.Do(b.done)
	}
	return n, e
}

// Opt-in only: credentials stay in process, fixtures/results contain audio and
// timing only, and each paid attempt reserves its budget before admission.
func TestVoiceAgentLiveSegments(t *testing.T) {
	if os.Getenv("RELAIS_VOICE_LIVE_SEGMENTS") != "1" || os.Getenv("CI") != "" {
		t.Skip("opt-in local sentence pipeline test")
	}
	ledger := os.Getenv("RELAIS_VOICE_LEDGER")
	require.NotEmpty(t, ledger)
	b, err := os.ReadFile(os.Getenv("RELAIS_VOICE_LIVE_ENV"))
	require.NoError(t, err)
	key := ""
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "OPENAI_API_KEY=") {
			key = strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "OPENAI_API_KEY=")), "\"'")
			break
		}
	}
	require.NotEmpty(t, key, "worker key unavailable")
	base := voice.NewOpenAI(key)
	fixturePath := os.Getenv("RELAIS_VOICE_LIVE_AUDIO")
	require.NotEmpty(t, fixturePath)
	fixture, err := os.ReadFile(fixturePath)
	if os.IsNotExist(err) {
		reserve := &liveVoiceProvider{base: base, ledger: ledger}
		require.NoError(t, reserve.reserve(voice.SpeechModel, "synthetic spoken question fixture", 0.015))
		body, e := base.Speech(context.Background(), "voice-question-fixture", "Explain why the sky is blue in three short sentences.")
		require.NoError(t, e)
		fixture, e = io.ReadAll(io.LimitReader(body, 128<<10))
		require.NoError(t, e)
		require.NoError(t, body.Close())
		require.NoError(t, os.WriteFile(fixturePath, fixture, 0600))
	} else {
		require.NoError(t, err)
	}
	var packets [][]byte
	reader, err := opusio.NewReader(bytes.NewReader(fixture))
	require.NoError(t, err)
	for {
		audio, e := reader.Next(context.Background())
		if e == io.EOF {
			break
		}
		require.NoError(t, e)
		packets = append(packets, audio.Packets...)
	}
	require.NotEmpty(t, packets)
	require.Less(t, len(packets), 250, "spoken question must stay below five seconds")
	for _, cut := range []string{"full", "move_segment2_playback", "move_segment2_rendering"} {
		t.Run(cut, func(t *testing.T) {
			p := &segmentLiveProvider{liveVoiceProvider: liveVoiceProvider{base: base, ledger: ledger}, stages: map[string]time.Duration{}, second: make(chan struct{})}
			reg := exampleRegistration(voice.Registration(p))
			limits := mediaworker.SnapshotLimits{MaxBytes: 24576, BytesPerSecond: 270336, BurstBytes: 98304}
			sys := startAgentHarness(t, nil, agentHarnessSettings{v2: []agenthost.Registration{reg, reg}, snapshotLimits: []mediaworker.SnapshotLimits{limits, limits}, remoteTarget: true, callbackBudget: 2 * time.Second})
			ctx, cancel := context.WithTimeout(context.Background(), 70*time.Second)
			defer cancel()
			call, e := sys.h.Dial(ctx, CallOptions{})
			require.NoError(t, e)
			id := call.SessionID()
			received := receiveVoice(t, call)
			voiceControl(t, sys, id, 1, "ptt.start")
			for _, packet := range packets {
				require.NoError(t, call.audio.WriteSample(media.Sample{Data: packet, Duration: 20 * time.Millisecond}))
				time.Sleep(20 * time.Millisecond)
			}
			p.timingMu.Lock()
			p.released = time.Now()
			p.timingMu.Unlock()
			voiceControl(t, sys, id, 2, "ptt.stop")
			target := 0
			if cut == "move_segment2_rendering" {
				select {
				case <-p.second:
				case <-ctx.Done():
					t.Fatal("second segment rendering did not start")
				}
				require.True(t, p.rendering.Load(), "rendering completed before move cut")
				_, e = sys.plane.Move(ctx, id, "1")
				require.NoError(t, e)
				target = 1
			}
			var first time.Duration
			ticker := time.NewTicker(20 * time.Millisecond)
			defer ticker.Stop()
		audio:
			for {
				select {
				case packet := <-received:
					first = packet.at.Sub(p.released)
					break audio
				case <-ticker.C:
					state, _ := voiceState(sys.workers[target], id)
					if state.Phase == "idle" && state.Outcome != agent.OK {
						t.Fatalf("live turn refused: outcome=%s error=%s reply_segments=%d", state.Outcome, state.Error, len(voice.Segments(state.Reply)))
					}
				case <-ctx.Done():
					state, _ := voiceState(sys.workers[target], id)
					t.Fatalf("live first audio deadline: phase=%s outcome=%s error=%s reply_segments=%d", state.Phase, state.Outcome, state.Error, len(voice.Segments(state.Reply)))
				}
			}
			if cut == "move_segment2_playback" {
				waitVoice(t, sys.workers[0], id, func(s voice.State) bool { return s.Phase == "playing" && s.Segment == 1 })
				_, e = sys.plane.Move(ctx, id, "1")
				require.NoError(t, e)
				target = 1
			}
			var state voice.State
			require.Eventually(t, func() bool {
				var ok bool
				state, ok = voiceState(sys.workers[target], id)
				return ok && state.Phase == "idle"
			}, 30*time.Second, 10*time.Millisecond)
			require.Equal(t, agent.OK, state.Outcome)
			require.GreaterOrEqual(t, len(voice.Segments(state.Reply)), 2)
			p.timingMu.Lock()
			t.Logf("LIVE %s stt_done=%s llm_done=%s first_segment_done=%s first_audio_received=%s turn_complete=%s segments=%d", cut, p.stages["stt_done"], p.stages["llm_done"], p.stages["first_segment_done"], first, time.Since(p.released), len(voice.Segments(state.Reply)))
			p.timingMu.Unlock()
			_, e = call.Hangup(ctx)
			require.NoError(t, e)
		})
	}
}
