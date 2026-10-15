package mediaworker

import (
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/srtp/v3"
	"github.com/relais/pkg/framecache"
	"github.com/relais/pkg/relay"
	"github.com/stretchr/testify/require"
)

// bits marks indexes below high: bit i is high-i (ADR 0009).
func bits(high uint64, indexes ...uint64) uint64 {
	var w uint64
	for _, index := range indexes {
		w |= 1 << (high - index)
	}
	return w
}

func TestReceiveWindowTracksAuthenticatedInput(t *testing.T) {
	for _, tc := range []struct {
		name     string
		start    srtpState
		seqs     []uint16
		high     uint64
		received uint64
	}{
		{name: "in order", seqs: []uint16{10, 11, 12}, high: 12, received: 0b111},
		{name: "first packet at index 0", seqs: []uint16{0}, high: 0, received: 1},
		{name: "overtaken input fills its bit late", seqs: []uint16{10, 12}, high: 12, received: bits(12, 10, 12)},
		{name: "late arrival inside the window", seqs: []uint16{10, 12, 11}, high: 12, received: 0b111},
		{name: "H-63 counts, H-64 is outside the window", seqs: []uint16{10, 80, 17, 16}, high: 80, received: bits(80, 80, 17)},
		{name: "a jump of 64 or more clears the window", seqs: []uint16{10, 11, 200}, high: 200, received: 1},
		{name: "sequence wrap", seqs: []uint16{65534, 65535, 0, 1}, high: 65537, received: 0b1111},
		{
			// A worker resuming an export without a window cannot know which
			// earlier indexes the previous owner consumed: assume all of them.
			name:  "inherited inexact export",
			start: srtpState{Inbound: map[uint32]uint64{7: 100}},
			seqs:  []uint16{102}, high: 102, received: 0xfffffffffffffffd,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := tc.start
			if state.Inbound == nil {
				state.Inbound = make(map[uint32]uint64)
			}
			for _, seq := range tc.seqs {
				state.noteInbound(7, seq)
			}
			require.Equal(t, tc.high, state.Inbound[7])
			require.Equal(t, fmt.Sprintf("%064b", tc.received), fmt.Sprintf("%064b", state.Received[7]))
		})
	}
}

// encryptAt encrypts a caller RTP packet at an exact extended index.
func encryptAt(t *testing.T, keys srtp.SessionKeys, profile srtp.ProtectionProfile, ssrc uint32, index uint64) []byte {
	t.Helper()
	ctx := testContext(t, keys.RemoteMasterKey, keys.RemoteMasterSalt, profile)
	ctx.SetROC(ssrc, uint32(index>>16))
	raw, err := (&rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 111, SequenceNumber: uint16(index), SSRC: ssrc}, Payload: []byte{1, 2, 3}}).Marshal()
	require.NoError(t, err)
	encrypted, err := ctx.EncryptRTP(nil, raw, nil)
	require.NoError(t, err)
	return encrypted
}

// A restored window rejects every consumed index, as the exporting worker's
// own context would, and admits each unconsumed one exactly once.
func TestRestoreInboundWindowMatchesExporter(t *testing.T) {
	for _, profile := range []srtp.ProtectionProfile{srtp.ProtectionProfileAeadAes128Gcm, srtp.ProtectionProfileAes128CmHmacSha1_80} {
		for _, tc := range []struct {
			name     string
			high     uint64
			consumed []uint64
			accept   []uint64
			reject   []uint64
		}{
			{name: "window", high: 1000, consumed: []uint64{1000, 998, 990, 937}, accept: []uint64{999, 991, 938, 1001}, reject: []uint64{998, 990, 937, 936, 1000}},
			{name: "across a wrap", high: 65541, consumed: []uint64{65541, 65535, 65534}, accept: []uint64{65536, 65533, 65542}, reject: []uint64{65535, 65534, 65541}},
			{name: "below 63", high: 3, consumed: []uint64{3, 1}, accept: []uint64{2, 0}, reject: []uint64{1, 3}},
		} {
			t.Run(profileName(profile)+"/"+tc.name, func(t *testing.T) {
				keys := testSessionKeys(t, profile)
				in := testContext(t, keys.RemoteMasterKey, keys.RemoteMasterSalt, profile)
				require.NoError(t, restoreInboundWindow(in, keys, profile, 456, tc.high, bits(tc.high, tc.consumed...)))
				for _, index := range tc.accept {
					_, err := in.DecryptRTP(nil, encryptAt(t, keys, profile, 456, index), nil)
					require.NoError(t, err, "unconsumed index %d", index)
				}
				for _, index := range tc.reject {
					_, err := in.DecryptRTP(nil, encryptAt(t, keys, profile, 456, index), nil)
					require.ErrorContains(t, err, "duplicated packet", "consumed or too old index %d", index)
				}
			})
		}
	}
}

// Pion's replay error type is unexported; this pins the message the worker
// classifies, so a dependency change cannot turn replays into failures.
func TestReplayRejectionIsNotDecryptFailure(t *testing.T) {
	w, s, caller := agentPacketSession(t, &countingAgent{}, time.Second)
	packet := testEncrypt(t, caller, 456, 7)
	s.handleRTP(packet)
	s.handleRTP(packet)
	require.Zero(t, s.decryptFailures.Load())
	require.EqualValues(t, 1, s.replayRejections.Load())
	tampered := testEncrypt(t, caller, 456, 8)
	tampered[len(tampered)-1] ^= 0xff
	s.handleRTP(tampered)
	require.EqualValues(t, 1, s.decryptFailures.Load())
	require.EqualValues(t, 1, s.replayRejections.Load())
	rejections, err := w.SessionReplayRejections(s.id)
	require.NoError(t, err)
	require.EqualValues(t, 1, rejections)
	body := metricsBody(w)
	require.Contains(t, body, "relais_worker_replay_rejections_total 1\n")
	require.Contains(t, body, "relais_worker_decryption_failures_total 1\n")
}

// While a custom audio agent flushes for export, only its audio stays
// unauthenticated. Video and SRTCP keep their unchanged paths (ADR 0006).
func TestPausedAgentLeavesVideoAndRTCPActive(t *testing.T) {
	_, s, caller := agentPacketSession(t, &countingAgent{}, time.Second)
	s.handleRTP(testEncrypt(t, caller, 456, 1))
	require.Eventually(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.state.Agent.Progress.Consumed == 1 }, time.Second, time.Millisecond)
	s.mu.Lock()
	s.agent.paused = true
	s.mu.Unlock()
	s.handleRTP(testEncrypt(t, caller, 456, 2))
	raw, err := (&rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 96, SequenceNumber: 50, SSRC: 789, Marker: true}, Payload: []byte{0x10, 1}}).Marshal()
	require.NoError(t, err)
	video, err := caller.EncryptRTP(nil, raw, nil)
	require.NoError(t, err)
	s.handleRTP(video)
	pli, err := (&rtcp.PictureLossIndication{SenderSSRC: 789, MediaSSRC: 124}).Marshal()
	require.NoError(t, err)
	control, err := caller.EncryptRTCP(nil, pli, nil)
	require.NoError(t, err)
	s.handleRTCP(control)
	s.mu.Lock()
	audioHigh, videoHigh, videoSent, pliSent := s.state.SRTP.Inbound[456], s.state.SRTP.Inbound[789], s.state.Video.Packets, s.state.Video.SRTCPIndex
	s.mu.Unlock()
	require.EqualValues(t, 1, audioHigh, "paused agent audio is not authenticated")
	require.EqualValues(t, 50, videoHigh, "video is authenticated while the agent flushes")
	require.EqualValues(t, 1, videoSent, "video is still echoed")
	require.Positive(t, pliSent, "the caller's keyframe request was processed and relayed while paused")
	require.Zero(t, s.decryptFailures.Load())
}

func TestSnapshotConsumedInputReportsExactness(t *testing.T) {
	state := sessionState{Version: sessionStateVersion, ID: "exact", ICE: iceState{LocalUfrag: "exact", RemoteAddr: netip.MustParseAddrPort("127.0.0.1:5004")}, SRTP: srtpState{Profile: srtp.ProtectionProfileAeadAes128Gcm,
		Inbound: map[uint32]uint64{1: 100, 2: 7}, Received: map[uint32]uint64{1: 0b101}}}
	data, err := json.Marshal(snapshot{Version: sessionStateVersion, State: state, DTLSConnection: []byte{1}})
	require.NoError(t, err)
	input, err := SnapshotConsumedInput(data)
	require.NoError(t, err)
	require.Equal(t, map[uint32]relay.ConsumedInput{1: {High: 100, Window: 0b101, Exact: true}, 2: {High: 7}}, input)
	var raw map[string]any
	require.NoError(t, json.Unmarshal(data, &raw))
	srtpJSON := raw["State"].(map[string]any)["SRTP"].(map[string]any)
	require.Contains(t, srtpJSON, "Received")
	state.SRTP.Received = nil
	data, err = json.Marshal(snapshot{Version: sessionStateVersion, State: state, DTLSConnection: []byte{1}})
	require.NoError(t, err)
	require.NotContains(t, string(data), "Received", "an older worker's export has no window")
	input, err = SnapshotConsumedInput(data)
	require.NoError(t, err)
	require.False(t, input[1].Exact)
}

// After a cached-picture takeover, an exact margin-zero planned resume keeps
// the positive ReplayFloor (ADR 0009). Unconsumed late video is echoed under
// fresh outbound indexes, consumed or too-old input is rejected by the
// restored window, and the caller's receive context never sees a reused
// index. Without a restored video window the floor still advances.
func TestExactPlannedResumeKeepsReplayFloor(t *testing.T) {
	for _, tc := range []struct {
		name       string
		restore    string // "video", "none" or "audio-only"
		margin     uint16
		keepsFloor bool
	}{
		{name: "restored video window", restore: "video", keepsFloor: true},
		{name: "mask present, window not restored", restore: "none"},
		{name: "restored window with a crash margin", restore: "video", margin: 8192},
		{name: "only another SSRC restored", restore: "audio-only"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newTestWorker(t)
			w.cfg.DisableResumePLI = true
			receiver := listenTestUDP(t)
			profile := srtp.ProtectionProfileAeadAes128Gcm
			keys := testSessionKeys(t, profile)
			state := sessionState{ID: "late", ICE: iceState{RemoteAddr: receiver.LocalAddr().(*net.UDPAddr).AddrPort()}, SRTP: srtpState{Profile: profile, Inbound: map[uint32]uint64{456: 1000}}, Video: trackState{MID: "1", ID: "video", SSRC: 123, PayloadType: 96, Anchored: true, InboundSSRC: 456, Packets: 10, HighestSentIndex: 6000, SeqOffset: 5000, LastTimestamp: 9000}}
			sess := sessionFromState(w, state)
			defer sess.close()
			sess.srtpIn = testContext(t, keys.RemoteMasterKey, keys.RemoteMasterSalt, profile)
			sess.srtpOut = testContext(t, keys.LocalMasterKey, keys.LocalMasterSalt, profile)
			require.NoError(t, restoreInboundIndex(sess.srtpIn, keys, profile, 456, 1000))
			require.NoError(t, sess.resumeTrack(&sess.state.Video, ResumeOptions{SequenceMargin: 8192}))
			var frames []framecache.Frame
			for i := range 10 {
				frames = append(frames, framecache.Frame{Timestamp: uint32(3000 + i*3000), EchoTimestamp: 9000, SourceSSRC: 456, Arrival: time.Now(), Keyframe: i == 0, Packets: []framecache.Packet{{SequenceNumber: uint16(991 + i), Marker: true, Payload: []byte{0x10, 0}}}})
			}
			require.True(t, sess.reserveReplay(frames, 8192))
			burst, err := sess.encodeReplay()
			require.NoError(t, err)
			sess.sendReplay(burst)
			callerRx := testContext(t, keys.LocalMasterKey, keys.LocalMasterSalt, profile)
			read := func() uint16 {
				require.NoError(t, receiver.SetReadDeadline(time.Now().Add(time.Second)))
				buf := make([]byte, 2048)
				n, _, err := receiver.ReadFromUDP(buf)
				require.NoError(t, err)
				var h rtp.Header
				_, err = callerRx.DecryptRTP(nil, buf[:n], &h)
				require.NoError(t, err, "caller must never see duplicate SRTP indexes")
				return h.SequenceNumber
			}
			for range 10 {
				read()
			}
			floor := sess.state.Video.ReplayFloor
			require.Positive(t, floor)
			send := func(seq uint16) {
				raw, err := (&rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 96, SSRC: 456, SequenceNumber: seq, Timestamp: uint32(seq) * 3000, Marker: true}, Payload: []byte{0x10, 1}}).Marshal()
				require.NoError(t, err)
				sess.handleRTP(encryptVideo(t, keys, profile, raw))
			}
			// The resumed owner consumes 1020 and two late packets, then exports.
			for _, seq := range []uint16{1020, 1005, 1003} {
				send(seq)
				read()
			}
			persisted := sess.state
			require.Equal(t, bits(1020, 1020, 1005, 1003), persisted.SRTP.Received[456]&bits(1020, 1020, 1019, 1018, 1017, 1016, 1015, 1014, 1013, 1012, 1011, 1010, 1009, 1008, 1007, 1006, 1005, 1004, 1003, 1002, 1001),
				"only the indexes this owner authenticated above the inherited checkpoint are marked")

			// The planned target restores the exported window, then resumes the track.
			sess.srtpIn = testContext(t, keys.RemoteMasterKey, keys.RemoteMasterSalt, profile)
			sess.srtpOut = testContext(t, keys.LocalMasterKey, keys.LocalMasterSalt, profile)
			sess.state = persisted
			sess.afterReplay = false
			sess.restoredWindows = nil
			switch tc.restore {
			case "video":
				require.NoError(t, sess.restoreReplayInbound(keys, ResumeOptions{}))
				require.True(t, sess.restoredWindows[456])
			case "none":
				require.NoError(t, restoreInboundIndex(sess.srtpIn, keys, profile, 456, 1020))
			case "audio-only":
				require.NoError(t, restoreInboundIndex(sess.srtpIn, keys, profile, 456, 1020))
				sess.restoredWindows = map[uint32]bool{999: true}
			}
			require.NoError(t, sess.resumeTrack(&sess.state.Video, ResumeOptions{SequenceMargin: tc.margin}))
			if !tc.keepsFloor {
				require.Greater(t, sess.state.Video.ReplayFloor, floor, "every other resume advances the floor")
				return
			}
			require.Equal(t, floor, sess.state.Video.ReplayFloor)
			before := sess.state.Video.Packets
			for seq := uint16(1019); seq >= 1001; seq-- {
				send(seq)
				if seq == 1005 || seq == 1003 {
					continue // consumed by the exporter: rejected by the restored window
				}
				got := read()
				require.Greater(t, extendIndex(sess.state.Video.HighestSentIndex, got), floor)
			}
			require.Equal(t, before+17, sess.state.Video.Packets, "17 unconsumed late packets echoed once each")
			require.EqualValues(t, 2, sess.replayRejections.Load())
			for seq := uint16(1000); seq >= 940; seq-- {
				send(seq) // consumed before the checkpoint, or older than the window
			}
			require.Equal(t, before+17, sess.state.Video.Packets)
			require.EqualValues(t, 2+61, sess.replayRejections.Load())
			require.Zero(t, sess.decryptFailures.Load())
			require.NoError(t, receiver.SetReadDeadline(time.Now().Add(100*time.Millisecond)))
			_, _, err = receiver.ReadFromUDP(make([]byte, 2048))
			require.Error(t, err, "nothing else was echoed")
		})
	}
}

func encryptVideo(t *testing.T, keys srtp.SessionKeys, profile srtp.ProtectionProfile, raw []byte) []byte {
	t.Helper()
	ctx := testContext(t, keys.RemoteMasterKey, keys.RemoteMasterSalt, profile)
	encrypted, err := ctx.EncryptRTP(nil, raw, nil)
	require.NoError(t, err)
	return encrypted
}
