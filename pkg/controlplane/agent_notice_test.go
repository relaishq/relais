package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
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

// Agent-version failures leave ownership to the control plane, which can try
// another compatible factory or finish loss immediately with the real cause.
type rejectingAgentWorker struct {
	*fakeWorker
	attempts int
	err      error
}

func (w *rejectingAgentWorker) ResumeSession([]byte, mediaworker.ResumeOptions) (string, error) {
	w.attempts++
	if w.err != nil {
		return "", w.err
	}
	return "", fmt.Errorf("%w: %w", agent.ErrRestore, agent.ErrVersion)
}
func TestAgentTakeoverRestoreFailurePreservesCause(t *testing.T) {
	for _, failure := range []struct {
		name  string
		err   error
		cause error
	}{
		{"version", fmt.Errorf("%w: %w", agent.ErrRestore, agent.ErrVersion), agent.ErrVersion},
		{"restore-timeout", fmt.Errorf("%w: %w", agent.ErrRestore, context.DeadlineExceeded), agent.ErrRestore},
	} {
		for _, remote := range []bool{false, true} {
			for _, fallback := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/remote=%t/fallback=%t", failure.name, remote, fallback), func(t *testing.T) {
					p, _, baseB, _ := setup(t)
					b := &rejectingAgentWorker{fakeWorker: baseB, err: failure.err}
					p.workers["b"].worker = b
					if remote {
						server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							switch {
							case r.URL.Path == "/resume":
								var req mediaworker.ResumeRequest
								if !privateapi.Read(w, r, &req) {
									return
								}
								_, err := b.ResumeSession(req.State, mediaworker.ResumeOptions{})
								if errors.Is(err, agent.ErrVersion) {
									privateapi.Error(w, err, map[string]error{"agent_version": agent.ErrVersion})
								} else {
									privateapi.Error(w, err, map[string]error{"agent_restore": agent.ErrRestore})
								}
							case r.URL.Path == "/status":
								privateapi.Write(w, mediaworker.WorkerStatus{Address: b.addr})
							case r.Method == http.MethodDelete:
								w.WriteHeader(http.StatusNoContent)
							default:
								t.Errorf("unexpected worker request: %s %s", r.Method, r.URL.Path)
							}
						}))
						defer server.Close()
						p.workers["b"].worker = &RemoteWorker{URL: server.URL, Client: server.Client()}
					}
					if fallback {
						c := &takeoverWorker{fakeWorker: &fakeWorker{store: p.store, addr: netip.MustParseAddrPort("127.0.0.1:3"), running: map[string]bool{}}}
						require.NoError(t, p.Register("c", c.addr, c))
					}
					ctx := context.Background()
					id, _, err := p.Create(ctx, "offer", "a")
					require.NoError(t, err)
					lease, err := p.store.Get(ctx, id)
					require.NoError(t, err)
					require.NoError(t, p.store.PutState(ctx, lease, takeoverSnapshot(t, id, 0)))
					p.workers["a"].dead = true
					p.takeover(ctx, p.workers["a"], lease, time.Now())
					status, err := p.Status(ctx)
					require.NoError(t, err)
					require.Len(t, status.Takeovers, 1, "typed agent failure must finish this attempt rather than retry the same target")
					require.Equal(t, 1, b.attempts)
					if fallback {
						require.False(t, status.Takeovers[0].Lost)
						require.Equal(t, "c", status.Calls[0].Owner)
						require.NoError(t, p.End(ctx, id))
					} else {
						require.True(t, status.Takeovers[0].Lost)
						require.Contains(t, status.Takeovers[0].Error, failure.cause.Error())
						require.NotContains(t, status.Takeovers[0].Error, "lease vanished")
						require.Empty(t, status.Calls)
					}
				})
			}
		}
	}
}
