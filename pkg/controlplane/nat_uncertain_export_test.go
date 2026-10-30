package controlplane_test

import (
	"context"
	"net/http"
	"net/netip"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/callharness"
	"github.com/relaishq/relais/pkg/relay"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

func TestNATDrainUncertainExportPermitsImmediateMoves(t *testing.T) {
	forHTTPRecoveryStores(t, func(t *testing.T, store sessionstore.Store) {
		var nat *udpNAT
		var drop atomic.Bool
		s := newHTTPSystemWithSettings(t, nil, nil, nil, httpSystemSettings{
			store: store, dialOut: true, dropLinkExport: &drop, buffer: &relay.BufferConfig{},
			targetTransport: natTransportFunc(func(req *http.Request) (*http.Response, error) {
				resp, err := http.DefaultTransport.RoundTrip(req)
				if err == nil && req.URL.Path == "/resume" {
					// Keep the applied reply in flight across normal recovery
					// observation while the gate is being promoted.
					time.Sleep(100 * time.Millisecond)
				}
				return resp, err
			}),
			sourceLeg: func(a netip.AddrPort) netip.AddrPort { nat = newUDPNAT(t, a); return nat.WorkerAddr() },
		})
		report := flowingCall(t, s, func(ctx context.Context, c *callharness.Call) {
			drop.Store(true)
			next := nat.rebind(t)
			require.Eventually(t, func() bool {
				status, e := s.plane.Status(ctx)
				return e == nil && len(status.Calls) == 1 && status.Calls[0].Owner == "1" && rebindReady(status, next)
			}, 6*time.Second, 10*time.Millisecond)
			_, e := s.plane.Move(ctx, c.SessionID(), "0")
			require.NoError(t, e)
			_, e = s.plane.Move(ctx, c.SessionID(), "1")
			require.NoError(t, e)
		})
		require.Zero(t, report.DecryptionFailures.Total())
		video := report.Track("video").Video
		// The decoder sees the reversed timestamps; without ffmpeg the relay
		// replay test still covers the retained pre-barrier input.
		if os.Getenv("RELAIS_HARNESS_REQUIRE_FFMPEG") != "" {
			require.True(t, video.FullDecode.Ran())
		}
		if video.FullDecode.Ran() {
			require.Empty(t, video.FullDecode.Errors)
			require.Equal(t, video.FullDecode.FramesIn, video.FullDecode.FramesDecoded)
		}
	})
}
