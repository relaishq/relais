package mediaworker

import (
	"encoding/json"
	"net/netip"
	"testing"

	"github.com/pion/srtp/v3"
	"github.com/stretchr/testify/require"
)

func TestSnapshotInboundIndexesSeparateOutboundSpace(t *testing.T) {
	snap := snapshot{Version: sessionStateVersion, DTLSConnection: []byte{1}, State: sessionState{
		Version: sessionStateVersion, ID: "id", ICE: iceState{LocalUfrag: "id", RemoteAddr: netip.MustParseAddrPort("127.0.0.1:12345")},
		SRTP:  srtpState{Profile: srtp.ProtectionProfileAes128CmHmacSha1_80, Inbound: map[uint32]uint64{42: 7<<16 | 123}},
		Video: trackState{SSRC: 999, HighestSentIndex: 8<<16 | 456, ReplayFloor: 8<<16 | 455},
	}}
	raw, err := json.Marshal(snap)
	require.NoError(t, err)
	indexes, err := SnapshotInboundIndexes(raw)
	require.NoError(t, err)
	require.Equal(t, map[uint32]uint64{42: 7<<16 | 123}, indexes)
	indexes[42] = 1
	again, err := SnapshotInboundIndexes(raw)
	require.NoError(t, err)
	require.EqualValues(t, 7<<16|123, again[42])
	_, err = SnapshotInboundIndexes([]byte(`{"Version":0}`))
	require.Error(t, err)
}
