package workerapp

import (
	"context"
	"errors"
	"log"
	"net/http"
	"net/url"
	"time"

	"github.com/relaishq/relais/internal/privateapi"
	"github.com/relaishq/relais/pkg/controlplane"
)

type drainTasks interface {
	BeginDrain()
	QuiesceTasks(context.Context) error
}

// The mark is bounded independently so an unresponsive mark cannot spend the
// handler budget. Every phase shares the caller's one shutdown deadline.
func quiesceWorker(ctx context.Context, control, name string, worker drainTasks, taskBudget, reserve time.Duration) {
	markCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	err := privateapi.Do(markCtx, &http.Client{}, control, http.MethodPost, "/private/workers/"+url.PathEscape(name)+"/mark-draining", nil, nil, controlplane.RemoteErrors)
	cancel()
	if err != nil {
		log.Printf("worker %s: mark-draining not acknowledged; skipping handler wait and falling through to drain: %v", name, err)
		return
	}
	worker.BeginDrain()
	if reserve == 0 {
		return
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return
	}
	budget := max(time.Duration(0), min(taskBudget, time.Until(deadline)-reserve))
	// Even a zero budget fences launches until urgent export. Never reset the
	// budget per session or wait for buffered audio to finish playing.
	waitCtx, stopWait := context.WithTimeout(ctx, budget)
	err = worker.QuiesceTasks(waitCtx)
	stopWait()
	if err != nil {
		log.Printf("worker %s: handler quiescence ended (%s budget): %v; draining", name, budget, err)
	}
}

// The context bounds both coordination and waiting; the worker remains live
// throughout. An unavailable control plane is logged by the caller, then Close
// releases remaining calls rather than making SIGTERM an unbounded wait.
func drainWorker(ctx context.Context, control, name string, remaining func() int) error {
	var reply controlplane.DrainResult
	err := privateapi.Do(ctx, &http.Client{}, control, http.MethodPost, "/workers/"+url.PathEscape(name)+"/drain", nil, &reply, nil)
	if err != nil {
		return err
	}
	var drainErr error
	if reply.Error != "" {
		drainErr = errors.New(reply.Error)
	}
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for remaining() != 0 {
		select {
		case <-ctx.Done():
			return errors.Join(drainErr, ctx.Err())
		case <-ticker.C:
		}
	}
	return drainErr
}
