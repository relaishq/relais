package relay

import (
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDataChannelDTLSRecordsNeverEnterReplayCache(t *testing.T) {
	b := newPacketBuffer(BufferConfig{})
	caller := netip.MustParseAddrPort("127.0.0.1:23456")
	// RFC 7983 DTLS application data, including bytes that look like RTP
	// indexes inside ciphertext, must never be interpreted as SRTP.
	for _, kind := range []byte{20, 21, 22, 23, 25, 63} {
		record := []byte{kind, 0xfe, 0xfd, 0, 1, 0, 0, 0, 0, 0, 1, 0, 4, 0x80, 111, 0, 7}
		_, _, rtp := callerIndex(record)
		require.False(t, rtp)
		b.add("data", caller, record, time.Now())
	}
	require.Empty(t, b.sessions)
	require.Empty(t, b.packets.Len())
	require.Zero(t, b.bytes)
}
