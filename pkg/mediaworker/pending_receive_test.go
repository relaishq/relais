package mediaworker

import (
	"context"
	"net"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/srtp/v3"
	"github.com/pion/stun/v4"
	"github.com/relaishq/relais/internal/relayleg"
	"github.com/relaishq/relais/pkg/relay"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

func TestPendingRelayAuthenticatedMediaSwitch(t *testing.T) {
	for _, switchMedia := range []bool{true, false} {
		t.Run(map[bool]string{true: "media", false: "rollback"}[switchMedia], func(t *testing.T) {
			forV2Stores(t, func(t *testing.T, store sessionstore.Store) {
				ctx := context.Background()
				commits := &relayCommitFault{Store: store, RelayAssignments: store.(sessionstore.RelayAssignments), attempts: make(chan struct{}, 16)}
				commits.block.Store(switchMedia)
				store = commits
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
				caller := netip.MustParseAddrPort("192.0.2.9:4567")
				send := func(index int, p []byte) {
					_, e := sockets[index].WriteToUDPAddrPort(append(relay.AppendHeader(nil, caller), p...), w.LocalAddr())
					require.NoError(t, e)
				}
				rawRead := func(index int) []byte {
					require.NoError(t, sockets[index].SetReadDeadline(time.Now().Add(2*time.Second)))
					b := make([]byte, receiveMTU)
					n, e := sockets[index].Read(b)
					require.NoError(t, e)
					return b[:n]
				}
				read := func(index int) []byte {
					to, p, e := relay.ParseHeader(rawRead(index))
					require.NoError(t, e)
					require.Equal(t, caller, to)
					return p
				}
				quiet := func(index int) {
					require.NoError(t, sockets[index].SetReadDeadline(time.Now().Add(200*time.Millisecond)))
					_, e := sockets[index].Read(make([]byte, receiveMTU))
					require.Error(t, e)
					require.True(t, e.(net.Error).Timeout())
				}
				check := func(local, remote, pwd string, nominate bool) []byte {
					attrs := []stun.Setter{stun.BindingRequest, stun.TransactionID, stun.NewUsername(local + ":" + remote)}
					if nominate {
						attrs = append(attrs, stun.RawAttribute{Type: stun.AttrUseCandidate})
					}
					attrs = append(attrs, stun.NewShortTermIntegrity(pwd), stun.Fingerprint)
					m, e := stun.Build(attrs...)
					require.NoError(t, e)
					return m.Raw
				}
				// A private leg barrier acknowledges that the actual UDP reader processed
				// preceding input. No sleep decides whether receive permission is installed.
				var barrier uint64
				drain := func(index int) {
					barrier++
					_, e := sockets[index].WriteToUDPAddrPort(relayleg.Barrier(barrier, false), w.LocalAddr())
					require.NoError(t, e)
					got, ack, ok := relayleg.ParseBarrier(rawRead(index))
					require.True(t, ok && ack)
					require.Equal(t, barrier, got)
				}
				send(0, check(id, testCallerUfrag, answerPwd(t, answer), true))
				require.True(t, stun.IsMessage(read(0)))
				// Real SRTP processing at the UDP seam, with fixed synthetic test keys.
				profile := srtp.ProtectionProfileAeadAes128Gcm
				key, salt := make([]byte, 16), make([]byte, 12)
				in, out := testContext(t, key, salt, profile), testContext(t, key, salt, profile)
				sender, receiver := testContext(t, key, salt, profile), testContext(t, key, salt, profile)
				s := w.session(id)
				s.mu.Lock()
				s.srtpIn, s.srtpOut = in, out
				s.state.SRTP.Profile = profile
				s.mu.Unlock()
				echo := func(index int) {
					plain, e := receiver.DecryptRTP(nil, read(index), nil)
					require.NoError(t, e)
					var p rtp.Packet
					require.NoError(t, p.Unmarshal(plain))
					require.Equal(t, []byte{1, 2, 3}, p.Payload)
				}
				original := testEncrypt(t, sender, 123, 1)
				send(0, original)
				echo(0)
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
				packet := testEncrypt(t, sender, 123, 2)
				send(1, packet)
				drain(1)
				quiet(0) // no check: no receive permission
				send(1, check(ufrag, "newufrag", "incorrect-password", false))
				drain(1)
				send(1, packet)
				drain(1)
				quiet(0) // failed authentication grants nothing
				send(1, check(ufrag, "newufrag", pwd, false))
				require.True(t, stun.IsMessage(read(1)))
				send(1, original) // authenticated but already consumed on R1
				drain(1)
				quiet(1)
				forged := append([]byte(nil), packet...)
				forged[len(forged)-1] ^= 1
				send(1, forged)
				drain(1)
				quiet(1)
				send(2, packet) // trusted relay, not assigned to this call
				drain(2)
				quiet(2)
				other := netip.MustParseAddrPort("192.0.2.10:4567")
				_, err = sockets[1].WriteToUDPAddrPort(append(relay.AppendHeader(nil, other), packet...), w.LocalAddr())
				require.NoError(t, err)
				drain(1)
				quiet(1)
				send(0, testEncrypt(t, sender, 123, 3))
				echo(0) // invalid/replayed/nonassigned packets did not switch output
				quiet(1)
				if switchMedia {
					send(1, packet)
					echo(1) // first authenticated R2 media, without USE-CANDIDATE
					quiet(0)
					for range 2 {
						select {
						case <-commits.attempts:
						case <-time.After(2 * time.Second):
							t.Fatal("switch commit must retry")
						}
					}
					_, got, e := a.RelayOwner(ctx, id)
					require.NoError(t, e)
					require.Equal(t, pending, got, "failed commit keeps pending authorization")
					require.Error(t, w.AbortRelayRestart(ctx, id, 1), "switched path must never roll back after a failed commit")
					send(1, testEncrypt(t, sender, 123, 4))
					echo(1)
					quiet(0)
					commits.block.Store(false)
					require.Eventually(t, func() bool {
						_, got, e := a.RelayOwner(ctx, id)
						return e == nil && got == (sessionstore.RelayAssignment{Slot: "r2", Generation: 1})
					}, 2*time.Second, time.Millisecond)
					send(1, check(ufrag, "newufrag", pwd, true))
					require.True(t, stun.IsMessage(read(1)))
					send(1, testEncrypt(t, sender, 123, 5))
					echo(1)
					quiet(0)
					return
				}

				abort, ok := any(w).(interface {
					AbortRelayRestart(context.Context, string, uint64) error
				})
				require.True(t, ok, "worker must revoke pending receive before rollback")
				require.ErrorIs(t, abort.AbortRelayRestart(ctx, id, 2), sessionstore.ErrRelayConflict)
				require.NoError(t, abort.AbortRelayRestart(ctx, id, 1))
				require.NoError(t, a.CASRelay(ctx, lease, pending, sessionstore.RelayAssignment{Slot: "r1", Generation: 1}))
				send(1, check(ufrag, "newufrag", pwd, false))
				drain(1) // retired credentials get no response
				final := testEncrypt(t, sender, 123, 4)
				send(1, final)
				drain(1)
				quiet(0)
				quiet(1)
				send(0, final)
				echo(0) // same call continues on its current path after rollback
				failures, err := w.SessionDecryptFailures(id)
				require.NoError(t, err)
				require.EqualValues(t, 1, failures) // the deliberately forged packet
				t.Log("PENDING_RECEIVE before_auth=drop authenticated_media=switch nonassigned=drop after_rollback=drop old_path=live replay=drop")
			})
		})
	}
}

// The store boundary refuses only the post-switch assignment write. Ownership
// renewal and reads continue, as during a transient write outage.
type relayCommitFault struct {
	sessionstore.Store
	sessionstore.RelayAssignments
	block    atomic.Bool
	attempts chan struct{}
}

func (s *relayCommitFault) CASRelay(ctx context.Context, l sessionstore.Lease, from, to sessionstore.RelayAssignment) error {
	if from.Pending != "" && to.Slot == "r2" && s.block.Load() {
		select {
		case s.attempts <- struct{}{}:
		default:
		}
		return sessionstore.ErrTransient
	}
	return s.RelayAssignments.CASRelay(ctx, l, from, to)
}
