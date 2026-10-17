package workerapp_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/relaishq/relais/examples/delayed-loopback/ledger"
	"github.com/relaishq/relais/internal/taskrun"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/stretchr/testify/require"
)

// The teaching service can commit before its HTTP response is lost. Exercise
// the production handler, including real connection loss, without a provider.
func TestLedgerExampleTypedOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		outcome agent.Outcome
	}{
		{"lost-reply", 0, agent.OutcomeUnknown},
		{"uncertain-write", http.StatusServiceUnavailable, agent.OutcomeUnknown},
		{"invalid-receipt", http.StatusOK, agent.OutcomeUnknown},
		{"conflict", http.StatusConflict, agent.Failed},
		{"capacity", http.StatusInsufficientStorage, agent.Capacity},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var service *ledger.Ledger
			if tc.status == 0 {
				var err error
				service, err = ledger.Open(filepath.Join(t.TempDir(), "ledger.jsonl"))
				require.NoError(t, err)
				defer func() { require.NoError(t, service.Close()) }()
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.status == 0 {
					service.ServeHTTP(httptest.NewRecorder(), r)
					conn, _, err := w.(http.Hijacker).Hijack()
					if err == nil {
						_ = conn.Close()
					}
					return
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte("unavailable receipt"))
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			run := taskrun.New(agent.TaskSpec{Class: agent.SideEffecting, Request: []byte("effect")}, "stable-key", 0, func(context.Context, uint64, agent.Chunk) error {
				t.Error("invalid response must not publish a receipt")
				return nil
			})
			err := ledger.Handler(server.URL)(ctx, run)
			var failure *taskrun.Failure
			require.True(t, errors.As(err, &failure), "handler must return a typed outcome: %v", err)
			require.Equal(t, tc.outcome, failure.Outcome)
			if service != nil {
				deliveries, commits := service.Counts()
				require.Equal(t, 1, deliveries)
				require.Equal(t, 1, commits)
			}
		})
	}
}
