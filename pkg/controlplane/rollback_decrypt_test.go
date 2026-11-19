package controlplane_test

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/relaishq/relais/internal/privateapi"
	"github.com/relaishq/relais/internal/workerprobe"
	"github.com/relaishq/relais/pkg/agent"
	"github.com/relaishq/relais/pkg/callharness"
	"github.com/relaishq/relais/pkg/framecache"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/relay"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

func TestHTTPUncertainRollbackDecrypt(t *testing.T) {
	forHTTPRecoveryStores(t, func(t *testing.T, store sessionstore.Store) {
		for _, waitForMedia := range []bool{false, true} {
			for _, sequence := range []uint16{1000, 31000, 57000, 65000} {
				t.Run(fmt.Sprintf("target-media=%t/sequence=%d", waitForMedia, sequence), func(t *testing.T) {
					source := &lostReply{path: "/never"}
					target := &lostReply{path: "/resume", failure: errors.New("lost resume reply")}
					s := newHTTPSystemWithSettings(t, source, target, nil, httpSystemSettings{
						holdTimeout: 20 * time.Millisecond,
						store:       store,
						buffer:      &relay.BufferConfig{},
						cache:       framecache.NewMemory(framecache.Limits{}),
						diagnostics: true,
					})
					require.NoError(t, workerprobe.SetInitialSequences(s.workers[0].LocalAddr(), sequence, sequence))
					var targetAudio atomic.Uint64
					require.NoError(t, workerprobe.SetAfterEcho(s.workers[1].LocalAddr(), func(_ context.Context, _ string, packet []byte) {
						var header rtp.Header
						if _, err := header.Unmarshal(packet); err == nil && header.PayloadType == 111 {
							targetAudio.Add(1)
						}
					}))
					if waitForMedia {
						target.afterCommit = func() {
							require.Eventually(t, func() bool { return targetAudio.Load() >= 3 }, time.Second, time.Millisecond)
						}
					}
					report := flowingCall(t, s, func(ctx context.Context, call *callharness.Call) {
						target.armed.Store(true)
						move, err := s.plane.Move(ctx, call.SessionID(), "1")
						require.ErrorIs(t, err, privateapi.ErrUncertain)
						require.True(t, move.Result.RolledBack)
						if waitForMedia {
							require.GreaterOrEqual(t, targetAudio.Load(), uint64(3))
						}
					})
					require.NotNil(t, report.SRTPDiagnostics)
					require.True(t, report.SRTPDiagnostics.Ready, report.SRTPDiagnostics.Unavailable)
					require.Greater(t, report.SRTPDiagnostics.Packets, uint64(100))
					require.Zero(t, report.SRTPDiagnostics.Skipped)
					require.Equal(t, report.DecryptionFailures, report.SRTPDiagnostics.Failures)
				})
			}
		}
	})
}

// wrapWatch follows one echoed track's RTP sequence numbers on one worker.
type wrapWatch struct {
	payloadType uint8
	first, last atomic.Uint32 // sequence number + 1; zero before the first packet
	high        atomic.Bool   // a sequence number in the upper half was echoed
	wrapped     atomic.Uint32 // lower-half packets echoed after the upper half
}

func (w *wrapWatch) observe(_ context.Context, _ string, packet []byte) {
	var header rtp.Header
	if _, err := header.Unmarshal(packet); err != nil || header.PayloadType != w.payloadType {
		return
	}
	w.first.CompareAndSwap(0, uint32(header.SequenceNumber)+1)
	w.last.Store(uint32(header.SequenceNumber) + 1)
	if header.SequenceNumber >= 1<<15 {
		w.high.Store(true)
	} else if w.high.Load() {
		w.wrapped.Add(1)
	}
}

// Force the target's echoed track to the 16-bit RTP sequence wrap before the
// uncertain move rolls back (#474). In "cross" the target sends three packets
// past the wrap; in "before" it reaches the last four sequence numbers below
// it. The source then resumes from the target's fenced checkpoint with crash
// margins, and the caller must accept every packet of both owners.
func TestHTTPUncertainRollbackAfterTargetWrap(t *testing.T) {
	const approach = 24 // packets below the wrap the source reaches before the move
	const headroom = 64 // more than the source echoes before the move starts
	forHTTPRecoveryStores(t, func(t *testing.T, store sessionstore.Store) {
		for _, track := range []struct {
			name        string
			payloadType uint8
		}{{"audio", 111}, {"video", 96}} {
			for _, cross := range []bool{true, false} {
				phase := map[bool]string{true: "cross", false: "before"}[cross]
				t.Run(track.name+"/"+phase, func(t *testing.T) {
					source := &lostReply{path: "/never"}
					target := &lostReply{path: "/resume", failure: errors.New("lost resume reply")}
					s := newHTTPSystemWithSettings(t, source, target, nil, httpSystemSettings{
						holdTimeout: 20 * time.Millisecond,
						store:       store,
						buffer:      &relay.BufferConfig{},
						cache:       framecache.NewMemory(framecache.Limits{}),
						diagnostics: true,
					})
					audio, video := uint16(1000), uint16(1000)
					if track.payloadType == 111 {
						audio = 1<<16 - approach - headroom
					} else {
						video = 1<<16 - approach - headroom
					}
					require.NoError(t, workerprobe.SetInitialSequences(s.workers[0].LocalAddr(), audio, video))
					sourceWatch := &wrapWatch{payloadType: track.payloadType}
					targetWatch := &wrapWatch{payloadType: track.payloadType}
					require.NoError(t, workerprobe.SetAfterEcho(s.workers[0].LocalAddr(), sourceWatch.observe))
					require.NoError(t, workerprobe.SetAfterEcho(s.workers[1].LocalAddr(), targetWatch.observe))
					reached := func() bool {
						if cross {
							return targetWatch.wrapped.Load() >= 3
						}
						return targetWatch.wrapped.Load() > 0 || targetWatch.last.Load() > 1<<16-4
					}
					var ready atomic.Bool
					target.afterCommit = func() {
						// Stay inside the 2 s worker client timeout.
						deadline := time.Now().Add(1500 * time.Millisecond)
						for !reached() && time.Now().Before(deadline) {
							time.Sleep(time.Millisecond)
						}
						ready.Store(reached())
					}
					report := flowingCall(t, s, func(ctx context.Context, call *callharness.Call) {
						require.Eventually(t, func() bool { return sourceWatch.last.Load() > 1<<16-approach }, 3*time.Second, time.Millisecond)
						require.Zero(t, sourceWatch.wrapped.Load(), "the source must not wrap before the move")
						target.armed.Store(true)
						move, err := s.plane.Move(ctx, call.SessionID(), "1")
						require.ErrorIs(t, err, privateapi.ErrUncertain)
						require.True(t, move.Result.RolledBack)
						progress := fmt.Sprintf("target %s: sequences %d to %d, %d packets past the wrap", track.name,
							int(targetWatch.first.Load())-1, int(targetWatch.last.Load())-1, targetWatch.wrapped.Load())
						require.True(t, ready.Load(), "target media must reach the wrap before rollback; %s", progress)
						t.Log(progress)
						// Rollback resumes the source as a takeover from the target's
						// fenced checkpoint, with the default crash margins.
						resumes := source.requests()
						require.Len(t, resumes, 1)
						require.Equal(t, agent.Takeover, resumes[0].Kind)
						require.EqualValues(t, 8192, resumes[0].SequenceMargin)
						require.EqualValues(t, 128, resumes[0].SRTCPIndexMargin)
						require.GreaterOrEqual(t, resumes[0].CallerSequenceReserve, uint32(mediaworker.SequenceGapReserve))
					})
					require.NotNil(t, report.SRTPDiagnostics)
					require.True(t, report.SRTPDiagnostics.Ready, report.SRTPDiagnostics.Unavailable)
					require.Greater(t, report.SRTPDiagnostics.Packets, uint64(100))
					require.Zero(t, report.SRTPDiagnostics.Skipped)
					require.Equal(t, report.DecryptionFailures, report.SRTPDiagnostics.Failures)
				})
			}
		}
	})
}
