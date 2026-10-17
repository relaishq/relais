package callharness

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/relaishq/relais/examples/voice-agent/voice"
	"github.com/relaishq/relais/internal/agenthost"
	"github.com/relaishq/relais/internal/workerprobe"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/stretchr/testify/require"
)

type liveVoiceProvider struct {
	base   voice.Provider
	ledger string
	mu     sync.Mutex
	active atomic.Bool
}

func (p *liveVoiceProvider) Available() bool { return p.base.Available() }

// Reserve a deliberately conservative upper estimate BEFORE each provider
// attempt. No bodies, keys, provider errors, or response text enter the ledger.
func (p *liveVoiceProvider) reserve(model, purpose string, cost float64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	b, err := os.ReadFile(p.ledger)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("live ledger unavailable")
	}
	total := 0.0
	for _, line := range strings.Split(string(b), "\n") {
		i := strings.LastIndex(line, "$")
		if i < 0 {
			continue
		}
		value := strings.Fields(line[i+1:])
		if len(value) > 0 {
			n, _ := strconv.ParseFloat(value[0], 64)
			total += n
		}
	}
	if total+cost >= 0.75 {
		return &voice.ProviderError{Code: "provider_refused"}
	}
	f, err := os.OpenFile(p.ledger, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		return fmt.Errorf("live ledger unavailable")
	}
	_, err = fmt.Fprintf(f, "%s | %s | %s | $%.4f\n", time.Now().UTC().Format(time.RFC3339), model, purpose, cost)
	returnError := f.Close()
	if err != nil || returnError != nil {
		return fmt.Errorf("live ledger unavailable")
	}
	return nil
}
func (p *liveVoiceProvider) Transcribe(ctx context.Context, key string, b []byte) (string, error) {
	if err := p.reserve(voice.TranscribeModel, "live turn STT attempt", 0.01); err != nil {
		return "", err
	}
	p.active.Store(true)
	defer p.active.Store(false)
	return p.base.Transcribe(ctx, key, b)
}
func (p *liveVoiceProvider) Reply(ctx context.Context, key, text string, h []voice.Exchange) (string, error) {
	if err := p.reserve(voice.ReplyModel, "live turn LLM attempt", 0.002); err != nil {
		return "", err
	}
	return p.base.Reply(ctx, key, text, h)
}
func (p *liveVoiceProvider) Speech(ctx context.Context, key, text string) (io.ReadCloser, error) {
	if err := p.reserve(voice.SpeechModel, "live turn TTS attempt", 0.03); err != nil {
		return nil, err
	}
	return p.base.Speech(ctx, key, text)
}

// TestVoiceAgentLive is local-only and never loads credentials unless explicitly
// enabled. Optional RELAIS_VOICE_LIVE_ENV names a private env file; only its
// OPENAI_API_KEY line is read, in process. RELAIS_VOICE_LEDGER is mandatory.
func TestVoiceAgentLive(t *testing.T) {
	if os.Getenv("RELAIS_VOICE_LIVE") != "1" || os.Getenv("CI") != "" {
		t.Skip("opt-in local provider test")
	}
	ledger := os.Getenv("RELAIS_VOICE_LEDGER")
	require.NotEmpty(t, ledger, "live cost ledger path required")
	key := os.Getenv("OPENAI_API_KEY")
	if file := os.Getenv("RELAIS_VOICE_LIVE_ENV"); file != "" {
		b, err := os.ReadFile(file)
		require.NoError(t, err)
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "OPENAI_API_KEY=") {
				key = strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "OPENAI_API_KEY=")), "\"'")
				break
			}
		}
	}
	require.NotEmpty(t, key, "worker key unavailable")
	provider := &liveVoiceProvider{base: voice.NewOpenAI(key), ledger: ledger}
	for _, cut := range []string{"full", "move_playback", "kill_provider"} {
		t.Run(cut, func(t *testing.T) {
			reg := exampleRegistration(voice.Registration(provider))
			limits := mediaworker.SnapshotLimits{MaxBytes: 24576, BytesPerSecond: 270336, BurstBytes: 98304}
			sys := startAgentHarness(t, nil, agentHarnessSettings{v2: []agenthost.Registration{reg, reg}, snapshotLimits: []mediaworker.SnapshotLimits{limits, limits}, remoteTarget: true, callbackBudget: 2 * time.Second})
			ctx, cancel := context.WithTimeout(context.Background(), 70*time.Second)
			defer cancel()
			call, err := sys.h.Dial(ctx, CallOptions{})
			require.NoError(t, err)
			id := call.SessionID()
			received := receiveVoice(t, call)
			voiceControl(t, sys, id, 1, "ptt.start")
			packets := pacerFixtures(t, 80)
			packets = append(packets, packets...)
			for _, packet := range packets {
				require.NoError(t, call.audio.WriteSample(media.Sample{Data: packet, Duration: 20 * time.Millisecond}))
				time.Sleep(20 * time.Millisecond)
			}
			released := time.Now()
			voiceControl(t, sys, id, 2, "ptt.stop")
			target := 0
			if cut == "kill_provider" {
				require.Eventually(t, provider.active.Load, 5*time.Second, 5*time.Millisecond)
				time.Sleep(100 * time.Millisecond)
				require.True(t, provider.active.Load(), "provider finished before kill cut")
				voiceStoreCut(t, sys.store, id, "transcribing")
				require.NoError(t, workerprobe.Kill(sys.workers[0].LocalAddr()))
				waitVoice(t, sys.workers[1], id, func(s voice.State) bool { return s.Phase == "idle" && s.Outcome == agent.OutcomeUnknown })
				t.Logf("LIVE %s release_to_notice=%s", cut, time.Since(released))
			} else {
				select {
				case <-received:
				case <-ctx.Done():
					t.Fatal("live first audio unavailable")
				}
				first := time.Since(released)
				if cut == "move_playback" {
					_, err = sys.plane.Move(ctx, id, "1")
					require.NoError(t, err)
					target = 1
				}
				state := waitVoice(t, sys.workers[target], id, func(s voice.State) bool { return s.Phase == "idle" })
				require.Equal(t, agent.OK, state.Outcome)
				require.NotEmpty(t, state.Transcript)
				require.NotEmpty(t, state.Reply)
				t.Logf("LIVE %s first_audio_after_release=%s total_after_release=%s", cut, first, time.Since(released))
				// Parse only to establish a valid text state, without logging its contents.
				b, err := json.Marshal(state)
				require.NoError(t, err)
				require.LessOrEqual(t, len(b), voice.MaxStateBytes)
			}
			_, err = call.Hangup(ctx)
			require.NoError(t, err)
		})
	}
}
