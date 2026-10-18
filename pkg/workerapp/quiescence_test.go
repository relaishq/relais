package workerapp

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type observedDrain struct {
	marked, waited bool
	budget         time.Duration
}

func (w *observedDrain) BeginDrain() { w.marked = true }
func (w *observedDrain) QuiesceTasks(ctx context.Context) error {
	w.waited = true
	deadline, _ := ctx.Deadline()
	w.budget = time.Until(deadline)
	<-ctx.Done()
	return ctx.Err()
}
func TestQuiescenceBudgetAndMarkFailure(t *testing.T) {
	for _, mode := range []string{"configured", "unset", "tight", "zero", "mark-fails", "mark-stalls"} {
		t.Run(mode, func(t *testing.T) {
			requests := make(chan error, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var err error
				if r.URL.Path != "/private/workers/source/mark-draining" || r.Method != http.MethodPost {
					err = fmt.Errorf("unexpected mark request: %s %s", r.Method, r.URL.Path)
				}
				requests <- err
				if mode == "mark-stalls" {
					<-r.Context().Done()
					return
				}
				if mode == "mark-fails" {
					w.WriteHeader(503)
					return
				}
				w.WriteHeader(204)
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			taskBudget, reserve := 40*time.Millisecond, 100*time.Millisecond
			if mode == "unset" {
				reserve = 0
			}
			if mode == "tight" {
				reserve = 280 * time.Millisecond
			}
			if mode == "zero" {
				taskBudget = 0
			}
			worker := new(observedDrain)
			start := time.Now()
			quiesceWorker(ctx, server.URL, "source", worker, taskBudget, reserve)
			require.NoError(t, <-requests)
			if mode == "mark-fails" || mode == "mark-stalls" {
				require.False(t, worker.marked)
				require.False(t, worker.waited)
				require.Less(t, time.Since(start), 250*time.Millisecond)
				return
			}
			require.True(t, worker.marked)
			require.Equal(t, mode != "unset", worker.waited)
			require.LessOrEqual(t, worker.budget, taskBudget)
			if mode == "tight" {
				require.LessOrEqual(t, worker.budget, 20*time.Millisecond)
			}
		})
	}
}
func TestDrainFlagValidation(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"-agent-drain-task-budget=-1ns"}, "-agent-drain-task-budget must be between 0 and 2s"},
		{[]string{"-agent-drain-task-budget=2001ms"}, "-agent-drain-task-budget must be between 0 and 2s"},
		{[]string{"-agent-drain-move-reserve=0"}, "-agent-drain-move-reserve must be greater than 0"},
		{[]string{"-agent-drain-move-reserve=-1s"}, "-agent-drain-move-reserve must be greater than 0"},
		{[]string{"-agent-drain-move-reserve=5s"}, "less than -drain-timeout (5s)"},
		{[]string{"-drain-timeout=1s", "-agent-drain-move-reserve=2s"}, "less than -drain-timeout (1s)"},
	} {
		require.ErrorContains(t, Run(context.Background(), tc.args, Options{}), tc.want)
	}
}
