package controlplane

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/relaishq/relais/internal/privateapi"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/stretchr/testify/require"
)

type reviewUnflushedWorker struct{ *fakeWorker }

func (*reviewUnflushedWorker) ExportSession(string) ([]byte, error) {
	return nil, errors.Join(mediaworker.ErrExportNotFlushed, mediaworker.ErrMoveBudget)
}

// The reviewer's uncertainty probe, with assertions at the real move boundary
// for both in-process errors and their private HTTP representation.
func TestReviewExportBudgetRefusalDoesNotTriggerCrashRecovery(t *testing.T) {
	for _, remote := range []bool{false, true} {
		t.Run(map[bool]string{false: "direct", true: "remote"}[remote], func(t *testing.T) {
			p, a, b, _ := setup(t)
			ctx := context.Background()
			id, _, err := p.Create(ctx, "offer", "a")
			require.NoError(t, err)
			before, err := p.store.Get(ctx, id)
			require.NoError(t, err)
			cause := errors.Join(mediaworker.ErrExportNotFlushed, mediaworker.ErrMoveBudget)
			if remote {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					privateapi.Error(w, cause, map[string]error{"export_not_flushed": mediaworker.ErrExportNotFlushed})
				}))
				defer server.Close()
				p.workers["a"].worker = &RemoteWorker{URL: server.URL, Client: server.Client()}
			} else {
				p.workers["a"].worker = &reviewUnflushedWorker{a}
			}
			result, err := p.Move(ctx, id, "b")
			require.ErrorIs(t, err, mediaworker.ErrMoveBudget)
			require.ErrorIs(t, err, mediaworker.ErrExportNotFlushed)
			require.False(t, uncertainExport(err))
			require.True(t, uncertainExport(privateapi.Uncertain(cause)), "transport uncertainty still takes precedence")
			require.False(t, result.Lost)
			require.Equal(t, "move", result.Kind)
			require.True(t, a.runs(id))
			require.False(t, b.runs(id))
			after, err := p.store.Get(ctx, id)
			require.NoError(t, err)
			require.Equal(t, before.Worker, after.Worker)
			require.Equal(t, before.Epoch, after.Epoch, "unflushed export must not bump epoch")
			status, err := p.Status(ctx)
			require.NoError(t, err)
			require.Empty(t, status.Takeovers)
		})
	}
}
