package callharness_test

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/pion/rtp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/relais/internal/workerprobe"
	"github.com/relais/pkg/callharness"
	"github.com/relais/pkg/controlplane"
	"github.com/relais/pkg/mediaworker"
	"github.com/relais/pkg/metrics"
	"github.com/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

// Only the crashed tenure is faulty. A new owner can persist its counter
// reservation normally, as it can after a slow writer or failed connection.
type checkpointFaultStore struct {
	sessionstore.Store
	epoch       atomic.Uint64
	attempts    atomic.Uint64
	mode        string
	entered     chan struct{}
	release     chan struct{}
	committed   chan struct{}
	once        sync.Once
	delayedDone atomic.Bool
}

func (s *checkpointFaultStore) PutState(ctx context.Context, lease sessionstore.Lease, data []byte) error {
	if lease.Epoch != s.epoch.Load() {
		return s.Store.PutState(ctx, lease, data)
	}
	s.attempts.Add(1)
	s.once.Do(func() { close(s.entered) })
	if s.mode == "fail" {
		return errors.New("injected checkpoint failure")
	}
	if s.mode == "delayed-success" && !s.delayedDone.Load() {
		select {
		case <-s.release:
		case <-ctx.Done():
			return ctx.Err()
		}
		if err := s.Store.PutState(ctx, lease, data); err != nil {
			return err
		}
		s.delayedDone.Store(true)
		close(s.committed)
		return nil
	}
	<-ctx.Done()
	return ctx.Err()
}

func TestRelayCheckpointAgePolicy(t *testing.T) {
	forSessionStores(t, func(t *testing.T, store sessionstore.Store) {
		if store == nil {
			store = sessionstore.NewMemory()
		}
		for _, mode := range []string{"fail", "stall", "delayed-success"} {
			for _, loss := range []bool{false, true} {
				name := mode + "/scaled"
				rate := float64(5000)
				policy := "scaled"
				if loss {
					name = mode + "/definitive-loss"
					rate = 12000
					policy = "definitive-loss"
				}
				t.Run(name, func(t *testing.T) {
					faulty := &checkpointFaultStore{Store: store, mode: mode, entered: make(chan struct{}), release: make(chan struct{}), committed: make(chan struct{})}
					envelope := mediaworker.CheckpointEnvelope{MaxAge: 50 * time.Millisecond, MaxRTPPacketRate: rate, RTPBacklogAllowance: 1}
					h := startHarness(t, callharness.Options{Relay: true, Workers: 2, SessionStore: faulty, DisableFrameCache: true,
						TakeoverConfig: controlplane.Config{CheckpointEnvelope: envelope, SequenceMargin: 8}})
					ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
					defer cancel()
					call, err := h.Dial(ctx, callharness.CallOptions{Video: true})
					require.NoError(t, err)
					sent := make(chan error, 1)
					go func() { sent <- call.SendMedia(ctx, 3*time.Second) }()
					require.NoError(t, h.WaitForSnapshot(ctx, 0, call.SessionID()))
					time.Sleep(300 * time.Millisecond)
					lease, err := store.Get(ctx, call.SessionID())
					require.NoError(t, err)
					fresh, err := store.GetState(ctx, call.SessionID())
					require.NoError(t, err)
					info, err := mediaworker.SnapshotCheckpoint(fresh)
					require.NoError(t, err)
					margin, _, _, err := envelope.CheckpointMargins(envelope.MaxAge, info, 8, 128)
					require.NoError(t, err, "fresh checkpoints must fit this configuration")
					_, err = mediaworker.SequenceResumeAttemptsWithReserve(fresh, margin, envelope.CallerSequenceReserve(info, 0))
					require.NoError(t, err, "the storage fault must cause budget exhaustion, not the configuration alone")
					var echoed atomic.Uint64
					var sequenceMu sync.Mutex
					latest := map[uint32]uint16{}
					require.NoError(t, workerprobe.SetAfterEcho(lease.Worker, func(_ context.Context, id string, raw []byte) {
						if id == call.SessionID() {
							echoed.Add(1)
							var header rtp.Header
							if _, err := header.Unmarshal(raw); err == nil {
								sequenceMu.Lock()
								latest[header.SSRC] = header.SequenceNumber
								sequenceMu.Unlock()
							}
						}
					}))
					metricBefore := checkpointCounter(t, metrics.CheckpointEnvelopeEvents.WithLabelValues(policy))
					faulty.epoch.Store(lease.Epoch)
					select {
					case <-faulty.entered:
					case <-ctx.Done():
						t.Fatal("faulty write not entered")
					}
					time.Sleep(650 * time.Millisecond)
					require.Greater(t, echoed.Load(), uint64(15), "media must continue while snapshot writes are stalled or failing")
					if mode == "delayed-success" {
						close(faulty.release)
						select {
						case <-faulty.committed:
						case <-ctx.Done():
							t.Fatal("delayed write not committed")
						}
					}
					// Negative control: inspect the exact checkpoint selected for the
					// crash (including the late commit), then prove its counters
					// lag actual encrypted output by more than the plain margin.
					stale, err := store.GetState(ctx, call.SessionID())
					require.NoError(t, err)
					var counters struct {
						State struct {
							Audio, Video struct {
								SSRC             uint32
								HighestSentIndex uint64
							}
						}
					}
					require.NoError(t, json.Unmarshal(stale, &counters))
					sequenceMu.Lock()
					audioAdvance := uint16(latest[counters.State.Audio.SSRC] - uint16(counters.State.Audio.HighestSentIndex))
					videoAdvance := uint16(latest[counters.State.Video.SSRC] - uint16(counters.State.Video.HighestSentIndex))
					sequenceMu.Unlock()
					require.Greater(t, audioAdvance, uint16(8), "negative control: plain margin is smaller than stale audio advance")
					require.Greater(t, videoAdvance, uint16(8), "negative control: plain margin is smaller than stale video advance")
					t.Logf("CHECKPOINT_NEGATIVE_CONTROL plain_margin=8 backlog_allowance=1 audio_advance=%d video_advance=%d", audioAdvance, videoAdvance)
					require.NoError(t, h.Kill(0))
					var status controlplane.Status
					require.Eventually(t, func() bool {
						var err error
						status, err = h.Status(ctx)
						return err == nil && len(status.Takeovers) == 1
					}, 2*time.Second, 5*time.Millisecond)
					event := status.Takeovers[0]
					require.Equal(t, policy, event.CheckpointPolicy)
					require.Equal(t, loss, event.Lost)
					require.Greater(t, event.SnapshotAge, 650*time.Millisecond)
					require.GreaterOrEqual(t, event.SnapshotAge, event.CheckpointAge)
					require.Equal(t, metricBefore+1, checkpointCounter(t, metrics.CheckpointEnvelopeEvents.WithLabelValues(policy)))
					require.Positive(t, event.Checkpoint.Successes)
					require.False(t, event.CheckpointStoredAt.IsZero())
					if loss {
						require.Empty(t, status.Calls)
						require.EqualValues(t, 1, status.LostCount)
						_, err = store.Get(ctx, call.SessionID())
						require.ErrorIs(t, err, sessionstore.ErrNotFound)
					} else {
						require.Empty(t, event.Error)
						require.Greater(t, event.SequenceMargin, uint16(8))
						require.Len(t, status.Calls, 1)
						require.Equal(t, "1", status.Calls[0].Owner)
						data, err := store.GetState(ctx, call.SessionID())
						require.NoError(t, err)
						restored, err := mediaworker.SnapshotCheckpoint(data)
						require.NoError(t, err)
						require.Equal(t, event.CheckpointAge, restored.TakeoverAge)
						require.Equal(t, event.SnapshotAge, restored.TakeoverSnapshotAge)
						require.True(t, event.CheckpointStoredAt.Equal(restored.TakeoverStoredAt))
					}
					require.NoError(t, <-sent)
					report, err := call.Hangup(ctx)
					if loss {
						require.Error(t, err)
						require.True(t, strings.Contains(err.Error(), "404"), err)
					} else {
						require.NoError(t, err)
					}
					require.Zero(t, report.DecryptionFailures.Total())
					require.Len(t, report.Moves, 1)
					if !loss {
						require.Len(t, report.Moves[0].Tracks, 2)
						for _, track := range report.Moves[0].Tracks {
							require.Greater(t, track.PacketsAfter, 15, "scaled policy must resume decryptable caller media on %s", track.Kind)
						}
					} else {
						require.Less(t, report.Tracks[0].LastArrival, event.End.Sub(report.StartedAt)+50*time.Millisecond, "definitive loss sends no resumed media")
					}
					require.Zero(t, report.Renegotiations)
					require.Zero(t, report.ICERestarts)
					require.Positive(t, report.Tracks[0].Packets)
					t.Logf("CHECKPOINT_POLICY mode=%s max_age=50ms max_rate=%.0f checkpoint_age=%s snapshot_age=%s policy=%s margin=%d fault_attempts=%d decryption_failures=%d", mode, rate, event.CheckpointAge, event.SnapshotAge, event.CheckpointPolicy, event.SequenceMargin, faulty.attempts.Load(), report.DecryptionFailures.Total())
				})
			}
		}
	})
}

func checkpointCounter(t *testing.T, counter prometheus.Counter) float64 {
	t.Helper()
	var metric dto.Metric
	require.NoError(t, counter.Write(&metric))
	return metric.GetCounter().GetValue()
}
