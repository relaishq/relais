package mediaworker

import (
	"net"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/srtp/v3"
	"github.com/stretchr/testify/require"
)

func TestCheckpointRelayReplayMeasuresSourceTimeThroughEcho(t *testing.T) {
	for _, kind := range []string{"audio", "video"} {
		t.Run(kind, func(t *testing.T) {
			w := newTestWorker(t)
			w.cfg.Relay = &RelayConfig{} // enable the production rate observation
			sink := listenTestUDP(t)
			pps, clock, pt := 50, uint32(48000), uint8(111)
			track := trackState{MID: "0", SSRC: 123, PayloadType: pt, Anchored: true, InboundSSRC: 456, SeqOffset: 8192, Packets: 1, HighestSentIndex: 9191}
			state := sessionState{ID: "replayed-rate", ICE: iceState{RemoteAddr: sink.LocalAddr().(*net.UDPAddr).AddrPort()}, SRTP: srtpState{Inbound: map[uint32]uint64{456: 999}}}
			if kind == "video" {
				pps, clock, pt = 500, 90000, 96
				track.MID, track.PayloadType = "1", pt
				state.Video = track
			} else {
				state.Audio = track
			}
			// This is the same reconstruction used by resume. The old inbound
			// floor and outbound margin survive; source timestamps do not move.
			sess := sessionFromState(w, state)
			defer sess.close()
			profile := srtp.ProtectionProfileAeadAes128Gcm
			keys := testSessionKeys(t, profile)
			sess.srtpIn = testContext(t, keys.RemoteMasterKey, keys.RemoteMasterSalt, profile)
			sess.srtpOut = testContext(t, keys.LocalMasterKey, keys.LocalMasterSalt, profile)
			caller := testContext(t, keys.RemoteMasterKey, keys.RemoteMasterSalt, profile)
			count := pps*12/5 + 1 // 2.4 source seconds, delivered without waits
			started := time.Now()
			for i := range count {
				raw, err := (&rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: pt, SSRC: 456, SequenceNumber: uint16(1000 + i), Timestamp: uint32(i) * (clock / uint32(pps))}, Payload: []byte{0x10, 0}}).Marshal()
				require.NoError(t, err)
				encrypted, err := caller.EncryptRTP(nil, raw, nil)
				require.NoError(t, err)
				sess.handleRTP(encrypted)
				if i == pps*19/10 { // 1.9 media seconds is insufficient
					var info CheckpointState
					sess.checkpointRates(&info)
					require.Zero(t, info.RTPPacketRate)
				}
			}
			elapsed := time.Since(started)
			require.Less(t, elapsed, 2*time.Second, "prove compressed arrival, not normal source pacing")
			require.Zero(t, sess.decryptFailures.Load())
			packets := sess.state.Audio.Packets
			if kind == "video" {
				packets = sess.state.Video.Packets
			}
			require.EqualValues(t, count+1, packets, "every replay packet reaches the live echo path")
			var info CheckpointState
			sess.checkpointRates(&info)
			require.InDelta(t, pps, info.RTPPacketRate, float64(pps)*0.02, "rate follows caller media time, not replay arrival time")
			t.Logf("REPLAY_SOURCE_RATE kind=%s packets=%d media_time=2.4s arrival_time=%s measured_pps=%.2f", kind, count, elapsed, info.RTPPacketRate)
		})
	}
}
