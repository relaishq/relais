package main

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/relais/internal/privateapi"
	"github.com/relais/pkg/controlplane"
)

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
