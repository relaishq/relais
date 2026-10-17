package voice

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	legacy "github.com/relaishq/relais/pkg/agent"
	"github.com/relaishq/relais/pkg/agent/opusio"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/stretchr/testify/require"
)

func recording(packets int) []byte {
	b := []byte{1}
	for range packets {
		b = binary.BigEndian.AppendUint16(b, 3)
		b = append(b, 0xf8, 0xff, 0xfe)
	}
	return b
}
func TestRecordingOggShortPreskipRoundTrip(t *testing.T) {
	for _, n := range []int{1, 2, 4, 5, 100} {
		t.Run(time.Duration(n).String(), func(t *testing.T) {
			b, err := RecordingOgg(recording(n))
			require.NoError(t, err)
			again, err := RecordingOgg(recording(n))
			require.NoError(t, err)
			require.Equal(t, b, again, "retry upload must have identical bytes")
			reader, err := opusio.NewReader(bytes.NewReader(b))
			require.NoError(t, err)
			require.Zero(t, reader.Header().PreSkip)
			count := 0
			for {
				audio, err := reader.Next(context.Background())
				if err == io.EOF {
					break
				}
				require.NoError(t, err)
				require.Equal(t, []byte{0xf8, 0xff, 0xfe}, audio.Packets[0])
				count++
			}
			require.Equal(t, n, count)
			require.Equal(t, time.Duration(n)*20*time.Millisecond, reader.Position())
		})
	}
}
func TestTextBudgetAndVersion(t *testing.T) {
	text := strings.Repeat("\x00\"😀<汉", 3000)
	bounded := boundedText(text, 1024)
	require.True(t, utf8.ValidString(bounded))
	b, err := json.Marshal(bounded)
	require.NoError(t, err)
	require.LessOrEqual(t, len(b), 1026)
	a := Registration(NewOpenAI("")).Factory(agent.FactoryArgs{}).(*Agent)
	a.state.Transcript, a.state.Reply = bounded, bounded
	for range 4 {
		a.state.History = append(a.state.History, Exchange{boundedText(text, 512), boundedText(text, 512)})
	}
	saved, err := a.Save(context.Background())
	require.NoError(t, err)
	require.LessOrEqual(t, len(saved.Bytes), MaxStateBytes)
	saved.Version++
	require.ErrorIs(t, a.Restore(context.Background(), saved), agent.ErrVersion)
}
func TestMissingKeyRefusesAndRollbackKeepsRecording(t *testing.T) {
	ctx := context.Background()
	a := Registration(NewOpenAI("")).Factory(agent.FactoryArgs{}).(*Agent)
	_, err := a.Handle(ctx, agent.Event{Kind: agent.Control, Control: &agent.ControlMessage{Name: "ptt.start"}})
	require.NoError(t, err)
	require.Equal(t, "provider_unavailable", a.state.Error)
	require.Equal(t, agent.Failed, a.state.Outcome)
	require.Equal(t, "idle", a.state.Phase)
	a.available = true
	_, err = a.Handle(ctx, agent.Event{Kind: agent.Control, Control: &agent.ControlMessage{Name: "ptt.start"}})
	require.NoError(t, err)
	_, err = a.Handle(ctx, agent.Event{Kind: agent.AudioInput, Input: &legacy.Input{Payload: []byte{0xf8, 0xff, 0xfe}, Progress: legacy.Progress{SSRC: 1, Index: 1}}})
	require.NoError(t, err)
	saved, err := a.Save(ctx)
	require.NoError(t, err)
	before, err := a.SaveMoveOnly(ctx)
	require.NoError(t, err)
	effects, err := a.Handle(ctx, agent.Event{Kind: agent.Control, Control: &agent.ControlMessage{Name: "ptt.start"}})
	require.NoError(t, err)
	require.NotNil(t, effects.CancelBefore)
	require.NotNil(t, effects.ClearAudioBefore)
	require.NoError(t, a.Restore(ctx, saved))
	after, err := a.SaveMoveOnly(ctx)
	require.NoError(t, err)
	require.Equal(t, before, after)
}
func TestCrashClearsMoveLengthKeepsText(t *testing.T) {
	a := Registration(NewOpenAI("test-not-a-key")).Factory(agent.FactoryArgs{}).(*Agent)
	a.state.Phase = "synthesizing"
	a.state.Transcript = "hello"
	a.state.Reply = "hi"
	a.length = 500
	_, err := a.Handle(context.Background(), agent.Event{Kind: agent.Resumed, Resume: &agent.ResumeNotice{MoveOnly: agent.OutcomeUnknown}})
	require.NoError(t, err)
	saved, err := a.Save(context.Background())
	require.NoError(t, err)
	require.Zero(t, saved.MoveOnlyLength)
	require.False(t, saved.HasMoveOnly)
	require.Equal(t, "hello", a.state.Transcript)
	require.Equal(t, "hi", a.state.Reply)
	require.Equal(t, agent.OutcomeUnknown, a.state.Outcome)
}
func TestProviderRequestShapesAndErrorRedaction(t *testing.T) {
	fixture, err := os.ReadFile("testdata/speech.ogg")
	require.NoError(t, err)
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer test-private-value", r.Header.Get("Authorization"))
		require.Equal(t, "stable", r.Header.Get("Idempotency-Key"))
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/audio/transcriptions":
			require.NoError(t, r.ParseMultipartForm(1<<20))
			f, h, err := r.FormFile("file")
			require.NoError(t, err)
			defer f.Close()
			require.Equal(t, "recording.ogg", h.Filename)
			require.Equal(t, TranscribeModel, r.FormValue("model"))
			_, _ = io.WriteString(w, `{"text":"Hello."}`)
		case "/chat/completions":
			var b map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&b))
			require.Equal(t, ReplyModel, b["model"])
			require.Equal(t, false, b["store"])
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"Hi."}}]}`)
		case "/audio/speech":
			_, _ = w.Write(fixture)
		default:
			w.WriteHeader(500)
			_, _ = io.WriteString(w, "test-private-value")
		}
	}))
	defer server.Close()
	p := NewOpenAI("test-private-value")
	p.origin = server.URL
	text, err := p.Transcribe(context.Background(), "stable", []byte("OggS"))
	require.NoError(t, err)
	require.Equal(t, "Hello.", text)
	text, err = p.Reply(context.Background(), "stable", text, nil)
	require.NoError(t, err)
	require.Equal(t, "Hi.", text)
	body, err := p.Speech(context.Background(), "stable", text)
	require.NoError(t, err)
	b, err := readSpeech(context.Background(), body)
	require.NoError(t, err)
	require.Greater(t, len(b), 1)
	require.NoError(t, body.Close())
	_, err = p.request(context.Background(), "/error", "application/json", "stable", nil)
	require.EqualError(t, err, "provider_uncertain")
	require.NotContains(t, err.Error(), "test-private-value")
	require.Len(t, paths, 4)
}

func TestProviderUncertaintyRetriesOnce(t *testing.T) {
	calls := 0
	err := retryOnce(context.Background(), func() error { calls++; return &ProviderError{Code: "provider_uncertain", Uncertain: true} })
	require.Equal(t, 2, calls)
	var failure *ProviderError
	require.ErrorAs(t, err, &failure)
	require.True(t, failure.Uncertain)
	calls = 0
	err = retryOnce(context.Background(), func() error { calls++; return &ProviderError{Code: "provider_refused"} })
	require.Equal(t, 1, calls)
	require.EqualError(t, err, "provider_refused")
}

func TestTakeoverBeforeRecordingCheckpointIsVisible(t *testing.T) {
	a := Registration(NewOpenAI("test-not-a-key")).Factory(agent.FactoryArgs{}).(*Agent)
	// The checkpoint can still say idle even when a later local press/audio was
	// accepted before the worker died. Never infer absence of loss from idle.
	_, err := a.Handle(context.Background(), agent.Event{Kind: agent.Resumed, Resume: &agent.ResumeNotice{ResumeNotice: legacy.ResumeNotice{Kind: legacy.Takeover}}})
	require.NoError(t, err)
	require.Equal(t, agent.OutcomeUnknown, a.state.Outcome)
	require.NotEmpty(t, a.state.Notice)
	require.Equal(t, "idle", a.state.Phase)
}
