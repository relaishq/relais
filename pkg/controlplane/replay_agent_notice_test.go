package controlplane

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/privateapi"
	"github.com/relaishq/relais/pkg/agent"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/relay"
	"github.com/stretchr/testify/require"
)

func TestAgentReplayNoticeMatchesDirectAndHTTP(t *testing.T) {
	for _, mode := range []string{"none", "complete", "incomplete", "empty"} {
		replay := mode == "complete" || mode == "incomplete"
		for _, remote := range []bool{false, true} {
			t.Run(mode+"/http="+stringBool(remote), func(t *testing.T) {
				p, _, baseB, baseR := setup(t)
				if mode != "none" {
					p.relay = &checkpointReplayRelay{fakeRelay: baseR, incompletePlan: mode != "complete", emptyPlan: mode == "empty"}
				}
				notices := make(chan mediaworker.ResumeOptions, 1)
				target := &replayCheckpointWorker{takeoverWorker: &takeoverWorker{fakeWorker: baseB}}
				p.workers["b"].worker = target
				if remote {
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.URL.Path != "/resume" {
							require.NoError(t, target.RequestKeyframe(r.Context(), "call"))
							w.WriteHeader(http.StatusNoContent)
							return
						}
						var req mediaworker.ResumeRequest
						if !privateapi.Read(w, r, &req) {
							return
						}
						id, err := target.ResumeSession(req.State, mediaworker.ResumeOptions{Lease: req.Lease, Context: r.Context(), Kind: req.Kind, InputMayBeDuplicated: req.InputMayBeDuplicated, DuplicateWindows: req.DuplicateWindows, RelayReplay: req.RelayReplay, RelayInputReplay: req.RelayInputReplay})
						if err != nil {
							privateapi.Error(w, err, mediaworker.RemoteErrors)
							return
						}
						notices <- target.opts
						privateapi.Write(w, mediaworker.ResumeReply{ID: id})
					}))
					defer server.Close()
					p.workers["b"].worker = &RemoteWorker{URL: server.URL, Client: server.Client()}
				}
				ctx := context.Background()
				id, _, err := p.Create(ctx, "offer", "a")
				require.NoError(t, err)
				lease, err := p.store.Get(ctx, id)
				require.NoError(t, err)
				require.NoError(t, p.store.PutState(ctx, lease, replayCheckpointSnapshot(t, id, 10)))
				p.workers["a"].dead = true
				p.takeover(ctx, p.workers["a"], lease, time.Now())
				require.Len(t, p.recentTakeovers(), 1)
				var opts mediaworker.ResumeOptions
				if remote {
					opts = <-notices
				} else {
					opts = target.opts
				}
				require.Equal(t, agent.Takeover, opts.Kind)
				require.Equal(t, replay, opts.InputMayBeDuplicated)
				require.Equal(t, mode == "complete", opts.RelayReplay)
				require.Equal(t, mode == "incomplete", opts.RelayInputReplay)
				require.Len(t, p.recentTakeovers(), 1)
				if replay {
					want := []agent.DuplicateWindow{{SSRC: 42, First: 11, Last: 11}}
					require.Equal(t, want, opts.DuplicateWindows)
					require.Equal(t, want, p.recentTakeovers()[0].Result.DuplicateWindows)
				} else {
					require.Empty(t, opts.DuplicateWindows)
				}
			})
		}
	}
}
func stringBool(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

func TestAgentReplayNoticeExcludesNewLiveInput(t *testing.T) {
	planned := []relay.ReplayWindow{{SSRC: 42, First: 11, Last: 20}}
	got := deliveredReplayWindows(planned, []relay.ReplayWindow{{SSRC: 42, First: 15, Last: 30}, {SSRC: 43, First: 1, Last: 10}})
	require.Equal(t, []agent.DuplicateWindow{{SSRC: 42, First: 15, Last: 20}}, got)
	require.Empty(t, deliveredReplayWindows(planned, []relay.ReplayWindow{{SSRC: 42, First: 21, Last: 30}}))
	require.Empty(t, deliveredReplayWindows(nil, []relay.ReplayWindow{{SSRC: 42, First: 1, Last: 10}}))
}

func TestAgentReplayNoticeAlignsColdRelayROC(t *testing.T) {
	// A new relay sees only the sequence wrap in its short ring; the snapshot
	// still supplies the caller's real ROC even before the agent consumes input.
	got := agentReplayWindows([]relay.ReplayWindow{{SSRC: 42, First: 65530, Last: 65609}}, map[uint32]uint64{42: 8<<16 | 73})
	require.Equal(t, []agent.DuplicateWindow{{SSRC: 42, First: 7<<16 | 65530, Last: 8<<16 | 73}}, got)
}
