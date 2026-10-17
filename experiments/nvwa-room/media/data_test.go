package media

import (
	"github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestNetworkStatePolicyKeepsCommandsReliable(t *testing.T) {
	for _, reliable := range []bool{true, false} {
		t.Run(map[bool]string{true: "reliable", false: "unreliable"}[reliable], func(t *testing.T) {
			pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
			require.NoError(t, err)
			defer pc.Close()
			dc, err := NewDCPolicy(pc, reliable)
			require.NoError(t, err)
			require.True(t, dc.control.Ordered())
			require.Nil(t, dc.control.MaxRetransmits())
			require.Nil(t, dc.control.MaxPacketLifeTime())
			require.Equal(t, reliable, dc.state.Ordered())
			if reliable {
				require.Nil(t, dc.state.MaxRetransmits())
			} else {
				require.NotNil(t, dc.state.MaxRetransmits())
				require.Zero(t, *dc.state.MaxRetransmits())
			}
		})
	}
}
