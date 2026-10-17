package voice

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/stretchr/testify/require"
)

func TestStartPreconnectsWithoutCredentialsAndReusesTLS(t *testing.T) {
	var handshakes atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			require.Equal(t, "/models", r.URL.Path)
			require.Empty(t, r.Header.Get("Authorization"))
			require.Empty(t, r.Header.Get("Idempotency-Key"))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "Bearer not-a-key", r.Header.Get("Authorization"))
		_, _ = io.WriteString(w, `{"text":"hello"}`)
	}))
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS12, GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) { handshakes.Add(1); return nil, nil }}
	server.StartTLS()
	defer server.Close()
	p := NewOpenAI("not-a-key")
	p.origin = server.URL
	p.client = server.Client()
	a := Registration(p).Factory(agent.FactoryArgs{}).(*Agent)
	effects, err := a.Handle(context.Background(), agent.Event{Kind: agent.Control, Control: &agent.ControlMessage{Name: "ptt.start"}})
	require.NoError(t, err)
	require.Len(t, effects.StartTasks, 1, "start should warm TLS while the user is speaking")
	spec := effects.StartTasks[0]
	require.Equal(t, "voice/warm/v1", spec.Kind)
	require.Equal(t, agent.ReadOnly, spec.Class)
	require.Empty(t, spec.Request)
	handler := handlers(p)[spec.Kind]
	require.NotNil(t, handler)
	run := &agent.TaskRun{}
	at := time.Now()
	require.NoError(t, handler(context.Background(), run))
	cold := time.Since(at)
	var reused bool
	var mu sync.Mutex
	ctx := httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { mu.Lock(); reused = info.Reused; mu.Unlock() }})
	at = time.Now()
	_, err = p.Transcribe(ctx, "stable", []byte("OggS"))
	require.NoError(t, err)
	warm := time.Since(at)
	mu.Lock()
	require.True(t, reused)
	mu.Unlock()
	require.EqualValues(t, 1, handshakes.Load())
	t.Logf("local TLS stub cold preconnect=%s reused POST=%s handshakes=%d", cold, warm, handshakes.Load())
	// A late warm result cannot claim the STT task cursor or advance its phase.
	a.state.Phase = "transcribing"
	_, err = a.Handle(context.Background(), agent.Event{Kind: agent.TaskMessage, Result: &agent.TaskResult{Turn: spec.Turn, TaskID: "warm", Sequence: 1, Final: true, Outcome: agent.OK}})
	require.NoError(t, err)
	require.Empty(t, a.state.Task)
	require.Zero(t, a.state.Cursor)
	require.Equal(t, "transcribing", a.state.Phase)
}
