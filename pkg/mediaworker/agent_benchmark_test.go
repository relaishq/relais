package mediaworker

import (
	"net"
	"testing"

	"github.com/pion/rtp"
	"github.com/pion/srtp/v3"
	"github.com/stretchr/testify/require"
)

// This file also compiles unchanged on main. Both runs create a normal default
// session, authenticate new caller indexes and exercise the production UDP path.
// Caller encryption happens outside the timed section, in bounded batches.
func BenchmarkEchoAudioPacket(b *testing.B) {
	w, err := New(Config{})
	require.NoError(b, err)
	defer func() { require.NoError(b, w.Close()) }()
	offer, err := parseOffer(testOffer(testOfferAttrs{}))
	require.NoError(b, err)
	s, err := newSession(w, offer)
	require.NoError(b, err)
	defer s.close()
	sink, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(b, err)
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, receiveMTU)
		for {
			if _, _, err := sink.ReadFromUDP(buf); err != nil {
				return
			}
		}
	}()
	defer func() { require.NoError(b, sink.Close()); <-done }()
	s.state.ICE.RemoteAddr = sink.LocalAddr().(*net.UDPAddr).AddrPort()
	profile := srtp.ProtectionProfileAeadAes128Gcm
	key, salt := make([]byte, 16), make([]byte, 12)
	s.srtpIn, err = srtp.CreateContext(key, salt, profile, srtp.SRTPReplayProtection(replayWindow))
	require.NoError(b, err)
	s.srtpOut, err = srtp.CreateContext(key, salt, profile)
	require.NoError(b, err)
	caller, err := srtp.CreateContext(key, salt, profile)
	require.NoError(b, err)
	b.ReportAllocs()
	b.ResetTimer()
	b.StopTimer()
	for first := 0; first < b.N; {
		n := min(4096, b.N-first)
		packets := make([][]byte, n)
		for i := range packets {
			raw, err := (&rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 111, SSRC: 456, SequenceNumber: uint16(first + i), Timestamp: uint32(first+i) * 960}, Payload: []byte{0xf8, 0xff, 0xfe}}).Marshal()
			require.NoError(b, err)
			packets[i], err = caller.EncryptRTP(nil, raw, nil)
			require.NoError(b, err)
		}
		b.StartTimer()
		for _, packet := range packets {
			s.handleRTP(packet)
		}
		b.StopTimer()
		first += n
	}
	require.EqualValues(b, b.N, s.state.Audio.Packets)
	require.Zero(b, s.decryptFailures.Load())
}
