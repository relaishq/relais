package callharness

import (
	"context"
	"net"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/relais/internal/workerprobe"
	"github.com/relais/pkg/mediaworker"
	"github.com/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

// A single missing input packet must not cause cached-picture replay to discard
// retained video. Exercise the relay, control plane and worker's private HTTP
// resume path, with the checkpoint fixed at the last pre-outage wire packet.
func TestNetworkLossTakeoverRetainsVideo(t *testing.T) {
	relayBufferStores(t, func(t *testing.T, store sessionstore.Store) {
		for _, scenario := range []struct {
			name string
			drop uint8
		}{
			{"clean-cache-on", 0},
			{"audio-hole-cache-on", opusPayloadType},
			{"video-hole-cache-on", vp8PayloadType},
		} {
			t.Run(scenario.name, func(t *testing.T) {
				sys := startBufferFailureSystem(t, store, bufferFailureConfig{cache: true, workers: 2})
				conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
				require.NoError(t, err)
				wire := &networkLossConn{PacketConn: conn, gate: make(chan struct{}), timestamps: make(map[uint32]uint16), last: make(map[uint32]uint16)}
				t.Cleanup(wire.release)
				sys.h.external.CallerSocket = func() (net.PacketConn, error) { return wire, nil }
				ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
				defer cancel()
				call, err := sys.h.Dial(ctx, CallOptions{Video: true, InitialSequenceNumbers: &RTPSequenceNumbers{Audio: 1000, Video: 2000}})
				require.NoError(t, err)
				var mu sync.Mutex
				decrypted := make(map[uint16]bool)
				echoed := make(map[uint16]bool)
				require.NoError(t, workerprobe.SetAfterDecrypt(sys.workers[1].LocalAddr(), func(id string, ssrc uint32, index uint64) {
					if id == call.SessionID() && ssrc == call.videoSSRC {
						mu.Lock()
						decrypted[uint16(index)] = true //nolint:gosec // bounded probe never wraps
						mu.Unlock()
					}
				}))
				require.NoError(t, workerprobe.SetAfterEcho(sys.workers[1].LocalAddr(), func(_ context.Context, id string, raw []byte) {
					var packet rtp.Packet
					if id != call.SessionID() || packet.Unmarshal(raw) != nil || packet.PayloadType != vp8PayloadType {
						return
					}
					var desc codecs.VP8Packet
					if _, err := desc.Unmarshal(packet.Payload); err == nil {
						mu.Lock()
						echoed[desc.PictureID] = true
						mu.Unlock()
					}
				}))
				sent := make(chan error, 1)
				go func() { sent <- call.SendMedia(ctx, 5*time.Second) }()
				time.Sleep(1250 * time.Millisecond)
				wire.mu.Lock()
				wire.paused = true
				wire.mu.Unlock()
				// Quiesce writes and drain the loopback leg. Assert below that the
				// checkpoint includes every packet actually transmitted before pause.
				time.Sleep(50 * time.Millisecond)
				ready := make(chan struct{})
				var once sync.Once
				require.NoError(t, workerprobe.SetBeforeSnapshot(sys.workers[0].LocalAddr(), func(lifetime context.Context, id string) {
					if id == call.SessionID() {
						once.Do(func() { close(ready) })
						<-lifetime.Done()
					}
				}))
				select {
				case <-ready:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				state, err := sys.workers[0].SnapshotSession(call.SessionID())
				require.NoError(t, err)
				inbound, err := mediaworker.SnapshotInboundIndexes(state)
				require.NoError(t, err)
				wire.mu.Lock()
				for ssrc, seq := range wire.last {
					require.EqualValues(t, seq, inbound[ssrc], "checkpoint covers all pre-pause source packets")
				}
				wire.drop = scenario.drop
				wire.mu.Unlock()
				lease, err := store.Get(ctx, call.SessionID())
				require.NoError(t, err)
				require.NoError(t, store.PutState(ctx, lease, state))
				killed := time.Now()
				require.NoError(t, workerprobe.Kill(sys.workers[0].LocalAddr()))
				wire.release()
				status := waitBufferTakeovers(t, ctx, sys.h, 1)
				require.NoError(t, <-sent)
				workerFailures, err := sys.workers[1].SessionDecryptFailures(call.SessionID())
				require.NoError(t, err)
				require.Zero(t, workerFailures)
				report, err := call.Hangup(ctx)
				require.NoError(t, err)
				require.Zero(t, report.DecryptionFailures.Total())
				require.Len(t, report.Moves, 1)
				wire.mu.Lock()
				mu.Lock()
				call.rec.mu.Lock()
				var lost []networkLossLostFrame
				for picture, source := range call.rec.sentVideoFrames {
					if !source.written || source.returnedAt > 0 {
						continue
					}
					f := networkLossLostFrame{PictureID: picture, SentFromKillMS: float64(source.at-killed.Sub(report.StartedAt)) / float64(time.Millisecond), WorkerEcho: echoed[picture]}
					for _, packet := range wire.video {
						if packet.pictureID == picture {
							f.SentPackets++
							if f.SentPackets == 1 {
								f.WireSendFromKillMS = float64(packet.at.Sub(killed)) / float64(time.Millisecond)
							}
							if decrypted[packet.seq] {
								f.WorkerDecryptedPackets++
							}
						}
					}
					lost = append(lost, f)
				}
				call.rec.mu.Unlock()
				mu.Unlock()
				dropped, droppedPicture := wire.dropped, wire.droppedPicture
				wire.mu.Unlock()
				sort.Slice(lost, func(i, j int) bool { return lost[i].PictureID < lost[j].PictureID })
				t.Logf("NETWORK_LOSS scenario=%s dropped=%d relay_complete=%t relay_packets=%d video_lost=%d lost=%+v", scenario.name, dropped, status.Takeovers[0].Result.RelayReplayComplete, status.Takeovers[0].Result.RelayReplayPackets, len(lost), lost)
				require.Positive(t, status.Takeovers[0].Result.RelayReplayPackets)
				if scenario.drop == 0 {
					require.Zero(t, dropped)
					require.True(t, status.Takeovers[0].Result.RelayReplayComplete)
					require.Zero(t, report.SentVideo.KeyframeRequests)
					require.Empty(t, lost)
				} else {
					require.Equal(t, 1, dropped)
					require.False(t, status.Takeovers[0].Result.RelayReplayComplete)
					require.True(t, status.Takeovers[0].Result.RelayReplayRecoveryPLI)
					require.Positive(t, report.SentVideo.KeyframeRequests)
				}
				if scenario.drop == vp8PayloadType {
					require.Len(t, lost, 1, "only the deliberately dropped picture may be lost")
					require.Equal(t, droppedPicture, lost[0].PictureID)
				} else {
					require.Empty(t, lost, "a missing audio packet must not discard retained video")
				}
			})
		}
	})
}

type networkLossLostFrame struct {
	PictureID              uint16
	SentFromKillMS         float64
	WireSendFromKillMS     float64
	SentPackets            int
	WorkerDecryptedPackets int
	WorkerEcho             bool
}

type networkLossConn struct {
	net.PacketConn
	mu             sync.Mutex
	paused         bool
	gate           chan struct{}
	once           sync.Once
	drop           uint8
	dropped        int
	droppedPicture uint16
	timestamps     map[uint32]uint16
	last           map[uint32]uint16
	video          []networkLossVideoPacket
}

type networkLossVideoPacket struct {
	pictureID, seq uint16
	at             time.Time
}

func (c *networkLossConn) release() { c.once.Do(func() { close(c.gate) }) }

func (c *networkLossConn) WriteTo(raw []byte, addr net.Addr) (int, error) {
	var header rtp.Header
	if len(raw) < 12 || raw[0]>>6 != 2 {
		return c.PacketConn.WriteTo(raw, addr)
	}
	if _, err := header.Unmarshal(raw); err != nil || (header.PayloadType != opusPayloadType && header.PayloadType != vp8PayloadType) {
		return c.PacketConn.WriteTo(raw, addr)
	}
	c.mu.Lock()
	paused := c.paused
	c.mu.Unlock()
	if paused {
		<-c.gate
	}
	c.mu.Lock()
	c.last[header.SSRC] = header.SequenceNumber
	if header.PayloadType == vp8PayloadType {
		picture, ok := c.timestamps[header.Timestamp]
		if !ok {
			picture = uint16(len(c.timestamps)) //nolint:gosec // bounded probe never wraps
			c.timestamps[header.Timestamp] = picture
		}
		c.video = append(c.video, networkLossVideoPacket{picture, header.SequenceNumber, time.Now()})
	}
	drop := c.drop != 0 && c.drop == header.PayloadType
	if drop {
		c.drop = 0
		c.dropped++
		if header.PayloadType == vp8PayloadType {
			c.droppedPicture = c.timestamps[header.Timestamp]
		}
	}
	c.mu.Unlock()
	if drop {
		return len(raw), nil // UDP write succeeds; the datagram is lost on uplink.
	}
	return c.PacketConn.WriteTo(raw, addr)
}
