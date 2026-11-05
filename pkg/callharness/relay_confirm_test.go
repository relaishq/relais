package callharness

import (
	"context"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/stun/v4"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

// staleReturnProxy fronts target for one caller. Caller packets pass; from
// the relay only STUN passes, so ICE connects but no media reaches the caller
// on this path. dropped counts discarded relay-to-caller media.
func staleReturnProxy(t *testing.T, target netip.AddrPort, dropped *atomic.Uint64) netip.AddrPort {
	t.Helper()
	front, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	back, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	var caller atomic.Pointer[netip.AddrPort]
	done := make(chan struct{}, 2)
	go func() {
		defer func() { done <- struct{}{} }()
		b := make([]byte, 2048)
		for {
			n, from, e := front.ReadFromUDPAddrPort(b)
			if e != nil {
				return
			}
			caller.Store(&from)
			_, _ = back.WriteToUDPAddrPort(b[:n], target)
		}
	}()
	go func() {
		defer func() { done <- struct{}{} }()
		b := make([]byte, 2048)
		for {
			n, _, e := back.ReadFromUDPAddrPort(b)
			if e != nil {
				return
			}
			to := caller.Load()
			if to == nil {
				continue
			}
			if !stun.IsMessage(b[:n]) {
				dropped.Add(1)
				continue
			}
			_, _ = front.WriteToUDPAddrPort(b[:n], *to)
		}
	}()
	t.Cleanup(func() {
		require.NoError(t, front.Close())
		require.NoError(t, back.Close())
		<-done
		<-done
	})
	return front.LocalAddr().(*net.UDPAddr).AddrPort()
}

func confirmationRecord(t *testing.T, store sessionstore.Store, id string) (sessionstore.RelayAssignment, sessionstore.RelayConfirmation) {
	t.Helper()
	a, rec, err := store.(sessionstore.RelayConfirmations).RelayConfirmation(context.Background(), id)
	require.NoError(t, err)
	require.NotNil(t, rec.Attempt)
	return a, rec
}

// ADR 0016 section 7: a planned change completes only on confirmation. The
// Pion caller declares C only; its receiver reports through the new relay
// must cover RTP sent after the switch on every required track. With
// MediaProgress the scripted A reporter completes a decode-mode call.
func TestRelayChangeConfirmation(t *testing.T) {
	for _, mode := range []string{sessionstore.RelayConfirmDelivery, sessionstore.RelayConfirmDecode} {
		t.Run(mode, func(t *testing.T) {
			forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
				p, w, h, relays, _ := relayChangeTopology(t, store, false, false)
				ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
				defer cancel()
				call, err := h.Dial(ctx, CallOptions{Video: true, RelayChange: true, MediaProgress: mode == sessionstore.RelayConfirmDecode})
				require.NoError(t, err)
				changes := 6
				if testing.Short() {
					changes = 2
				}
				sent := make(chan error, 1)
				go func() { sent <- call.SendMedia(ctx, time.Duration(3*changes+2)*time.Second) }()
				time.Sleep(time.Second)
				var latencies, afterCommit []time.Duration
				for i := range changes {
					target := []string{"r2", "r1"}[i%2]
					start := time.Now()
					require.NoError(t, p.ChangeRelay(ctx, call.SessionID(), target))
					done := time.Now()
					a, rec := confirmationRecord(t, store, call.SessionID())
					require.Equal(t, mode, rec.Mode)
					require.Equal(t, sessionstore.RelayPhaseConfirmed, rec.Attempt.Phase)
					require.Equal(t, sessionstore.RelayAssignment{Slot: target, Generation: uint64(i + 1)}, a)
					require.Equal(t, []string{"0", "1"}, rec.Attempt.Required, "audio and video are both required")
					if mode == sessionstore.RelayConfirmDecode {
						require.Equal(t, []string{"0", "1"}, rec.Attempt.Decoded)
					} else {
						require.ElementsMatch(t, []string{"0", "1"}, rec.Attempt.Delivered, "C needs every required track")
						require.Empty(t, rec.Attempt.Decoded)
					}
					latencies = append(latencies, done.Sub(start))
					afterCommit = append(afterCommit, done.Sub(rec.Attempt.CommitAt))
					require.Eventually(t, func() bool { return call.selectedRemoteAddr() == relays[(i+1)%2].PublicAddr().String() }, time.Second, 5*time.Millisecond)
				}
				require.NoError(t, <-sent)
				failures, err := w.SessionDecryptFailures(call.SessionID())
				require.NoError(t, err)
				require.Zero(t, failures)
				if mode == sessionstore.RelayConfirmDecode {
					require.GreaterOrEqual(t, call.progressReports.Load(), int64(changes))
				}
				report, err := call.Hangup(ctx)
				require.NoError(t, err)
				require.Zero(t, report.DecryptionFailures.Total())
				for _, track := range report.Tracks {
					require.Zero(t, track.DuplicatePackets)
				}
				t.Logf("CONFIRM mode=%s changes=%d event_to_confirmed p50=%s p95=%s max=%s commit_to_confirmed p50=%s max=%s", mode, changes,
					percentile(latencies, 50).Round(time.Millisecond), percentile(latencies, 95).Round(time.Millisecond), percentile(latencies, 100).Round(time.Millisecond),
					percentile(afterCommit, 50).Round(time.Millisecond), percentile(afterCommit, 100).Round(time.Millisecond))
			})
		})
	}
}

// "Connected but media stale": ICE connects on the new relay, but no media
// reaches the caller there. A committed switch is not success; without C (or
// A) the planned change rolls back to the old slot and keeps the call.
func TestRelayConnectedButStaleRollsBack(t *testing.T) {
	for _, mode := range []string{sessionstore.RelayConfirmDelivery, sessionstore.RelayConfirmDecode} {
		t.Run(mode, func(t *testing.T) {
			if testing.Short() && mode == sessionstore.RelayConfirmDelivery {
				t.Skip("C mode waits for the 10 s planned deadline; the A-mode case keeps the regression in -short")
			}
			forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
				p, w, h, relays, dropped := relayChangeTopology(t, store, false, false, true)
				ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
				defer cancel()
				call, err := h.Dial(ctx, CallOptions{Video: true, RelayChange: true, MediaProgress: mode == sessionstore.RelayConfirmDecode})
				require.NoError(t, err)
				sent := make(chan error, 1)
				media := 18 * time.Second
				if mode == sessionstore.RelayConfirmDecode {
					media = 9 * time.Second
				}
				go func() { sent <- call.SendMedia(ctx, media) }()
				time.Sleep(time.Second)
				start := time.Now()
				err = p.ChangeRelay(ctx, call.SessionID(), "r2")
				elapsed := time.Since(start)
				require.ErrorContains(t, err, "unconfirmed", "a connected but stale path must never count as success")
				// A mode rolls back inside the planned window (3 s attempt
				// bound). C mode has no A timer: still switched and unconfirmed
				// at the 10 s planned deadline, the call enters a stall episode,
				// whose first untried live slot is the original one.
				var a sessionstore.RelayAssignment
				var rec sessionstore.RelayConfirmation
				require.Eventually(t, func() bool {
					a, rec = confirmationRecord(t, store, call.SessionID())
					return a.Slot == "r1" && a.Pending == "" && rec.Attempt.Phase == sessionstore.RelayPhaseConfirmed
				}, 5*time.Second, 10*time.Millisecond)
				recovered := time.Since(start)
				require.GreaterOrEqual(t, a.Generation, uint64(2), "rollback uses a higher generation")
				require.Equal(t, sessionstore.RelayPhaseConfirmed, rec.Attempt.Phase, "the rollback is confirmed under the same mode")
				require.Equal(t, "r1", rec.Attempt.Slot)
				require.Positive(t, dropped.Load(), "the new path carried worker media that never reached the caller")
				require.Eventually(t, func() bool { return call.selectedRemoteAddr() == relays[0].PublicAddr().String() }, time.Second, 5*time.Millisecond)
				before := call.rec.report()
				time.Sleep(500 * time.Millisecond)
				after := call.rec.report()
				for i, track := range after.Tracks {
					require.Greater(t, track.Packets, before.Tracks[i].Packets, "caller media must progress after rollback")
				}
				require.NoError(t, <-sent)
				failures, err := w.SessionDecryptFailures(call.SessionID())
				require.NoError(t, err)
				require.Zero(t, failures)
				report, err := call.Hangup(ctx)
				require.NoError(t, err)
				require.Zero(t, report.DecryptionFailures.Total())
				for _, track := range report.Tracks {
					require.Zero(t, track.DuplicatePackets)
					t.Logf("STALE_ROLLBACK mode=%s track=%s caller_gap=%s", mode, track.Kind, track.MediaGap)
				}
				t.Logf("STALE_ROLLBACK mode=%s change_returned=%s event_to_confirmed_on_old_slot=%s generation=%d dropped=%d", mode, elapsed.Round(time.Millisecond), recovered.Round(time.Millisecond), a.Generation, dropped.Load())
			})
		})
	}
}
