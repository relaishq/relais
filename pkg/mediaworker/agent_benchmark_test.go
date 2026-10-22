package mediaworker

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"

	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/sessionstore"

	"github.com/pion/rtp"
	"github.com/pion/srtp/v3"
	"github.com/stretchr/testify/require"
)

// This packet benchmark also compiles unchanged on main. Both runs create a normal default
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
	require.Zero(b, s.decryptFailures.Load()+s.replayRejections.Load())
}

// Populated durable requests must not be copied by a scalar status observation.
// A successful fenced write establishes readiness and acknowledgement before timing.
func BenchmarkAgentSessionStatusDurableTasks(b *testing.B) {
	store := sessionstore.NewMemory()
	snap := componentFixture(b)
	snap.State.Agent.State.Bytes = []byte(`{"phase":"ready"}`)
	snap.State.Agent.V2.Name = "status/v2"
	snap.State.Agent.V2.Tasks = nil
	for i := range 8 {
		snap.State.Agent.V2.Tasks = append(snap.State.Agent.V2.Tasks, taskIntent{ID: agent.TaskID(string(rune('a' + i))), Step: "fetch", Phase: taskStarted, Request: make([]byte, 1024), RequestLength: 1024})
	}
	lease, err := store.Claim(context.Background(), snap.State.ID, netip.MustParseAddrPort("127.0.0.1:10000"), time.Hour)
	require.NoError(b, err)
	s, _ := dtlsCheckpointSession(b, store, snap, lease, ResumeOptions{})
	w := s.worker
	// Use the legacy envelope with room for all eight durable request payloads.
	// The status read is identical for legacy and split checkpoint layouts.
	w.cfg.SnapshotLimits = SnapshotLimits{MaxBytes: 64 << 10, BurstBytes: 256 << 10, BytesPerSecond: 80000}
	require.NoError(b, s.checkpoint.persist(context.Background(), checkpointRequired))
	w.sessions = map[string]*session{s.id: s}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		status, err := w.agentSessionStatus(s.id)
		if err != nil || len(status.Tasks) != 8 || !status.CheckpointKnown {
			b.Fatal("invalid populated status", err)
		}
	}
}
