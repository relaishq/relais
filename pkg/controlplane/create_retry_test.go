package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/relaishq/relais/internal/privateapi"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

type createFailureWorker struct {
	*fakeWorker
	err      error
	attempts int
}

func (w *createFailureWorker) CreateSession(context.Context, string) (string, string, error) {
	w.attempts++
	return "", "", w.err
}
func TestCreateRetriesOnlyDefiniteDrainRefusals(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		retry bool
	}{
		{"draining", mediaworker.ErrDraining, true},
		{"lost-reply", privateapi.Uncertain(errors.New("lost create reply")), false},
		{"timeout", context.DeadlineExceeded, false},
		{"canceled", context.Canceled, false},
		{"unknown", errors.New("worker create failed"), false},
		{"uncertain-draining", errors.Join(mediaworker.ErrDraining, privateapi.ErrUncertain), false},
		{"timeout-draining", errors.Join(mediaworker.ErrDraining, context.DeadlineExceeded), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, a, b, _ := setup(t)
			source := &createFailureWorker{fakeWorker: a, err: tc.err}
			p.workers["a"].worker = source
			id, _, err := p.Create(context.Background(), "offer", "a")
			require.Equal(t, 1, source.attempts)
			if tc.retry {
				require.NoError(t, err)
				require.True(t, b.runs(id))
				require.Len(t, p.calls, 1)
			} else {
				require.ErrorIs(t, err, tc.err)
				require.Empty(t, p.calls)
				require.Zero(t, b.next)
			}
			require.Zero(t, p.workers["a"].reserved)
			require.Zero(t, p.workers["b"].reserved)
		})
	}
}
func TestCreateExcludesEveryRefusingWorker(t *testing.T) {
	p, a, b, _ := setup(t)
	for _, w := range []*fakeWorker{a, b} {
		name := "a"
		if w == b {
			name = "b"
		}
		p.workers[name].worker = &createFailureWorker{fakeWorker: w, err: mediaworker.ErrDraining}
	}
	_, _, err := p.Create(context.Background(), "offer", "")
	require.ErrorIs(t, err, ErrNoTarget)
	for _, r := range p.workers {
		require.Equal(t, 1, r.worker.(*createFailureWorker).attempts)
		require.Zero(t, r.reserved)
	}
	require.Empty(t, p.calls)
}

type createResponseGolden struct {
	Code        string `json:"code"`
	Status      int    `json:"status"`
	ContentType string `json:"content_type"`
	Body        string `json:"body"`
}

// Captured from the 17c110c handler before changing create placement. These
// literals preserve status, content type and body independently of writeError.
func TestCreateErrorResponsesMatchMain(t *testing.T) {
	data, err := os.ReadFile("testdata/create-errors-main.json")
	require.NoError(t, err)
	var cases []createResponseGolden
	require.NoError(t, json.Unmarshal(data, &cases))
	for _, tc := range cases {
		t.Run(tc.Code, func(t *testing.T) {
			p, a, _, _ := setup(t)
			failure := RemoteErrors[tc.Code]
			if failure == nil {
				failure = privateapi.CommonErrors[tc.Code]
			}
			if tc.Code == "unknown" {
				failure = errors.New("worker create failed")
			}
			require.NotNil(t, failure)
			p.workers["a"].worker = &createFailureWorker{fakeWorker: a, err: failure}
			if tc.Code == "no_target" {
				require.NoError(t, p.MarkDraining("a"))
				require.NoError(t, p.MarkDraining("b"))
			}
			r := httptest.NewRequest(http.MethodPost, "/calls?worker=a", strings.NewReader("offer"))
			r.Header.Set("Content-Type", "application/sdp")
			w := httptest.NewRecorder()
			p.Handler().ServeHTTP(w, r)
			require.Equal(t, tc.Status, w.Code)
			require.Equal(t, tc.ContentType, w.Header().Get("Content-Type"))
			require.Equal(t, tc.Body, w.Body.String())
		})
	}
}
func TestUnknownWorkerDrainKeepsMainStatus(t *testing.T) {
	p, _, _, _ := setup(t)
	err := p.MarkDraining("missing")
	require.ErrorIs(t, err, sessionstore.ErrNotFound)
	require.Equal(t, `controlplane: unknown worker "missing": `+sessionstore.ErrNotFound.Error(), err.Error())
	for _, path := range []string{"/workers/missing/drain", "/private/workers/missing/mark-draining"} {
		w := httptest.NewRecorder()
		p.ProcessHandler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, nil))
		want := 500
		if strings.Contains(path, "mark-draining") {
			want = 404
		}
		require.Equal(t, want, w.Code)
		require.Contains(t, w.Body.String(), "unknown worker")
		require.Contains(t, w.Body.String(), "missing")
	}
}
