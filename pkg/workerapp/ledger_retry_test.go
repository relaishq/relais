package workerapp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relaishq/relais/examples/delayed-loopback/ledger"
	"github.com/relaishq/relais/internal/taskrun"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/stretchr/testify/require"
)

func TestLedgerBinaryRequestReceiptAndConflict(t *testing.T) {
	l, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.jsonl"))
	require.NoError(t, err)
	defer func() { require.NoError(t, l.Close()) }()
	statuses := make(chan int, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		response := httptest.NewRecorder()
		l.ServeHTTP(response, r)
		statuses <- response.Code
		w.WriteHeader(response.Code)
		_, _ = w.Write(response.Body.Bytes())
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var receipt struct {
		Key   string `json:"key"`
		Value []byte `json:"value"`
	}
	run := func(request []byte) error {
		task := taskrun.New(agent.TaskSpec{Class: agent.SideEffecting, Request: request}, "binary-key", 0, func(_ context.Context, _ uint64, chunk agent.Chunk) error {
			return json.Unmarshal(chunk.Data, &receipt)
		})
		return ledger.Handler(server.URL)(ctx, task)
	}
	first, second := run([]byte{0xff}), run([]byte{0xfe})
	require.Equal(t, http.StatusOK, <-statuses)
	require.Equal(t, http.StatusConflict, <-statuses, "different binary values on one key must conflict")
	require.NoError(t, first, "binary request must have a verified receipt")
	require.Equal(t, []byte{0xff}, receipt.Value)
	require.Equal(t, "binary-key", receipt.Key)
	var failure *taskrun.Failure
	require.True(t, errors.As(second, &failure))
	require.Equal(t, agent.Failed, failure.Outcome)
	deliveries, commits := l.Counts()
	require.Equal(t, 2, deliveries)
	require.Equal(t, 1, commits)
}

func TestLedgerRetriesLostFirstReplyWithExactBytes(t *testing.T) {
	l, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.jsonl"))
	require.NoError(t, err)
	defer func() { require.NoError(t, l.Close()) }()
	var mu sync.Mutex
	var bodies [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "body", http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		mu.Lock()
		bodies = append(bodies, body)
		first := len(bodies) == 1
		mu.Unlock()
		if first {
			l.ServeHTTP(httptest.NewRecorder(), r)
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		l.ServeHTTP(w, r)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var receipt struct {
		Key   string `json:"key"`
		Value []byte `json:"value"`
	}
	run := taskrun.New(agent.TaskSpec{Class: agent.SideEffecting, Request: []byte{0xff, 0, 0xfe}}, "retry-key", 0, func(_ context.Context, _ uint64, chunk agent.Chunk) error {
		return json.Unmarshal(chunk.Data, &receipt)
	})
	require.NoError(t, ledger.Handler(server.URL)(ctx, run), "same-key retry must recover the committed receipt")
	require.Equal(t, "retry-key", receipt.Key)
	require.Equal(t, []byte{0xff, 0, 0xfe}, receipt.Value)
	mu.Lock()
	received := append([][]byte(nil), bodies...)
	mu.Unlock()
	require.Len(t, received, 2)
	require.Equal(t, received[0], received[1], "retry must send identical bytes and key")
	deliveries, commits := l.Counts()
	require.Equal(t, 2, deliveries)
	require.Equal(t, 1, commits)
}

func TestLedgerRetryHonorsCancellation(t *testing.T) {
	for _, cancelOn := range []int32{1, 2} {
		t.Run(fmt.Sprint(cancelOn), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if requests.Add(1) == cancelOn {
					cancel()
					return
				}
				w.WriteHeader(http.StatusServiceUnavailable)
			}))
			defer server.Close()
			run := taskrun.New(agent.TaskSpec{Class: agent.SideEffecting, Request: []byte("effect")}, "cancel-key", 0, func(context.Context, uint64, agent.Chunk) error {
				t.Error("canceled attempt must not publish a receipt")
				return nil
			})
			done := make(chan error, 1)
			go func() { done <- ledger.Handler(server.URL)(ctx, run) }()
			select {
			case err := <-done:
				require.ErrorIs(t, err, context.Canceled)
			case <-time.After(5 * time.Second):
				t.Fatal("handler did not honor cancellation")
			}
			require.Equal(t, cancelOn, requests.Load())
		})
	}
}
