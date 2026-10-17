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

func TestSentenceSegmentsDeterministicAndBounded(t *testing.T) {
	for _, input := range []string{
		"Hi. Here is the next sentence with enough words! Third sentence is also long enough?",
		strings.Repeat("😀汉", 512),
		strings.Repeat("a ", 512),
		strings.Repeat("A sufficiently long sentence. ", 40),
		strings.Repeat("aaaa bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb ", 20),
		"Hello。第二个句子使用中文而且要有足够的字符才能独立成为片段！Third.",
		"", "\xffinvalid UTF-8. Still readable.",
	} {
		reply := boundedText(input, 512)
		segments := Segments(reply)
		require.Equal(t, segments, Segments(reply))
		require.LessOrEqual(t, len(segments), MaxSegments)
		for _, segment := range segments {
			require.True(t, utf8.ValidString(segment))
			require.LessOrEqual(t, utf8.RuneCountInString(segment), MaxSegmentRunes)
			require.NotEmpty(t, segment)
		}
		// Only inter-segment whitespace may change; no saved text can disappear.
		normalize := func(s string) string { return strings.Join(strings.Fields(s), "") }
		require.Equal(t, normalize(reply), normalize(strings.Join(segments, "")))
	}
	require.Equal(t, []string{"Hi. Here is a sufficiently long next sentence.", "Another sufficiently long sentence."}, Segments("Hi. Here is a sufficiently long next sentence. Another sufficiently long sentence."))
}

func TestSegmentAudioBoundsRefuseBeforePlayback(t *testing.T) {
	for _, cut := range []string{"total_bytes", "total_time", "segment_time"} {
		t.Run(cut, func(t *testing.T) {
			a := Registration(NewOpenAI("test-not-a-key")).Factory(agent.FactoryArgs{}).(*Agent)
			a.state = State{Turn: 9, Phase: "synthesizing", Reply: "Here is a sufficiently long sentence."}
			data := recording(100)
			switch cut {
			case "total_bytes":
				a.state.SpeechBytes = MaxSpeechBytes - 10
			case "total_time":
				a.state.SpeechNanos = SpeechTime - time.Second
			case "segment_time":
				data = recording(450)
			}
			effects, err := a.Handle(context.Background(), agent.Event{Kind: agent.TaskMessage, Result: &agent.TaskResult{Turn: 10, TaskID: "speech", Sequence: 1, Chunk: agent.Chunk{Data: data}}})
			require.NoError(t, err)
			require.Equal(t, agent.Failed, a.state.Outcome)
			require.Equal(t, "invalid_speech", a.state.Error)
			require.NotNil(t, effects.ClearAudioBefore)
			require.Empty(t, effects.Audio)
			require.Empty(t, effects.StartTasks)
			saved, err := a.Save(context.Background())
			require.NoError(t, err)
			require.False(t, saved.HasMoveOnly)
		})
	}
}

func TestRestoreRefusesOversizedPendingSegment(t *testing.T) {
	a := Registration(NewOpenAI("test-not-a-key")).Factory(agent.FactoryArgs{}).(*Agent)
	a.state = State{Turn: 9, Phase: "synthesizing", Reply: "A sufficiently long spoken sentence."}
	saved, err := a.Save(context.Background())
	require.NoError(t, err)
	saved.HasMoveOnly, saved.MoveOnlyLength = true, MaxSegmentBytes+1
	require.Error(t, a.Restore(context.Background(), saved), "restore must retain the per-segment bound")
}
