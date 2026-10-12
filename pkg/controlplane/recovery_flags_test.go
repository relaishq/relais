package controlplane_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/relais/internal/workerprobe"
	"github.com/relais/pkg/callharness"
	"github.com/relais/pkg/mediaworker"
	"github.com/relais/pkg/relay"
	"github.com/relais/pkg/sessionstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The first resume never reaches the worker. Hold it at the HTTP transport
// boundary until the real relay's next plan differs, then report uncertainty.
type unreceivedResume struct {
	entered chan mediaworker.ResumeRequest
	release chan struct{}
	mu      sync.Mutex
	resumes []mediaworker.ResumeRequest
}

func (f *unreceivedResume) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Path == "/resume" {
		data, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		req.Body = io.NopCloser(bytes.NewReader(data))
		var resume mediaworker.ResumeRequest
		if err := json.Unmarshal(data, &resume); err != nil {
			return nil, err
		}
		f.mu.Lock()
		f.resumes = append(f.resumes, resume)
		first := len(f.resumes) == 1
		f.mu.Unlock()
		if first {
			f.entered <- resume
			select {
			case <-f.release:
				return nil, context.DeadlineExceeded
			case <-req.Context().Done():
				return nil, req.Context().Err()
			}
		}
	}
	return http.DefaultTransport.RoundTrip(req)
}

func TestHTTPRecoveryConfirmationRetainsFirstFlags(t *testing.T) {
	for _, scenario := range []string{"complete-to-empty", "incomplete-to-empty", "empty-to-incomplete"} {
		t.Run(scenario, func(t *testing.T) {
			forHTTPRecoveryStores(t, func(t *testing.T, store sessionstore.Store) {
				fault := &unreceivedResume{entered: make(chan mediaworker.ResumeRequest, 1), release: make(chan struct{})}
				var release sync.Once
				defer release.Do(func() { close(fault.release) })
				buffer := &relay.BufferConfig{}
				holdTimeout := 500 * time.Millisecond
				if scenario == "incomplete-to-empty" {
					buffer.MaxSessionBytes = 4 << 10
				}
				if scenario == "empty-to-incomplete" {
					buffer.Window = 100 * time.Millisecond
					holdTimeout = 3 * time.Second
				}
				s := newHTTPSystemWithSettings(t, nil, nil, nil, httpSystemSettings{
					store: store, buffer: buffer, holdTimeout: holdTimeout, targetTransport: fault,
				})
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				call, err := s.harness.Dial(ctx, callharness.CallOptions{Video: true, Worker: 0})
				require.NoError(t, err)
				mediaCtx, stop := context.WithCancel(ctx)
				var sent chan error
				startMedia := func() {
					sent = make(chan error, 1)
					go func() { sent <- call.SendMedia(mediaCtx, 8*time.Second) }()
				}
				defer func() {
					stop()
					if sent != nil {
						<-sent
					}
				}()
				if scenario == "empty-to-incomplete" {
					// Stop caller input. The ring expires during dead-worker detection,
					// so the first prepared plan has no input to schedule.
					require.NoError(t, call.SendMedia(ctx, 650*time.Millisecond))
				} else {
					startMedia()
					time.Sleep(650 * time.Millisecond)
				}
				s.harness.RecordProcessKill("0", time.Now())
				require.NoError(t, workerprobe.Kill(s.workers[0].LocalAddr()))
				var first mediaworker.ResumeRequest
				select {
				case first = <-fault.entered:
				case <-ctx.Done():
					t.Fatal("first resume did not reach the transport")
				}
				require.Zero(t, s.workers[1].SessionCount(), "the first resume never adopted")
				require.Equal(t, scenario == "complete-to-empty", first.RelayReplay)
				require.Equal(t, scenario == "incomplete-to-empty", first.RelayInputReplay)
				if scenario == "empty-to-incomplete" {
					startMedia()
					require.Eventually(t, func() bool { return s.relay.Stats().HeldPackets >= 10 }, time.Second, time.Millisecond,
						"live input must arrive at the gate before confirmation")
				} else {
					require.Eventually(t, func() bool { return s.relay.Stats().HoldTimeouts == 1 }, time.Second, time.Millisecond)
				}
				inbound, err := mediaworker.SnapshotReplayIndexes(first.State)
				require.NoError(t, err)
				plan, err := s.relay.BeginReplay(ctx, call.SessionID(), s.workers[0].LocalAddr(), inbound)
				require.NoError(t, err)
				require.False(t, plan.Complete, "the confirmation plan cannot promise codec continuity")
				if scenario == "empty-to-incomplete" {
					require.NotEmpty(t, plan.Windows)
				} else {
					require.Empty(t, plan.Windows)
				}
				release.Do(func() { close(fault.release) })
				require.Eventually(t, func() bool {
					status, err := s.plane.Status(ctx)
					return err == nil && len(status.Takeovers) == 1
				}, 3*time.Second, time.Millisecond)
				status, err := s.plane.Status(ctx)
				require.NoError(t, err)
				require.False(t, status.Takeovers[0].Lost)
				require.False(t, status.Takeovers[0].Result.RelayReplayComplete)
				require.Equal(t, scenario != "empty-to-incomplete", status.Takeovers[0].Result.RelayReplayRecoveryPLI)
				fault.mu.Lock()
				resumes := append([]mediaworker.ResumeRequest(nil), fault.resumes...)
				fault.mu.Unlock()
				require.Len(t, resumes, 2)
				// Tie each scenario to the confirmation request the worker received.
				if scenario == "empty-to-incomplete" {
					require.NotEmpty(t, resumes[1].DuplicateWindows, "gated input must reach the confirmation plan")
				} else {
					require.Empty(t, resumes[1].DuplicateWindows)
				}
				assert.Equal(t, first.RelayReplay, resumes[1].RelayReplay, "confirmation retains complete-input recovery flag")
				assert.Equal(t, first.RelayInputReplay, resumes[1].RelayInputReplay, "confirmation retains incomplete-input recovery flag")
				require.Equal(t, first.Lease.Epoch, resumes[1].Lease.Epoch)
				require.Equal(t, first.State, resumes[1].State)
				require.Equal(t, first.SequenceMargin, resumes[1].SequenceMargin)
				require.Equal(t, first.SRTCPIndexMargin, resumes[1].SRTCPIndexMargin)
				time.Sleep(700 * time.Millisecond)
				stop()
				report, err := call.Hangup(ctx)
				require.NoError(t, err)
				assert.Equal(t, 1, report.SentVideo.KeyframeRequests, "recover once, without waiting for receiver feedback")
				require.Zero(t, report.KeyframeRequestsSent)
				require.Zero(t, report.DecryptionFailures.Total())
				require.Len(t, report.Moves, 1)
				assert.Positive(t, report.Moves[0].Recovery.FirstDecodedPLIAfterKill, "the caller must decode the requested recovery picture")
				t.Logf("CONFIRMATION_RECOVERY first=(%t,%t) confirmation=(%t,%t) next_windows=%d keyframes=%d decoded_pli=%s", first.RelayReplay, first.RelayInputReplay,
					resumes[1].RelayReplay, resumes[1].RelayInputReplay, len(plan.Windows), report.SentVideo.KeyframeRequests, report.Moves[0].Recovery.FirstDecodedPLIAfterKill)
			})
		})
	}
}

var _ http.RoundTripper = (*unreceivedResume)(nil)
