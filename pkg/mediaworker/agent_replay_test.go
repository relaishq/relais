package mediaworker

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/srtp/v3"
	"github.com/relais/pkg/agent"
	"github.com/stretchr/testify/require"
)

func TestCheckpointAgentReplayUsesConsumedFloor(t *testing.T) {
	state := sessionState{Version: sessionStateVersion, ID: "checkpoint-agent", ICE: iceState{LocalUfrag: "checkpoint-agent", RemoteAddr: (&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9000}).AddrPort()},
		SRTP: srtpState{Profile: srtp.ProtectionProfileAeadAes128Gcm, Inbound: map[uint32]uint64{42: 180, 43: 200}}, Audio: trackState{MID: "0", Anchored: true, InboundSSRC: 42},
		Agent: agentState{State: agent.State{Version: 1, Bytes: make([]byte, 8)}, Progress: agent.Progress{SSRC: 42, Index: 100, Consumed: 10}}}
	raw, err := json.Marshal(snapshot{Version: sessionStateVersion, State: state, DTLSConnection: []byte{1}})
	require.NoError(t, err)
	floors, err := SnapshotReplayIndexes(raw)
	require.NoError(t, err)
	require.Equal(t, map[uint32]uint64{42: 100, 43: 200}, floors, "queued audio must be replayed even below the transport high water mark")
}

func TestAgentReplayRestoresReceiveWindowBeforeQueuedAudio(t *testing.T) {
	for _, first := range []uint64{65501, 0, 65530, 7<<16 | 65530} {
		t.Run(fmt.Sprintf("first=%d", first), func(t *testing.T) {
			progress := agent.Progress{}
			if first == 65501 {
				progress = agent.Progress{SSRC: 42, Index: first - 1, Consumed: 10}
			}
			const queued = 121 // 2.4 media seconds; more than the 64-packet SRTP window
			consumed := make(chan uint64, 128)
			w, err := New(Config{Agent: AgentConfig{QueueCapacity: 128, Factory: func(string) agent.Agent {
				return &countingAgent{process: func(_ context.Context, in agent.Input) error { consumed <- in.Progress.Index; return nil }}
			}}})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, w.Close()) })
			w.cfg.Relay = &RelayConfig{} // production source-rate observation only
			sink := listenTestUDP(t)
			profile := srtp.ProtectionProfileAeadAes128Gcm
			keys := testSessionKeys(t, profile)
			state := sessionState{ID: "queued-audio", ICE: iceState{RemoteAddr: sink.LocalAddr().(*net.UDPAddr).AddrPort()},
				SRTP: srtpState{Profile: profile, Inbound: map[uint32]uint64{42: first + queued - 1}}, Audio: trackState{MID: "0", PayloadType: 111, SSRC: 123, Anchored: true, InboundSSRC: 42, InitialSeq: 1200},
				Agent: agentState{State: agent.State{Version: 1, Bytes: make([]byte, 8)}, Progress: progress}}
			s := sessionFromState(w, state)
			t.Cleanup(s.close)
			opts := ResumeOptions{Kind: agent.Takeover, SequenceMargin: 8192, InputMayBeDuplicated: true, DuplicateWindows: []agent.DuplicateWindow{{SSRC: 42, First: first, Last: first + queued - 1}}}
			require.NoError(t, s.initAgent(true, opts))
			s.srtpIn = testContext(t, keys.RemoteMasterKey, keys.RemoteMasterSalt, profile)
			s.srtpOut = testContext(t, keys.LocalMasterKey, keys.LocalMasterSalt, profile)
			require.NoError(t, s.restoreReplayInbound(keys, opts))
			w.running.Add(1)
			go func() { defer w.running.Done(); s.agentLoop() }()
			caller := testContext(t, keys.RemoteMasterKey, keys.RemoteMasterSalt, profile)
			if first > 0 {
				require.NoError(t, restoreOutboundIndex(caller, 42, first-1))
			}
			for index := first; index <= first+queued-1; index++ {
				raw, err := (&rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 111, SSRC: 42, SequenceNumber: uint16(index), Timestamp: uint32(index) * 960}, Payload: []byte{0xf8, 0xff, 0xfe}}).Marshal() //nolint:gosec // RTP low bits
				require.NoError(t, err)
				encrypted, err := caller.EncryptRTP(nil, raw, nil)
				require.NoError(t, err)
				s.handleRTP(encrypted)
			}
			require.Zero(t, s.decryptFailures.Load()+s.replayRejections.Load(), "replay must admit the queued input, including across ROC wrap")
			for index := first; index <= first+queued-1; index++ {
				select {
				case got := <-consumed:
					require.Equal(t, index, got)
				case <-time.After(time.Second):
					t.Fatalf("missing consumed index %d", index)
				}
			}
			require.Zero(t, w.AgentStats().InputDrops)
			s.mu.Lock()
			var checkpoint CheckpointState
			s.checkpointRates(&checkpoint)
			s.mu.Unlock()
			require.InDelta(t, 50, checkpoint.RTPPacketRate, 1, "queued replay rates use each authenticated input index, not the transport high water mark")
		})
	}
}
