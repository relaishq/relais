package callharness

import (
	"context"
	"net/http/httptest"
	"testing"

	"time"

	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/relay"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

func TestPlannedRelayChangeLive(t *testing.T) {
	forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		var relays []*relay.Relay
		var bindings []controlplane.RelayBinding
		for _, id := range []string{"r1", "r2"} {
			r, err := relay.New(relay.Config{Owners: store, SlotID: id})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, r.Close()) })
			relays = append(relays, r)
			bindings = append(bindings, controlplane.RelayBinding{Slot: mediaworker.RelaySlot{ID: id, Leg: r.WorkerAddr(), Public: r.PublicAddr()}, Relay: r})
		}
		w, err := mediaworker.New(mediaworker.Config{Relay: &mediaworker.RelayConfig{Owners: store, Addr: relays[0].WorkerAddr(), PublicAddr: relays[0].PublicAddr()}})
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, w.Close()) })
		p := controlplane.New(relays[0], store)
		require.NoError(t, p.Register("0", w.LocalAddr(), w))
		for _, r := range relays {
			r.AddWorker(w.LocalAddr())
		}
		require.NoError(t, p.ConfigureRelaySet(ctx, bindings))
		server := httptest.NewServer(p.Handler())
		t.Cleanup(server.Close)
		h, err := Start(Options{External: &ExternalTopology{SignalingURL: server.URL + "/calls", RelayAddr: relays[0].PublicAddr()}})
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, h.Close()) })
		call, err := h.Dial(ctx, CallOptions{Video: true, RelayChange: true})
		require.NoError(t, err)
		sent := make(chan error, 1)
		changes := 20
		if testing.Short() {
			changes = 4
		}
		go func() { sent <- call.SendMedia(ctx, time.Duration(changes+3)*time.Second) }()
		time.Sleep(time.Second)
		baseline := call.rec.report()
		pauses := map[string][]time.Duration{}
		for i := range changes {
			start := time.Since(call.rec.start)
			target := 1 - i%2
			require.NoError(t, p.ChangeRelay(ctx, call.SessionID(), bindings[target].Slot.ID))
			require.Eventually(t, func() bool { return call.selectedRemoteAddr() == relays[target].PublicAddr().String() }, time.Second, 5*time.Millisecond)
			time.Sleep(550 * time.Millisecond)
			old := relays[1-target].Stats().WorkerPackets
			time.Sleep(100 * time.Millisecond)
			require.Equal(t, old, relays[1-target].Stats().WorkerPackets, "old slot must receive no worker output after switch+500ms")
			for _, track := range call.rec.report().Tracks {
				var gap time.Duration
				for j := 1; j < len(track.Arrivals); j++ {
					if track.Arrivals[j] >= start {
						gap = max(gap, track.Arrivals[j]-track.Arrivals[j-1])
					}
				}
				pauses[track.Kind] = append(pauses[track.Kind], gap)
			}
		}
		require.NoError(t, <-sent)
		failures, err := w.SessionDecryptFailures(call.SessionID())
		require.NoError(t, err)
		require.Zero(t, failures)
		report, err := call.Hangup(ctx)
		require.NoError(t, err)
		require.Zero(t, report.DecryptionFailures.Total())
		for _, track := range report.Tracks {
			t.Logf("LIVE_RELAY_CHANGE track=%s gap=%s packets=%d duplicates=%d", track.Kind, track.MediaGap, track.Packets, track.DuplicatePackets)
			require.Zero(t, track.DuplicatePackets)
			require.Positive(t, track.Packets)
		}
		for _, track := range baseline.Tracks {
			base := track.MediaGap
			p95 := percentile(pauses[track.Kind], 95)
			t.Logf("RELAY_SAMPLES track=%s gaps=%v", track.Kind, pauses[track.Kind])
			t.Logf("RELAY_CHANGE_PAUSES track=%s n=%d p50=%s p95=%s max=%s baseline_max=%s target_met=%t", track.Kind, len(pauses[track.Kind]), percentile(pauses[track.Kind], 50), p95, percentile(pauses[track.Kind], 100), base, p95 <= 150*time.Millisecond && p95 <= base+50*time.Millisecond)
			require.LessOrEqual(t, p95, 150*time.Millisecond)
			require.LessOrEqual(t, p95, base+50*time.Millisecond)
		}
		call.dialMu.Lock()
		connected := 0
		for _, state := range call.dtlsStates {
			if state.State == "connected" {
				connected++
			}
		}
		call.dialMu.Unlock()
		require.Equal(t, 1, connected, "DTLS association must stay")
		t.Log(report.Summary())
	})
}
