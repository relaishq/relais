package mediaworker

import (
	"context"
	"errors"
	"github.com/relais/pkg/framecache"
	"github.com/relais/pkg/relay"
	"github.com/relais/pkg/sessionstore"
	"math"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCheckpointEnvelopeMargins(t *testing.T) {
	envelope := CheckpointEnvelope{}.Defaults()
	margin, rtcp, event, err := envelope.CheckpointMargins(550*time.Millisecond, CheckpointState{}, 8192, 128)
	require.NoError(t, err)
	require.EqualValues(t, 8192, margin)
	require.EqualValues(t, 128, rtcp)
	require.False(t, event)
	margin, rtcp, event, err = envelope.CheckpointMargins(1500*time.Millisecond, CheckpointState{}, 8192, 128)
	require.NoError(t, err)
	require.EqualValues(t, 9440, margin)
	require.EqualValues(t, 205, rtcp)
	require.True(t, event)
	_, _, event, err = envelope.CheckpointMargins(4*time.Second, CheckpointState{}, 8192, 128)
	require.ErrorIs(t, err, ErrSequenceBudgetExhausted)
	require.True(t, event)
	margin, _, event, err = envelope.CheckpointMargins(500*time.Millisecond, CheckpointState{RTPPacketRate: 10000}, 6000, 128)
	require.NoError(t, err)
	require.EqualValues(t, 6315, margin)
	require.True(t, event)
	for _, rate := range []float64{math.NaN(), math.Inf(1)} {
		_, _, _, err = envelope.CheckpointMargins(time.Second, CheckpointState{RTPPacketRate: rate}, 8192, 128)
		require.Error(t, err)
	}
	require.Equal(t, 0.75, (CheckpointState{Attempts: 4, Successes: 3, Failures: 1}).SuccessRate())
	require.Zero(t, (CheckpointState{}).SuccessRate())
}

func TestSRTCPMeterBoundsWorkerIndexRate(t *testing.T) {
	var meter packetMeter
	now := time.Now()
	for i := uint64(0); i < checkpointSRTCPBurst; i++ {
		require.True(t, meter.allow(now, i, 100, checkpointSRTCPBurst))
	}
	require.False(t, meter.allow(now, checkpointSRTCPBurst, 100, checkpointSRTCPBurst))
	require.True(t, meter.allow(now.Add(100*time.Millisecond), checkpointSRTCPBurst, 100, checkpointSRTCPBurst))
}

func TestSourceRateIgnoresThreeSecondHoldBacklogAndDecays(t *testing.T) {
	var meter sourceRate
	now := time.Now()
	// 500 pps for two seconds, then three seconds queued and released at once.
	for i := uint64(0); i <= 1000; i++ {
		meter.observe(now.Add(time.Duration(i)*2*time.Millisecond), i, uint32(i)*180, 90000)
	}
	for i := uint64(1001); i <= 2500; i++ {
		meter.observe(now.Add(5*time.Second), i, uint32(i)*180, 90000)
	}
	require.InDelta(t, 500, meter.rate(now.Add(5*time.Second)), 40)
	require.Less(t, meter.rate(now.Add(35*time.Second)), float64(70), "a retained peak must decay")
	_, _, _, err := (CheckpointEnvelope{}).CheckpointMargins(time.Second, CheckpointState{RTPPacketRate: meter.rate(now.Add(5 * time.Second))}, 8192, 128)
	require.NoError(t, err, "a later crash must still fit the default resume budget")
}

func TestCheckpointObservedPeakDecaysAcrossMove(t *testing.T) {
	w := newTestWorker(t)
	call, _ := dialDTLSCaller(t, w)
	sess := w.session(call.id)
	sess.mu.Lock()
	sess.state.Checkpoint.RTPPacketRate = 20000
	sess.videoRate.peak, sess.videoRate.peakAt = 20000, time.Now().Add(-30*time.Second)
	sess.mu.Unlock()
	state, err := w.ExportSession(call.id)
	require.NoError(t, err)
	info, err := SnapshotCheckpoint(state)
	require.NoError(t, err)
	require.InDelta(t, 2500, info.RTPPacketRate, 10, "export must not retain the all-time state maximum")
	_, err = w.ResumeSession(state, ResumeOptions{})
	require.NoError(t, err)
	sess = w.session(call.id)
	sess.mu.Lock()
	sess.audioRate.peakAt = time.Now().Add(-30 * time.Second)
	sess.videoRate.peakAt = sess.audioRate.peakAt
	sess.mu.Unlock()
	state, err = w.SnapshotSession(call.id)
	require.NoError(t, err)
	info, err = SnapshotCheckpoint(state)
	require.NoError(t, err)
	require.InDelta(t, 312.5, info.RTPPacketRate, 10, "resumed observation must keep decaying")
}

type checkpointFailStore struct {
	sessionstore.Store
	fail atomic.Bool
}

func (s *checkpointFailStore) PutState(ctx context.Context, lease sessionstore.Lease, data []byte) error {
	if s.fail.Load() {
		return errors.New("checkpoint unavailable")
	}
	return s.Store.PutState(ctx, lease, data)
}
func TestCheckpointFailuresPersistAfterRecovery(t *testing.T) {
	store := &checkpointFailStore{Store: sessionstore.NewMemory()}
	r, err := relay.New(relay.Config{PublicAddr: "127.0.0.1:0", WorkerAddr: "127.0.0.1:0", Owners: store})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, r.Close()) })
	w, err := New(Config{SnapshotInterval: time.Hour, Relay: &RelayConfig{Owners: store, Addr: r.WorkerAddr(), PublicAddr: r.PublicAddr()}})
	require.NoError(t, err)
	r.AddWorker(w.LocalAddr())
	t.Cleanup(func() { require.NoError(t, w.Close()) })
	call, _ := dialDTLSCaller(t, w)
	require.Eventually(t, func() bool { return w.session(call.id).snapshotStored.Load() }, time.Second, time.Millisecond)
	sess := w.session(call.id)
	// Serialize against the initial write before observing counters.
	sess.snapshotMu.Lock()
	ctx := context.Background()
	data, err := store.GetState(ctx, call.id)
	sess.snapshotMu.Unlock()
	require.NoError(t, err)
	before, err := SnapshotCheckpoint(data)
	require.NoError(t, err)
	checkpoint, err := store.Checkpoint(ctx, call.id, data)
	require.NoError(t, err)
	store.fail.Store(true)
	require.Error(t, sess.persistSnapshot())
	require.Error(t, sess.persistSnapshot())
	unchanged, err := store.Checkpoint(ctx, call.id, data)
	require.NoError(t, err)
	require.True(t, checkpoint.StoredAt.Equal(unchanged.StoredAt))
	local, err := w.SessionCheckpoint(call.id)
	require.NoError(t, err)
	require.Equal(t, before.Failures+2, local.Failures)
	require.Greater(t, local.AgeAtCapture, time.Duration(0))
	store.fail.Store(false)
	require.NoError(t, sess.persistSnapshot())
	data, err = store.GetState(ctx, call.id)
	require.NoError(t, err)
	after, err := SnapshotCheckpoint(data)
	require.NoError(t, err)
	require.Equal(t, before.Attempts+3, after.Attempts)
	require.Equal(t, before.Successes+1, after.Successes)
	require.Equal(t, before.Failures+2, after.Failures)
	require.Less(t, after.SuccessRate(), float64(1))
}

func TestCheckpointHigherCallerRateRetainsCryptoBudget(t *testing.T) {
	const reserve = 20064 // 10000 indexes/s for two seconds, plus burst
	const largestMargin = 12702
	for _, profile := range testSRTPProfiles {
		t.Run(profileName(profile), func(t *testing.T) {
			keys := testSessionKeys(t, profile)
			for _, margin := range []uint16{largestMargin, largestMargin + 1} {
				old := testContext(t, keys.LocalMasterKey, keys.LocalMasterSalt, profile)
				receiver := testContext(t, keys.LocalMasterKey, keys.LocalMasterSalt, profile)
				const ssrc = 123
				_, err := receiver.DecryptRTP(nil, testEncrypt(t, old, ssrc, 60000), nil)
				require.NoError(t, err)
				out := testContext(t, keys.LocalMasterKey, keys.LocalMasterSalt, profile)
				track := trackState{MID: "video", SSRC: ssrc, Packets: 1, HighestSentIndex: 60000}
				before := track
				err = (&session{srtpOut: out}).resumeTrack(&track, ResumeOptions{SequenceMargin: margin, CallerSequenceReserve: reserve})
				if margin == largestMargin {
					require.NoError(t, err)
				} else {
					require.ErrorIs(t, err, ErrSequenceBudgetExhausted)
					require.Equal(t, before, track)
					// Independently exercise the half-space failure prevented by the
					// larger reserve, rather than trusting just the guard's return value.
					require.NoError(t, restoreOutboundIndex(out, ssrc, 60000+uint64(margin)))
				}
				seq := uint16((60000 + uint64(margin) + reserve + 1) & 0xffff)
				_, err = receiver.DecryptRTP(nil, testEncrypt(t, out, ssrc, seq), nil)
				if margin == largestMargin {
					require.NoError(t, err)
				} else {
					require.Error(t, err)
				}
			}
		})
	}
	state := sessionState{Audio: trackState{MID: "audio", Packets: 1}}
	attempts, err := state.sequenceResumeAttemptsWithReserve(8192, 10064)
	require.NoError(t, err)
	require.Equal(t, 2, attempts)
	attempts, err = state.sequenceResumeAttemptsWithReserve(8192, reserve)
	require.NoError(t, err)
	require.Equal(t, 1, attempts)
}

func TestCheckpointCallerReserveAlsoBoundsCacheReplay(t *testing.T) {
	w := newTestWorker(t)
	w.cfg.ReplayMaxBurstDuration = 100 * time.Millisecond
	sess := sessionFromState(w, sessionState{ID: "caller-reserve", Video: trackState{MID: "1", Packets: 10, Anchored: true, AdvanceSinceSend: 12100}})
	defer sess.close()
	sess.callerSequenceReserve = 20064
	before := sess.state.Video
	frame := framecache.Frame{Keyframe: true, Arrival: time.Now(), Packets: make([]framecache.Packet, 600)}
	require.False(t, sess.reserveReplay([]framecache.Frame{frame}, 8192), "cache reservation must fit the enlarged caller reserve")
	require.Equal(t, before, sess.state.Video)
	frame.Packets = frame.Packets[:512]
	require.True(t, sess.reserveReplay([]framecache.Frame{frame}, 8192))
	require.Less(t, uint64(sess.state.Video.AdvanceSinceSend)+uint64(sess.callerSequenceReserve)+1, uint64(1<<15))
}
