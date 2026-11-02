package mediaworker

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/pion/stun/v4"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

func TestLostRelaySetCutsOffBothDirections(t *testing.T) {
	forV2Stores(t, func(t *testing.T, store sessionstore.Store) {
		ctx := context.Background()
		var slots []RelaySlot
		for _, id := range []string{"r1", "r2"} {
			sock := listenTestUDP(t)
			addr := sock.LocalAddr().(*net.UDPAddr).AddrPort()
			slots = append(slots, RelaySlot{ID: id, Leg: addr, Public: addr})
		}
		w, err := New(Config{Relay: &RelayConfig{Owners: store, Addr: slots[0].Leg, PublicAddr: slots[0].Public}})
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, w.Close()) })
		require.NoError(t, w.ConfigureRelays(ctx, slots))
		id, answer, err := w.CreateSession(ctx, testOffer(testOfferAttrs{}))
		require.NoError(t, err)
		s := w.session(id)
		caller := netip.MustParseAddrPort("192.0.2.9:4567")
		check, err := stun.Build(stun.BindingRequest, stun.TransactionID, stun.NewUsername(id+":"+testCallerUfrag), stun.RawAttribute{Type: stun.AttrUseCandidate}, stun.NewShortTermIntegrity(answerPwd(t, answer)), stun.Fingerprint)
		require.NoError(t, err)
		w.handleSTUNVia(check.Raw, caller, slots[0].Leg)
		// The same path is present in every permission class when control revokes it.
		s.mu.Lock()
		s.previousRelay, s.previousUntil = relayPath{slots[0].Leg, caller}, time.Now().Add(time.Hour)
		require.True(t, s.acceptsRelayPath(relayPath{slots[0].Leg, caller}, true))
		s.mu.Unlock()
		require.NoError(t, w.ConfigureRelays(ctx, slots[1:]), "a live worker must remove the lost bootstrap slot")
		s.mu.Lock()
		require.False(t, s.acceptsRelayPath(relayPath{slots[0].Leg, caller}, true))
		s.mu.Unlock()
		require.Error(t, s.answerBindingRequest(check, id, testCallerUfrag, caller, slots[0].Leg))
		_, err = w.sendVia([]byte{0x80, 111}, caller, slots[0].Leg)
		require.Error(t, err, "lost slot cannot receive worker output")
		require.Error(t, w.ConfigureRelays(ctx, slots), "a frozen instance cannot be readmitted")
	})
}
