package callharness

import (
	"context"
	"net"
	"net/http/httptest"
	"net/netip"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/relais/pkg/controlplane"
	"github.com/relais/pkg/mediaworker"
	"github.com/relais/pkg/relay"
	"github.com/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

// The private UDP leg can deliver a media datagram after the drain marker.
// Hold exactly one ciphertext datagram in a test proxy, pass the marker/ACK,
// and release that datagram only after the real old worker has exported.
// Set RELAIS_PLANNED_MOVE_REQUIRE_ZERO=1 to assert the requested zero-loss
// guarantee; this is a fault reproduction, not a change to production pacing.
func TestPlannedMoveReorderedPrivateInput(t *testing.T) {
	if os.Getenv("RELAIS_PLANNED_MOVE_REORDER") != "1" {
		t.Skip("opt-in private-leg ordering investigation")
	}
	for _, reorder := range []bool{false, true} {
		name := "ordered"
		if reorder {
			name = "reordered"
		}
		t.Run(name, func(t *testing.T) { testPlannedMovePrivateInputOrder(t, reorder, true) })
	}
	t.Run("delayed-without-move", func(t *testing.T) { testPlannedMovePrivateInputOrder(t, true, false) })
}
func testPlannedMovePrivateInputOrder(t *testing.T, reorder, move bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	store := sessionstore.NewMemory()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	proxy := conn.LocalAddr().(*net.UDPAddr).AddrPort()
	owners := &plannedProxyOwners{Owners: store, proxy: proxy}
	r, err := relay.New(relay.Config{Owners: owners, Buffer: &relay.BufferConfig{}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, r.Close()) })
	source, err := mediaworker.New(mediaworker.Config{Relay: &mediaworker.RelayConfig{Addr: proxy, PublicAddr: r.PublicAddr(), Owners: store}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, source.Close()) })
	owners.source = source.LocalAddr()
	target, err := mediaworker.New(mediaworker.Config{Relay: &mediaworker.RelayConfig{Addr: r.WorkerAddr(), PublicAddr: r.PublicAddr(), Owners: store}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, target.Close()) })
	var armed atomic.Bool
	captured := make(chan []byte, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 65535)
		for {
			n, from, err := conn.ReadFromUDPAddrPort(buf)
			if err != nil {
				return
			}
			to := r.WorkerAddr()
			if from == r.WorkerAddr() {
				to = source.LocalAddr()
				_, payload, err := relay.ParseHeader(buf[:n])
				if err == nil && len(payload) >= 12 && payload[0]>>6 == 2 && payload[1]&0x7f == opusPayloadType && armed.CompareAndSwap(true, false) {
					if !reorder {
						_, _ = conn.WriteToUDPAddrPort(buf[:n], to)
					}
					captured <- append([]byte(nil), buf[:n]...)
					continue
				}
			}
			_, _ = conn.WriteToUDPAddrPort(buf[:n], to)
		}
	}()
	t.Cleanup(func() { require.NoError(t, conn.Close()); <-done })
	translated := &plannedProxyRelay{Relay: r, source: source.LocalAddr(), proxy: proxy}
	plane := controlplane.New(translated, store)
	r.AddWorker(proxy)
	r.AddWorker(source.LocalAddr())
	r.AddWorker(target.LocalAddr())
	require.NoError(t, plane.Register("0", source.LocalAddr(), source))
	require.NoError(t, plane.Register("1", target.LocalAddr(), target))
	server := httptest.NewServer(plane.Handler())
	t.Cleanup(server.Close)
	h, err := Start(Options{External: &ExternalTopology{SignalingURL: server.URL + "/calls", RelayAddr: r.PublicAddr()}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, h.Close()) })
	call, err := h.Dial(ctx, CallOptions{})
	require.NoError(t, err)
	require.NoError(t, call.SendMedia(ctx, 300*time.Millisecond))
	src, err := newOpusSource(callerAudio)
	require.NoError(t, err)
	frame, duration, err := src.next()
	require.NoError(t, err)
	frame, err = call.rec.sendingAudio(frame, duration)
	require.NoError(t, err)
	armed.Store(true)
	require.NoError(t, call.audio.WriteSample(media.Sample{Data: frame, Duration: duration}))
	call.rec.sent(kindAudio, false)
	var late []byte
	select {
	case late = <-captured:
	case <-ctx.Done():
		t.Fatal("proxy did not capture input")
	}
	var result controlplane.MoveResult
	if move {
		result, err = plane.Move(ctx, call.SessionID(), "1")
		require.NoError(t, err)
		call.RecordExternalMove(result)
		require.Zero(t, source.SessionCount())
		require.Equal(t, 1, target.SessionCount())
	} else {
		require.Equal(t, 1, source.SessionCount())
		require.Zero(t, target.SessionCount())
	}
	require.Zero(t, result.Result.HeldPackets)
	_, payload, err := relay.ParseHeader(late)
	require.NoError(t, err)
	var header rtp.Header
	_, err = header.Unmarshal(payload)
	require.NoError(t, err)
	snapshot, err := store.GetState(ctx, call.SessionID())
	require.NoError(t, err)
	if !move {
		snapshot, err = source.SnapshotSession(call.SessionID())
		require.NoError(t, err)
	}
	inbound, err := mediaworker.SnapshotInboundIndexes(snapshot)
	require.NoError(t, err)
	if reorder {
		require.Equal(t, uint16(inbound[header.SSRC])+1, header.SequenceNumber, "the missing tail was never represented in the final export")
		_, err = conn.WriteToUDPAddrPort(late, source.LocalAddr())
		require.NoError(t, err)
	} else {
		require.Equal(t, uint16(inbound[header.SSRC]), header.SequenceNumber)
	}
	require.NoError(t, call.SendMedia(ctx, 300*time.Millisecond))
	time.Sleep(50 * time.Millisecond)
	call.rec.mu.Lock()
	unit := call.rec.sentAudioUnits[string(frame)]
	call.rec.mu.Unlock()
	lost := 0
	if unit.returnedAt == 0 {
		lost = 1
	}
	expected := 0
	if reorder && move {
		expected = 1
	}
	if os.Getenv("RELAIS_PLANNED_MOVE_REQUIRE_ZERO") == "1" {
		expected = 0
	}
	active := target
	if !move {
		active = source
	}
	failures, err := active.SessionDecryptFailures(call.SessionID())
	require.NoError(t, err)
	require.Zero(t, failures)
	require.Zero(t, r.Stats().WorkerNoFlow)
	require.Zero(t, r.Stats().HoldDrops)
	require.Zero(t, r.Stats().HoldSendFailures)
	t.Logf("PLANNED_REORDER moved=%t reordered=%t input_lost=%d held=%d worker_no_flow=%d decrypt_failures=%d snapshot_inbound=%d delayed_sequence=%d duration=%s", move, reorder, lost, result.Result.HeldPackets, r.Stats().WorkerNoFlow, failures, inbound[header.SSRC], header.SequenceNumber, result.Result.Duration)
	report, err := call.Hangup(ctx)
	require.NoError(t, err)
	require.True(t, report.ConnectedThroughout())
	require.Zero(t, report.DecryptionFailures.Total())
	require.Zero(t, report.ICERestarts)
	require.Zero(t, report.Renegotiations)
	if move {
		require.Len(t, report.Moves, 1)
		require.Equal(t, lost, report.Moves[0].Measurement.ContentLoss.Event.Audio.Lost)
	} else {
		require.Empty(t, report.Moves)
	}
	require.Equal(t, expected, lost, "pre-hold ciphertext arrived at the exported source after the barrier ACK; the planned move never replays it")
}

type plannedProxyOwners struct {
	sessionstore.Owners
	source, proxy netip.AddrPort
}

func (o *plannedProxyOwners) Owner(ctx context.Context, id string) (netip.AddrPort, error) {
	owner, err := o.Owners.Owner(ctx, id)
	if owner == o.source {
		owner = o.proxy
	}
	return owner, err
}

type plannedProxyRelay struct {
	*relay.Relay
	source, proxy netip.AddrPort
}

func (r *plannedProxyRelay) HoldSession(ctx context.Context, id string, from netip.AddrPort) error {
	if from == r.source {
		from = r.proxy
	}
	return r.Relay.HoldSession(ctx, id, from)
}
func (r *plannedProxyRelay) MoveSession(id string, from, to netip.AddrPort) error {
	if from == r.source {
		from = r.proxy
	}
	return r.Relay.MoveSession(id, from, to)
}
