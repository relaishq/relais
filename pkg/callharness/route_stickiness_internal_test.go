package callharness

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/pion/sdp/v3"
	"github.com/pion/stun/v4"
	"github.com/stretchr/testify/require"
)

// TestAuthenticatedSpoofCannotMoveRelayRoute is #20 end to end: an
// attacker holding valid ICE credentials for its own call B sends B's
// checks from the victim A's address while A is active. A's media must keep
// flowing to A's worker throughout: through the relay, whether B is on
// another worker or on A's own, and right after a planned move of A, which
// must carry A's activity over to its new worker. On a shared socket after
// a planned move, the socket's own record of A's checks is all that
// protects the address: A's new worker has not seen a check yet, and A's
// old worker, where B runs, has forgotten A.
func TestAuthenticatedSpoofCannotMoveRelayRoute(t *testing.T) {
	for _, tc := range []struct {
		name     string
		relay    bool // through the relay, or else on a shared socket
		attacker int  // the attacker's worker; the victim starts on worker 0
		move     bool // move the victim to worker 1 just before the attack
	}{
		{name: "different workers", relay: true, attacker: 1},
		{name: "same worker", relay: true, attacker: 0},
		{name: "after a planned move", relay: true, attacker: 0, move: true},
		{name: "shared socket after a planned move", attacker: 0, move: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, err := Start(Options{Relay: tc.relay, Workers: 2})
			require.NoError(t, err)
			t.Cleanup(func() { _ = h.Close() })
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			t.Cleanup(cancel)
			victim, err := h.Dial(ctx, CallOptions{})
			require.NoError(t, err)
			attacker, err := h.Dial(ctx, CallOptions{Worker: tc.attacker})
			require.NoError(t, err)
			var answer sdp.SessionDescription
			require.NoError(t, answer.UnmarshalString(attacker.pc.RemoteDescription().SDP))
			pwd, ok := answer.MediaDescriptions[0].Attribute("ice-pwd")
			require.True(t, ok)
			if tc.move {
				require.NoError(t, victim.Handover(HandoverOptions{To: 1}))
			}
			target := h.RelayAddr()
			if !tc.relay {
				target = h.workers.socket.LocalAddr()
			}
			sent := make(chan error, 1)
			go func() { sent <- victim.SendMedia(ctx, 3*time.Second) }()
			// Use the victim's actual socket as a test-only source injection seam.
			// No production hook or fake worker response is involved: B's ICE
			// credentials already established a real call on its own address.
			for range 20 {
				request, err := stun.Build(stun.TransactionID, stun.BindingRequest,
					stun.NewUsername(attacker.remoteUfrag+":"+attacker.localUfrag),
					stun.RawAttribute{Type: stun.AttrUseCandidate},
					stun.NewShortTermIntegrity(pwd), stun.Fingerprint)
				require.NoError(t, err)
				_, err = victim.socket.observer.conn.WriteTo(request.Raw, net.UDPAddrFromAddrPort(target))
				require.NoError(t, err)
				time.Sleep(50 * time.Millisecond)
			}
			require.NoError(t, <-sent)
			report, err := victim.Hangup(ctx)
			require.NoError(t, err)
			track := report.Track("audio")
			require.NotNil(t, track, "victim still receives decrypted echo after the attack")
			// A hijack stops the victim's echo for the rest of the attack: before
			// the fix, about 51 of the 150 packets came back.
			require.GreaterOrEqual(t, track.Packets, 140, "victim receives media throughout the attack")
			require.Less(t, track.MediaGap, 250*time.Millisecond)
			t.Logf("SPOOF_METRICS case=%q audio_packets=%d first=%s last=%s max_gap=%s",
				tc.name, track.Packets, track.FirstArrival, track.LastArrival, track.MediaGap)
			require.True(t, report.ConnectedThroughout())
			require.Zero(t, report.DecryptionFailures.Total())
			require.Zero(t, report.ICERestarts)
			require.Zero(t, report.Renegotiations)
			_, err = attacker.Hangup(ctx)
			require.NoError(t, err)
		})
	}
}
