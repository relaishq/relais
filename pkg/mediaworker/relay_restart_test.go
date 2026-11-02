package mediaworker

import (
	"context"
	"encoding/json"
	"net"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pion/stun/v4"
	"github.com/relaishq/relais/pkg/relay"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

func TestRelayRestartSnapshotKeepsVersion(t *testing.T) {
	golden, err := os.ReadFile("testdata/v7-787127e.json")
	require.NoError(t, err)
	snap, err := decodeSnapshot(golden)
	require.NoError(t, err)
	snap.Version, snap.State.Version = relaySessionStateVersion, relaySessionStateVersion
	snap.State.ICE.LocalUfrag = snap.State.ID + "+1"
	snap.State.ICE.Relay = &RelaySlot{ID: "r2", Leg: netip.MustParseAddrPort("127.0.0.1:1234"), Public: netip.MustParseAddrPort("127.0.0.1:1235")}
	wire, err := json.Marshal(snap)
	require.NoError(t, err)
	_, err = frozenV020DecodeSnapshot(wire)
	require.ErrorIs(t, err, errStateVersion)
	decoded, err := decodeSnapshot(wire)
	require.NoError(t, err)
	require.Equal(t, relaySessionStateVersion, decoded.Version)
	require.Equal(t, snap.State.ICE, decoded.State.ICE)
}

func TestPendingRelayOnlyAnswersChecksUntilNomination(t *testing.T) {
	forV2Stores(t, func(t *testing.T, store sessionstore.Store) {
		ctx := context.Background()
		var sockets []*net.UDPConn
		var slots []RelaySlot
		for _, id := range []string{"r1", "r2", "r3"} {
			sock := listenTestUDP(t)
			sockets = append(sockets, sock)
			addr := sock.LocalAddr().(*net.UDPAddr).AddrPort()
			slots = append(slots, RelaySlot{ID: id, Leg: addr, Public: addr})
		}
		w, err := New(Config{Relay: &RelayConfig{Owners: store, Addr: slots[0].Leg, PublicAddr: slots[0].Public}})
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, w.Close()) })
		require.NoError(t, w.ConfigureRelays(ctx, slots))
		offer := testOffer(testOfferAttrs{audio: []string{"a=ssrc:123 cname:caller"}})
		id, answer, err := w.CreateSession(ctx, offer)
		require.NoError(t, err)
		s := w.session(id)
		caller := netip.MustParseAddrPort("192.0.2.9:4567")
		check := func(ufrag, remote, pwd string, nominate bool) []byte {
			attrs := []stun.Setter{stun.BindingRequest, stun.TransactionID, stun.NewUsername(ufrag + ":" + remote)}
			if nominate {
				attrs = append(attrs, stun.RawAttribute{Type: stun.AttrUseCandidate})
			}
			attrs = append(attrs, stun.NewShortTermIntegrity(pwd), stun.Fingerprint)
			msg, e := stun.Build(attrs...)
			require.NoError(t, e)
			return msg.Raw
		}
		read := func(index int) []byte {
			require.NoError(t, sockets[index].SetReadDeadline(time.Now().Add(2*time.Second)))
			buf := make([]byte, receiveMTU)
			n, e := sockets[index].Read(buf)
			require.NoError(t, e)
			to, pkt, e := relay.ParseHeader(buf[:n])
			require.NoError(t, e)
			require.Equal(t, caller, to)
			return pkt
		}
		quiet := func(index int) {
			require.NoError(t, sockets[index].SetReadDeadline(time.Now().Add(200*time.Millisecond)))
			_, e := sockets[index].Read(make([]byte, receiveMTU))
			require.Error(t, e)
			require.True(t, e.(net.Error).Timeout())
		}
		w.handleSTUNVia(check(id, testCallerUfrag, answerPwd(t, answer), true), caller, slots[0].Leg)
		require.True(t, stun.IsMessage(read(0)))
		a := store.(sessionstore.RelayAssignments)
		lease, from, err := a.RelayOwner(ctx, id)
		require.NoError(t, err)
		pending := from
		pending.Pending = "r2"
		require.NoError(t, a.CASRelay(ctx, lease, from, pending))
		restart := strings.ReplaceAll(strings.ReplaceAll(offer, testCallerUfrag, "newufrag"), testCallerPwd, "newpasswordnewpasswordnewpassword")
		answer, err = w.RestartSession(ctx, id, RestartRequest{Offer: restart, Generation: 1, Slot: "r2"})
		require.NoError(t, err)
		ufrag, pwd := answerCredentials(t, answer)
		w.handleSTUNVia(check(ufrag, "newufrag", pwd, false), caller, slots[1].Leg)
		require.True(t, stun.IsMessage(read(1)), "pending relay receives binding success")
		media := []byte{0x80, 111, 0, 1}
		s.mu.Lock()
		_, err = s.send(media, caller)
		s.mu.Unlock()
		require.NoError(t, err)
		require.Equal(t, media, read(0))
		quiet(1)
		w.handleSTUNVia(check(ufrag, "newufrag", pwd, true), caller, slots[2].Leg)
		quiet(2)
		w.handleSTUNVia(check(ufrag, "newufrag", pwd, true), caller, slots[1].Leg)
		require.True(t, stun.IsMessage(read(1)))
		s.mu.Lock()
		_, err = s.send(media, caller)
		s.mu.Unlock()
		require.NoError(t, err)
		require.Equal(t, media, read(1))
		quiet(0)
		require.Eventually(t, func() bool {
			_, got, e := a.RelayOwner(ctx, id)
			return e == nil && got.Generation == 1 && got.Slot == "r2"
		}, 2*time.Second, time.Millisecond)
		t.Log("FENCE_COUNTERS pending_R2_binding_responses=1 pending_R2_media=0 nonassigned_R3_packets=0 post_switch_R1_packets=0")
	})
}

func TestRestartIdentityAllowsNegotiatedFeedbackButPinsMedia(t *testing.T) {
	base := testOffer(testOfferAttrs{audio: []string{"a=ssrc:123 cname:caller", "a=extmap:1 urn:example:extension"}})
	want, err := restartIdentity(base)
	require.NoError(t, err)
	got, err := restartIdentity(strings.ReplaceAll(base, "a=extmap:1 urn:example:extension\r\n", ""))
	require.NoError(t, err)
	require.Equal(t, want, got)
	for _, changed := range []string{strings.ReplaceAll(base, "ssrc:123", "ssrc:456"), strings.ReplaceAll(base, "opus/48000/2", "opus/8000/2"), strings.ReplaceAll(base, "AB:", "CD:")} {
		got, err := restartIdentity(changed)
		require.NoError(t, err)
		require.NotEqual(t, want, got)
	}
}
