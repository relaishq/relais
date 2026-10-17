package mediaworker

import (
	"net"
	"testing"

	"github.com/pion/srtp/v3"
	"github.com/relaishq/relais/internal/workerprobe"
	"github.com/stretchr/testify/require"
)

func TestDecryptProbeRecordsReorderedPacketIndex(t *testing.T) {
	for _, tc := range []struct {
		name  string
		floor uint64
		seqs  []uint16
		order []int
		want  []uint64
	}{
		{"reordered", 9, []uint16{10, 11, 12}, []int{2, 0, 1}, []uint64{12, 10, 11}},
		{"reordered across wrap", 65533, []uint16{65534, 65535, 0, 1}, []int{0, 2, 1, 3}, []uint64{65534, 65536, 65535, 65537}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Cleanup(workerprobe.Enable())
			w := newTestWorker(t)
			workerprobe.Register(w.LocalAddr(), nil)
			t.Cleanup(func() { workerprobe.Remove(w.LocalAddr()) })
			sink := listenTestUDP(t)
			state := sessionState{ID: "decrypt-probe", ICE: iceState{RemoteAddr: sink.LocalAddr().(*net.UDPAddr).AddrPort()},
				SRTP:  srtpState{Inbound: map[uint32]uint64{456: tc.floor}},
				Audio: trackState{MID: "0", SSRC: 123, PayloadType: 111, Anchored: true, InboundSSRC: 456, SeqOffset: 8192, Packets: 1, HighestSentIndex: tc.floor + 8192},
			}
			sess := sessionFromState(w, state)
			defer sess.close()
			profile := srtp.ProtectionProfileAeadAes128Gcm
			keys := testSessionKeys(t, profile)
			sess.srtpIn = testContext(t, keys.RemoteMasterKey, keys.RemoteMasterSalt, profile)
			sess.srtpOut = testContext(t, keys.LocalMasterKey, keys.LocalMasterSalt, profile)
			caller := testContext(t, keys.RemoteMasterKey, keys.RemoteMasterSalt, profile)
			var observed []uint64
			require.NoError(t, workerprobe.SetAfterDecrypt(w.LocalAddr(), func(id string, ssrc uint32, index uint64) {
				require.Equal(t, state.ID, id)
				require.EqualValues(t, 456, ssrc)
				observed = append(observed, index)
			}))
			var encrypted [][]byte
			for _, seq := range tc.seqs {
				encrypted = append(encrypted, testEncrypt(t, caller, 456, seq))
			}
			for _, i := range tc.order {
				sess.handleRTP(encrypted[i])
			}
			require.Zero(t, sess.decryptFailures.Load()+sess.replayRejections.Load())
			require.Equal(t, tc.want, observed, "late authenticated input keeps its own index")
		})
	}
}
