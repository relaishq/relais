package controlplane

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/relais/internal/privateapi"
	"github.com/relais/pkg/agent"
	"github.com/relais/pkg/mediaworker"
	"github.com/stretchr/testify/require"
)

func TestRemoteAgentResumeNotice(t *testing.T) {
	requests := make(chan mediaworker.ResumeRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req mediaworker.ResumeRequest
		if !privateapi.Read(w, r, &req) {
			return
		}
		requests <- req
		privateapi.Write(w, mediaworker.ResumeReply{ID: "call"})
	}))
	defer server.Close()
	windows := []agent.DuplicateWindow{{SSRC: 456, First: 900, Last: 925}}
	opts := mediaworker.ResumeOptions{Kind: agent.Takeover, CheckpointAge: 350 * time.Millisecond, SnapshotAge: 400 * time.Millisecond, InputMayBeDuplicated: true, DuplicateWindows: windows, Context: context.Background()}
	_, err := (&RemoteWorker{URL: server.URL, Client: server.Client()}).ResumeSession([]byte("snapshot"), opts)
	require.NoError(t, err)
	req := <-requests
	require.Equal(t, opts.Kind, req.Kind)
	require.Equal(t, opts.CheckpointAge, req.CheckpointAge)
	require.Equal(t, opts.SnapshotAge, req.SnapshotAge)
	require.Equal(t, opts.InputMayBeDuplicated, req.InputMayBeDuplicated)
	require.Equal(t, opts.DuplicateWindows, req.DuplicateWindows)
}
