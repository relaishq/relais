package voice

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/stretchr/testify/require"
)

func TestWarmModesUseOnlyBoundedMetadataRequests(t *testing.T) {
	for _, mode := range []string{"", "auth", "none"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("RELAIS_VOICE_WARM", mode)
			const key = "warm-stub-secret"
			type request struct{ method, path, authorization, idempotency string }
			requests := make(chan request, 4)
			release := make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unblock()
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests <- request{r.Method, r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Idempotency-Key")}
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				status := http.StatusOK
				if r.URL.Path == "/models/"+ReplyModel {
					status = http.StatusServiceUnavailable
				}
				w.WriteHeader(status)
				_, _ = io.WriteString(w, strings.Repeat("body-that-must-not-be-logged", 1024))
			}))
			server.EnableHTTP2 = true
			server.StartTLS()
			defer server.Close()
			client := server.Client()
			transport := client.Transport
			var bodiesMu sync.Mutex
			var bodies []*countedWarmBody
			client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				response, err := transport.RoundTrip(r)
				if err != nil {
					return response, err
				}
				body := &countedWarmBody{ReadCloser: response.Body}
				response.Body = body
				bodiesMu.Lock()
				bodies = append(bodies, body)
				bodiesMu.Unlock()
				return response, nil
			})
			p := NewOpenAIWithClient(key, client)
			p.origin = server.URL
			var traceMu sync.Mutex
			var traces []RequestTrace
			p.Trace = func(row RequestTrace) { traceMu.Lock(); defer traceMu.Unlock(); traces = append(traces, row) }
			var logs bytes.Buffer
			oldLog := log.Writer()
			log.SetOutput(&logs)
			defer log.SetOutput(oldLog)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			done := make(chan struct{})
			go func() { p.Warm(requestContext(ctx, 10, "warm")); close(done) }()
			var received []request
			expected := 3
			if mode == "none" {
				expected = 0
			}
			for range expected {
				select {
				case r := <-requests:
					received = append(received, r)
				case <-ctx.Done():
					unblock()
					t.Fatal("warm requests did not start concurrently")
				}
			}
			unblock()
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal("warm did not finish")
			}
			require.Empty(t, requests, "metadata failures must not retry")
			require.Len(t, traces, expected)
			if mode != "none" {
				paths := []string{}
				for _, r := range received {
					require.Equal(t, http.MethodGet, r.method)
					require.Equal(t, "Bearer "+key, r.authorization)
					require.Empty(t, r.idempotency)
					paths = append(paths, r.path)
				}
				require.ElementsMatch(t, []string{"/models/" + TranscribeModel, "/models/" + ReplyModel, "/models/" + SpeechModel}, paths)
				stages := []string{}
				for _, r := range traces {
					stages = append(stages, r.Stage)
					require.EqualValues(t, 10, r.Turn)
					require.Equal(t, "h2", r.Protocol)
				}
				require.ElementsMatch(t, []string{"warm-transcribe", "warm-reply", "warm-speech"}, stages)
			}
			bodiesMu.Lock()
			defer bodiesMu.Unlock()
			require.Len(t, bodies, expected)
			for _, b := range bodies {
				require.LessOrEqual(t, b.read.Load(), int64(8<<10))
				require.True(t, b.closed.Load())
				require.EqualValues(t, 8<<10, b.read.Load())
			}
			require.NotContains(t, logs.String(), key)
			require.NotContains(t, logs.String(), "body-that-must-not-be-logged")
			require.NotContains(t, logs.String(), server.URL)
		})
	}
}

type countedWarmBody struct {
	io.ReadCloser
	read   atomic.Int64
	closed atomic.Bool
}

func (b *countedWarmBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.read.Add(int64(n))
	return n, err
}
func (b *countedWarmBody) Close() error { b.closed.Store(true); return b.ReadCloser.Close() }

func TestAuthenticatedWarmNeverFollowsRedirects(t *testing.T) {
	t.Setenv("RELAIS_VOICE_WARM", "auth")
	var redirected atomic.Int32
	other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1) }))
	defer other.Close()
	var calls atomic.Int32
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		require.Equal(t, "Bearer redirect-stub-secret", r.Header.Get("Authorization"))
		http.Redirect(w, r, other.URL+"/must-not-receive-credentials", http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	p := NewOpenAIWithClient("redirect-stub-secret", origin.Client())
	p.origin = origin.URL
	var mu sync.Mutex
	var traces []RequestTrace
	p.Trace = func(row RequestTrace) { mu.Lock(); defer mu.Unlock(); traces = append(traces, row) }
	var logs bytes.Buffer
	oldLog := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(oldLog)
	p.Warm(context.Background())
	require.Len(t, traces, 3)
	for _, row := range traces {
		require.Equal(t, http.StatusTemporaryRedirect, row.Status)
	}
	require.EqualValues(t, 3, calls.Load())
	require.Zero(t, redirected.Load())
	require.NotContains(t, logs.String(), "redirect-stub-secret")
	require.NotContains(t, logs.String(), other.URL)
}

func TestWarmModeDoesNotAlterStateOrRegistration(t *testing.T) {
	var baseline []byte
	var kinds []string
	for _, mode := range []string{"", "auth", "none"} {
		t.Setenv("RELAIS_VOICE_WARM", mode)
		provider := NewOpenAI("fixture")
		expected := mode
		if expected == "" {
			expected = "auth"
		}
		require.Equal(t, expected, provider.WarmMode())
		reg := Registration(provider)
		require.Equal(t, Name, reg.Name)
		require.Equal(t, MaxStateBytes, reg.Limits.MaxStateBytes)
		got := []string{}
		for kind := range reg.Tasks {
			got = append(got, kind)
		}
		slices.Sort(got)
		a := reg.Factory(agent.FactoryArgs{}).(*Agent)
		saved, err := a.Save(context.Background())
		require.NoError(t, err)
		if baseline == nil {
			baseline = saved.Bytes
			kinds = got
		} else {
			require.Equal(t, baseline, saved.Bytes)
			require.Equal(t, kinds, got)
		}
		effects, err := a.Handle(context.Background(), agent.Event{Kind: agent.Control, Control: &agent.ControlMessage{Name: "ptt.start"}})
		require.NoError(t, err)
		require.Len(t, effects.StartTasks, 1)
		warm := effects.StartTasks[0]
		require.Equal(t, WarmKind, warm.Kind)
		require.Equal(t, 3*time.Second, warm.Timeout)
		a.buffer = recording(1)
		a.length = len(a.buffer)
		effects, err = a.Handle(context.Background(), agent.Event{Kind: agent.Control, Control: &agent.ControlMessage{Name: "ptt.stop"}})
		require.NoError(t, err)
		require.NotNil(t, effects.CancelBefore)
		require.Greater(t, *effects.CancelBefore, warm.Turn)
	}
}

func TestWarmCancellationStopsAllConcurrentRequests(t *testing.T) {
	t.Setenv("RELAIS_VOICE_WARM", "auth")
	entered := make(chan struct{}, 3)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { entered <- struct{}{}; <-r.Context().Done() }))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	p := NewOpenAIWithClient("fixture", server.Client())
	p.origin = server.URL
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { p.Warm(ctx); close(done) }()
	for range 3 {
		select {
		case <-entered:
		case <-time.After(time.Second):
			cancel()
			t.Fatal("concurrent warm start deadline")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("warm did not honor cancellation")
	}
}
