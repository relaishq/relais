package mediaworker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/srtp/v3"
	"github.com/relais/pkg/relay"
	"github.com/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

func TestSnapshotIsResumableAndDoesNotFence(t *testing.T) {
	w := newTestWorker(t)
	call, client := dialDTLSCaller(t, w)
	state, err := w.SnapshotSession(call.id)
	require.NoError(t, err)
	snap, err := decodeSnapshot(state)
	require.NoError(t, err)
	require.Equal(t, call.id, snap.State.ID)
	require.False(t, w.session(call.id).fenced.Load())
	require.True(t, call.endpoint(t).check(t, false))
	require.NoError(t, client.SetReadDeadline(time.Now().Add(silence)))
	_, err = client.Read(make([]byte, receiveMTU))
	require.True(t, isTimeout(err))
	_, err = w.ExportSession(call.id)
	require.NoError(t, err)
	_, err = w.ResumeSession(state, ResumeOptions{})
	require.NoError(t, err, "non-destructive snapshot uses the exact resume format")
}

func TestLeaseLostSnapshotSilentlyDiscardsSession(t *testing.T) {
	store := sessionstore.NewMemory()
	r, err := relay.New(relay.Config{PublicAddr: "127.0.0.1:0", WorkerAddr: "127.0.0.1:0", Owners: store})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, r.Close()) })
	w, err := New(Config{SnapshotInterval: time.Hour, Relay: &RelayConfig{Owners: store, Addr: r.WorkerAddr(), PublicAddr: r.PublicAddr()}})
	require.NoError(t, err)
	r.AddWorker(w.LocalAddr())
	t.Cleanup(func() { require.NoError(t, w.Close()) })
	call, client := dialDTLSCaller(t, w)
	sess := w.session(call.id)
	require.Eventually(t, func() bool { _, err := store.GetState(context.Background(), call.id); return err == nil }, time.Second, time.Millisecond, "initial snapshot finished before transfer")
	lease, err := store.Get(context.Background(), call.id)
	require.NoError(t, err)
	_, err = store.Transfer(context.Background(), lease, netip.MustParseAddrPort("127.0.0.1:12345"), time.Minute)
	require.NoError(t, err)
	require.ErrorIs(t, sess.persistSnapshot(), sessionstore.ErrLeaseLost)
	require.Nil(t, w.session(call.id))
	require.NoError(t, client.SetReadDeadline(time.Now().Add(silence)))
	_, err = client.Read(make([]byte, receiveMTU))
	require.True(t, isTimeout(err), "lost put sends no close_notify")
}

func TestHandshakeSnapshotMarginSkipsFirstPacketIndexes(t *testing.T) {
	for _, initial := range []uint16{1200, 50000} {
		t.Run(fmt.Sprint(initial), func(t *testing.T) {
			profile := srtp.ProtectionProfileAeadAes128Gcm
			keys := testSessionKeys(t, profile)
			out := testContext(t, keys.LocalMasterKey, keys.LocalMasterSalt, profile)
			receiver := testContext(t, keys.LocalMasterKey, keys.LocalMasterSalt, profile)
			old := testContext(t, keys.LocalMasterKey, keys.LocalMasterSalt, profile)
			ssrc := uint32(123)
			_, err := receiver.DecryptRTP(nil, testEncrypt(t, old, ssrc, initial), nil)
			require.NoError(t, err)
			sess := &session{srtpOut: out}
			track := trackState{MID: "video", SSRC: ssrc, InitialSeq: initial}
			require.NoError(t, sess.resumeTrack(&track, ResumeOptions{SequenceMargin: 8192, SRTCPIndexMargin: 128}))
			_, err = receiver.DecryptRTP(nil, testEncrypt(t, out, ssrc, track.InitialSeq), nil)
			require.NoError(t, err)
			require.EqualValues(t, 128, track.SRTCPIndex)
		})
	}
}

type rejoinReceiverFunc func(netip.AddrPort) error

func (f rejoinReceiverFunc) Heartbeat(addr netip.AddrPort) error { return f(addr) }

func TestHeartbeatRejoinFencesBeforeAcknowledgment(t *testing.T) {
	w := newTestWorker(t)
	call, client := dialDTLSCaller(t, w)
	old := w.session(call.id)
	heartbeats := 0
	receiver := rejoinReceiverFunc(func(addr netip.AddrPort) error {
		require.Equal(t, w.LocalAddr(), addr)
		heartbeats++
		if heartbeats == 1 {
			return ErrRejoinRequired
		}
		require.Nil(t, w.session(call.id), "acknowledgment must see no stale session")
		require.True(t, old.fenced.Load())
		return nil
	})
	w.heartbeat(receiver)
	require.Equal(t, 2, heartbeats)
	require.NoError(t, client.SetReadDeadline(time.Now().Add(silence)))
	_, err := client.Read(make([]byte, receiveMTU))
	require.True(t, isTimeout(err), "rejoining silently discards old transport")
}

func TestInitialSequenceLeavesRoomForFreshReceiverTakeover(t *testing.T) {
	for range 32 {
		offer, err := parseOffer(testOffer(testOfferAttrs{}))
		require.NoError(t, err)
		track, err := newTrackState(offer.accepted(mediaVideo))
		require.NoError(t, err)
		require.Less(t, track.InitialSeq, uint16(1<<15))
		profile := srtp.ProtectionProfileAeadAes128Gcm
		keys := testSessionKeys(t, profile)
		out := testContext(t, keys.LocalMasterKey, keys.LocalMasterSalt, profile)
		old := testContext(t, keys.LocalMasterKey, keys.LocalMasterSalt, profile)
		observed := testContext(t, keys.LocalMasterKey, keys.LocalMasterSalt, profile)
		fresh := testContext(t, keys.LocalMasterKey, keys.LocalMasterSalt, profile)
		initial := track.InitialSeq
		for seq := initial; seq < initial+5; seq++ {
			_, err := observed.DecryptRTP(nil, testEncrypt(t, old, track.SSRC, seq), nil)
			require.NoError(t, err)
		}
		sess := &session{srtpOut: out}
		require.NoError(t, sess.resumeTrack(&track, ResumeOptions{SequenceMargin: 8192}))
		encrypted := testEncrypt(t, out, track.SSRC, track.InitialSeq)
		_, err = observed.DecryptRTP(nil, append([]byte{}, encrypted...), nil)
		require.NoError(t, err, "receiver that saw the source")
		_, err = fresh.DecryptRTP(nil, encrypted, nil)
		require.NoError(t, err, "receiver that never saw the source")
	}
}

// A negotiated but idle camera must never roll into ROC 1 before the receiver
// has seen a packet. Even repeated takeover margins fail before the wrap.
func TestUnsentTrackTakeoverRejectsSequenceWrap(t *testing.T) {
	profile := srtp.ProtectionProfileAeadAes128Gcm
	keys := testSessionKeys(t, profile)
	track := trackState{MID: "video", SSRC: 123, InitialSeq: 30000}
	for range 4 {
		out := testContext(t, keys.LocalMasterKey, keys.LocalMasterSalt, profile)
		sess := &session{srtpOut: out}
		require.NoError(t, sess.resumeTrack(&track, ResumeOptions{SequenceMargin: 8192}))
	}
	before := track
	sess := &session{srtpOut: testContext(t, keys.LocalMasterKey, keys.LocalMasterSalt, profile)}
	require.ErrorIs(t, sess.resumeTrack(&track, ResumeOptions{SequenceMargin: 8192}), ErrSequenceBudgetExhausted)
	require.Equal(t, before, track, "budget rejection fails before changing counters")
}

func TestFirstEchoRequestsImmediateSnapshotWithoutRollover(t *testing.T) {
	w := newTestWorker(t)
	sess := sessionFromState(w, sessionState{
		ID:    "first-echo",
		ICE:   iceState{RemoteAddr: w.LocalAddr()},
		SRTP:  srtpState{Inbound: make(map[uint32]uint64)},
		Audio: trackState{MID: "0", SSRC: 123, PayloadType: 111, InitialSeq: 1200},
	})
	defer sess.close()
	profile := srtp.ProtectionProfileAeadAes128Gcm
	keys := testSessionKeys(t, profile)
	sess.srtpIn = testContext(t, keys.RemoteMasterKey, keys.RemoteMasterSalt, profile)
	sess.srtpOut = testContext(t, keys.LocalMasterKey, keys.LocalMasterSalt, profile)
	caller := testContext(t, keys.RemoteMasterKey, keys.RemoteMasterSalt, profile)
	sess.handleRTP(testEncrypt(t, caller, 456, 1))
	require.EqualValues(t, 1, sess.state.Audio.Packets)
	require.Zero(t, sess.state.Audio.HighestSentIndex>>16)
	select {
	case <-sess.snapshotWanted:
	default:
		t.Fatal("first echo did not wake the snapshot writer")
	}
	sess.handleRTP(testEncrypt(t, caller, 456, 2))
	select {
	case <-sess.snapshotWanted:
		t.Fatal("ordinary second packet must not wake snapshot writer")
	default:
	}
}

func TestFencedSessionDoesNotRequestKeyframe(t *testing.T) {
	// An anchored session without an SRTP runtime would panic if the production
	// fence were missing. A fence must stop PLI before encryption or index use.
	sess := &session{state: sessionState{Video: trackState{Anchored: true}}}
	sess.fenced.Store(true)
	sess.requestKeyframe("fenced")
	require.Zero(t, sess.state.Video.SRTCPIndex)
}

// Keep each receiver anchored at its pre-silence index. Probe ciphertext is
// never recorded as sent on the silent track, so margins must accumulate even
// across encoded snapshots. Include the caller's whole reserved outage gap.
func TestSilentEstablishedTrackSequenceBudgetBeforeUndecryptableStep(t *testing.T) {
	for _, index := range []uint64{60000, 66536} {
		t.Run(fmt.Sprint(index), func(t *testing.T) {
			profile := srtp.ProtectionProfileAeadAes128Gcm
			keys := testSessionKeys(t, profile)
			const ssrc = 123
			originalReceiver := func() *srtp.Context {
				receiver := testContext(t, keys.LocalMasterKey, keys.LocalMasterSalt, profile)
				source := testContext(t, keys.LocalMasterKey, keys.LocalMasterSalt, profile)
				if index > 0xffff {
					_, err := receiver.DecryptRTP(nil, testEncrypt(t, source, ssrc, 65535), nil)
					require.NoError(t, err)
				}
				_, err := receiver.DecryptRTP(nil, testEncrypt(t, source, ssrc, uint16(index)), nil) //nolint:gosec // low 16 bits of test index
				require.NoError(t, err)
				return receiver
			}
			track := trackState{MID: "video", SSRC: ssrc, Packets: 1, HighestSentIndex: index, Anchored: true}
			var last uint16
			for step := range 2 {
				out := testContext(t, keys.LocalMasterKey, keys.LocalMasterSalt, profile)
				sess := &session{srtpOut: out}
				require.NoError(t, sess.resumeTrack(&track, ResumeOptions{SequenceMargin: 8192}))
				require.EqualValues(t, (step+1)*8192, track.AdvanceSinceSend)
				last = uint16(track.HighestSentIndex + SequenceGapReserve) //nolint:gosec // low 16 bits of test index
				_, err := originalReceiver().DecryptRTP(nil, testEncrypt(t, out, ssrc, last), nil)
				require.NoError(t, err, "pre-silence receiver decrypts every accepted step plus caller gap")
				encoded, err := json.Marshal(track)
				require.NoError(t, err)
				var restored trackState
				require.NoError(t, json.Unmarshal(encoded, &restored))
				track = restored
			}
			before := track
			sess := &session{srtpOut: testContext(t, keys.LocalMasterKey, keys.LocalMasterSalt, profile)}
			require.ErrorIs(t, sess.resumeTrack(&track, ResumeOptions{SequenceMargin: 8192}), ErrSequenceBudgetExhausted)
			require.Equal(t, before, track, "third silent advance is rejected before counter changes")
			track.noteSent(&rtp.Header{SequenceNumber: last})
			require.Zero(t, track.AdvanceSinceSend, "sending establishes a new caller anchor")
			require.NoError(t, sess.resumeTrack(&track, ResumeOptions{SequenceMargin: 8192}))
		})
	}
}
