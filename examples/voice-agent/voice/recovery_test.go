package voice

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	legacy "github.com/relaishq/relais/pkg/agent"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/stretchr/testify/require"
)

func TestIdleTakeoverPreservesOutcomeAndRedeliveredStartNotice(t *testing.T) {
	for _, outcome := range []agent.Outcome{agent.OK, agent.Failed} {
		t.Run(string(outcome), func(t *testing.T) {
			a := Registration(NewOpenAI("not-a-key")).Factory(agent.FactoryArgs{}).(*Agent)
			a.state = State{Turn: 9, Phase: "idle", Outcome: outcome, Error: "earlier_error", Transcript: "saved", Reply: "saved reply"}
			_, err := a.Handle(context.Background(), agent.Event{Kind: agent.Resumed, Resume: &agent.ResumeNotice{ResumeNotice: legacy.ResumeNotice{Kind: legacy.Takeover}, MoveOnly: agent.OK}})
			require.NoError(t, err)
			require.Equal(t, outcome, a.state.Outcome)
			require.Equal(t, "earlier_error", a.state.Error)
			require.NotEmpty(t, a.state.Notice)
			_, err = a.Handle(context.Background(), agent.Event{Kind: agent.Control, Control: &agent.ControlMessage{Name: "ptt.start"}})
			require.NoError(t, err)
			require.Equal(t, "recording", a.state.Phase)
			require.Contains(t, a.state.Notice, "earlier speech may be missing")
		})
	}
}
func TestLostRecordingNoticeSurvivesStart(t *testing.T) {
	a := Registration(NewOpenAI("not-a-key")).Factory(agent.FactoryArgs{}).(*Agent)
	_, err := a.Handle(context.Background(), agent.Event{Kind: agent.Resumed, Resume: &agent.ResumeNotice{ResumeNotice: legacy.ResumeNotice{Kind: legacy.Takeover}, MoveOnly: agent.OutcomeUnknown}})
	require.NoError(t, err)
	require.Equal(t, agent.OutcomeUnknown, a.state.Outcome)
	_, err = a.Handle(context.Background(), agent.Event{Kind: agent.Control, Control: &agent.ControlMessage{Name: "ptt.start"}})
	require.NoError(t, err)
	require.Contains(t, a.state.Notice, "earlier speech may be missing")
}
func TestInvalidUTF8StateHasEncodingError(t *testing.T) {
	a := Registration(NewOpenAI("")).Factory(agent.FactoryArgs{}).(*Agent)
	err := a.Restore(context.Background(), agent.State{Version: stateVersion, Bytes: []byte{0xff}})
	var invalid interface {
		error
		InvalidEncoding() bool
	}
	require.ErrorAs(t, err, &invalid)
	require.NotErrorIs(t, err, agent.ErrStateTooLarge)
}
func TestUnknownControlVisible(t *testing.T) {
	a := Registration(NewOpenAI("")).Factory(agent.FactoryArgs{}).(*Agent)
	_, err := a.Handle(context.Background(), agent.Event{Kind: agent.Control, Control: &agent.ControlMessage{Name: "private-unknown"}})
	require.NoError(t, err)
	require.Contains(t, a.state.Notice, "Unknown control")
	require.NotContains(t, a.state.Notice, "private-unknown")
}
func speechRun(t *testing.T, p Provider) error {
	t.Helper()
	request, err := json.Marshal(speechRequest{Text: "Hello.", Bytes: MaxSegmentBytes, Nanos: 8 * time.Second})
	require.NoError(t, err)
	var spec speechRequest
	require.NoError(t, json.Unmarshal(request, &spec))
	_, err = renderSpeech(context.Background(), p, "stable", spec)
	if err != nil {
		return taskFailure(&agent.TaskRun{}, err)
	}
	return nil
}
func TestSpeechReadTransportRetriedButFormatNotRetried(t *testing.T) {
	fixture, err := os.ReadFile("testdata/speech.ogg")
	require.NoError(t, err)
	for _, cut := range []string{"transport", "truncated", "format"} {
		t.Run(cut, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				require.Equal(t, "stable", r.Header.Get("Idempotency-Key"))
				if cut == "format" {
					_, _ = io.WriteString(w, "not an Ogg container that is at least 27 bytes")
					return
				}
				if cut == "transport" {
					w.Header().Set("Content-Length", "100000")
				}
				_, _ = w.Write(fixture[:len(fixture)/2])
			}))
			defer server.Close()
			p := NewOpenAI("not-a-key")
			p.origin = server.URL
			err := speechRun(t, p)
			if cut == "format" || cut == "truncated" {
				require.EqualValues(t, 1, calls.Load())
				require.EqualError(t, err, "task: failed: invalid_speech")
			} else {
				require.EqualValues(t, 2, calls.Load())
				require.EqualError(t, err, "task: outcome_unknown: provider_uncertain")
			}
		})
	}
}
func TestTaskFailureFallbackIsStageNeutral(t *testing.T) {
	err := taskFailure(&agent.TaskRun{}, errors.New("private transport text"))
	require.EqualError(t, err, "task: failed: task_failed")
}
func TestInvalidKeyIsDefiniteWithoutTransport(t *testing.T) {
	var calls atomic.Int32
	p := NewOpenAI("private\nkey")
	p.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) { calls.Add(1); return nil, errors.New("must not send") })
	attempts := 0
	err := retryOnce(context.Background(), func() error { attempts++; _, e := p.Speech(context.Background(), "stable", "Hello"); return e })
	var failure *ProviderError
	require.ErrorAs(t, err, &failure)
	require.Equal(t, "provider_configuration", failure.Code)
	require.False(t, failure.Uncertain)
	require.Equal(t, 1, attempts)
	require.Zero(t, calls.Load())
	require.NotContains(t, err.Error(), "private")
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
