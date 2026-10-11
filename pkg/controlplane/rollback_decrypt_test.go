package controlplane_test

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/relais/internal/privateapi"
	"github.com/relais/internal/workerprobe"
	"github.com/relais/pkg/callharness"
	"github.com/relais/pkg/framecache"
	"github.com/relais/pkg/relay"
	"github.com/relais/pkg/sessionstore"
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
