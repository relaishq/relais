package callharness

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

type blindRelayView struct {
	sessionstore.Store
	sessionstore.RelayAssignments
	sessionstore.RelayLeases
	sessionstore.ControlStore
	blind  atomic.Bool
	frozen sessionstore.RelayLease
}

func (s *blindRelayView) GetRelay(ctx context.Context, key string) (sessionstore.RelayLease, error) {
	lease, err := s.RelayLeases.GetRelay(ctx, key)
	if err == nil && key == "r1" && s.blind.Load() {
		return s.frozen, nil
	}
	return lease, err
}

func TestWrongRelayLossKeepsLiveCall(t *testing.T) {
	forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
		view := &blindRelayView{Store: store, RelayAssignments: store.(sessionstore.RelayAssignments), RelayLeases: store.(sessionstore.RelayLeases), ControlStore: store.(sessionstore.ControlStore)}
		p, w, h, _, _ := relayChangeTopology(t, view, false, false)
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()
		identity := sessionstore.RelayProcess{Owner: "live-relay", PID: 42, Start: "start", Host: "host", Namespace: "namespace", Executable: "relay"}
		first, err := view.ClaimRelay(ctx, "r1", identity, time.Minute)
		require.NoError(t, err)
		second, err := view.ClaimRelay(ctx, "r2", identity, time.Minute)
		require.NoError(t, err)
		view.frozen = first
		call, err := h.Dial(ctx, CallOptions{Video: true, RelayChange: true})
		require.NoError(t, err)
		life, stop := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			tick := time.NewTicker(50 * time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case <-life.Done():
					return
				case <-tick.C:
					_ = p.Heartbeat(w.LocalAddr())
					first, _ = view.RenewRelay(life, first, time.Minute)
					second, _ = view.RenewRelay(life, second, time.Minute)
				}
			}
		}()
		defer func() { stop(); <-done }()
		runDone := make(chan error, 1)
		require.NoError(t, p.Heartbeat(w.LocalAddr()))
		go func() { runDone <- p.Run(life) }()
		defer func() { stop(); require.NoError(t, <-runDone) }()
		media, stopMedia := context.WithCancel(ctx)
		sent := make(chan error, 1)
		go func() { sent <- call.SendMedia(media, 8*time.Second) }()
		defer func() { stopMedia(); <-sent }()
		require.Eventually(t, func() bool {
			r := call.rec.report()
			return len(r.Tracks) == 2 && r.Tracks[0].Packets > 30 && r.Tracks[1].Packets > 30
		}, 2*time.Second, 10*time.Millisecond)
		baseline := call.rec.report()
		view.blind.Store(true)
		require.Eventually(t, func() bool {
			_, a, e := store.(sessionstore.RelayAssignments).RelayOwner(ctx, call.SessionID())
			return e == nil && a.Slot == "r2" && a.Pending == ""
		}, 4*time.Second, 10*time.Millisecond)
		observed := time.Now()
		require.Eventually(t, func() bool { return call.RelayRecoveryGap(observed, observed) > 0 }, time.Second, 5*time.Millisecond)
		report := call.rec.report()
		for _, track := range report.Tracks {
			for _, base := range baseline.Tracks {
				if track.Kind == base.Kind {
					limit := min(150*time.Millisecond, base.MediaGap+50*time.Millisecond)
					require.LessOrEqual(t, track.MediaGap, relayTimingLimit(limit))
					t.Logf("WRONG_RELAY_LOSS row=5 track=%s n=1 gap=%s baseline=%s", track.Kind, track.MediaGap, base.MediaGap)
				}
			}
			require.Zero(t, track.DuplicatePackets)
		}
		require.Zero(t, report.DecryptionFailures.Total())
		failures, err := w.SessionDecryptFailures(call.SessionID())
		require.NoError(t, err)
		require.Zero(t, failures)
	})
}
