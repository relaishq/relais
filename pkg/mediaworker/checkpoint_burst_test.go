package mediaworker

import (
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/srtp/v3"
	"github.com/stretchr/testify/require"
)

// Both an unpaced keyframe and a full relay hold queue reach this exact live
// encryption path. Count actual decryptable UDP echoes, not just input packets.
func TestCheckpointRTPBurstEchoedInFull(t *testing.T) {
	for _, count := range []int{150, 256} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			w := newTestWorker(t)
			w.cfg.Relay = &RelayConfig{} // enable checkpoint measurement
			sink := listenTestUDP(t)
			sess := sessionFromState(w, sessionState{ID: "burst", ICE: iceState{RemoteAddr: sink.LocalAddr().(*net.UDPAddr).AddrPort()}, SRTP: srtpState{Inbound: make(map[uint32]uint64)}, Video: trackState{MID: "1", SSRC: 123, PayloadType: 96, InitialSeq: 1200}})
			defer sess.close()
			profile := srtp.ProtectionProfileAeadAes128Gcm
			keys := testSessionKeys(t, profile)
			sess.srtpIn = testContext(t, keys.RemoteMasterKey, keys.RemoteMasterSalt, profile)
			sess.srtpOut = testContext(t, keys.LocalMasterKey, keys.LocalMasterSalt, profile)
			caller := testContext(t, keys.RemoteMasterKey, keys.RemoteMasterSalt, profile)
			receiver := testContext(t, keys.LocalMasterKey, keys.LocalMasterSalt, profile)
			for i := range count {
				raw, err := (&rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 96, SSRC: 456, SequenceNumber: uint16(i + 1), Timestamp: 3000, Marker: i == count-1}, Payload: []byte{0x10, 0}}).Marshal()
				require.NoError(t, err)
				encrypted, err := caller.EncryptRTP(nil, raw, nil)
				require.NoError(t, err)
				sess.handleRTP(encrypted)
			}
			require.EqualValues(t, count, sess.state.Video.Packets)
			require.NoError(t, sink.SetReadDeadline(time.Now().Add(time.Second)))
			buf := make([]byte, receiveMTU)
			for range count {
				n, _, err := sink.ReadFromUDP(buf)
				require.NoError(t, err)
				_, err = receiver.DecryptRTP(nil, buf[:n], nil)
				require.NoError(t, err)
			}
		})
	}
}
