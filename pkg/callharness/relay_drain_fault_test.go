package callharness

import (
	"context"
	"io"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/stun/v4"
	"github.com/relaishq/relais/internal/relaytrace"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

const (
	faultOff = iota
	// faultRace fails the caller's next audio RTP write with
	// io.ErrClosedPipe. Armed while the caller restarts ICE, it stands for a
	// write that chose the old pair just before Pion's ICE restart closed
	// that pair's connection (#491).
	faultRace
	// faultDrop discards the caller's audio RTP to the new relay: the caller's
	// audio never reaches it, so the worker has none to send back there.
	faultDrop
)

// callerWriteFault is one caller's media socket with an armed write fault.
type callerWriteFault struct {
	net.PacketConn
	target netip.AddrPort // the new relay's public address
	mode   atomic.Int32
	raced  chan struct{} // closed by the failed write
}

func (f *callerWriteFault) WriteTo(p []byte, addr net.Addr) (int, error) {
	to := addr.(*net.UDPAddr).AddrPort()
	to = netip.AddrPortFrom(to.Addr().Unmap(), to.Port())
	// SRTP keeps the RTP header in the clear; RTCP types 200-204 never match.
	if !stun.IsMessage(p) && len(p) >= 2 && p[0]>>6 == 2 && p[1]&0x7f == opusPayloadType {
		switch f.mode.Load() {
		case faultRace:
			if f.mode.CompareAndSwap(faultRace, faultOff) {
				close(f.raced)
				return 0, io.ErrClosedPipe
			}
		case faultDrop:
			if to == f.target {
				return len(p), nil
			}
		}
	}
	return f.PacketConn.WriteTo(p, addr)
}

// faultTopology is relayChangeTopology with callers whose sockets can fault.
// It dials calls until want of them are on r1, hangs up the rest, and
// returns the r1 calls with their sockets.
func faultTopology(t *testing.T, ctx context.Context, store sessionstore.Store, want int) (*controlplane.Plane, *mediaworker.Worker, []*Call, []*callerWriteFault) {
	t.Helper()
	p, w, h, relays, _ := relayChangeTopology(t, store, false, false)
	var mu sync.Mutex
	var sockets []*callerWriteFault
	faulty, err := Start(Options{External: &ExternalTopology{
		SignalingURL: h.external.SignalingURL,
		RelayAddr:    h.external.RelayAddr,
		CallerSocket: func() (net.PacketConn, error) {
			conn, err := net.ListenUDP("udp4", net.UDPAddrFromAddrPort(netip.MustParseAddrPort("127.0.0.1:0")))
			if err != nil {
				return nil, err
			}
			f := &callerWriteFault{PacketConn: conn, target: relays[1].PublicAddr(), raced: make(chan struct{})}
			mu.Lock()
			sockets = append(sockets, f)
			mu.Unlock()
			return f, nil
		},
	}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, faulty.Close()) })
	var calls, other []*Call
	var faults []*callerWriteFault
	for len(calls) < want {
		require.Less(t, len(calls)+len(other), 4*want)
		c, e := faulty.Dial(ctx, CallOptions{Video: true, RelayChange: true})
		require.NoError(t, e)
		_, a, e := store.(sessionstore.RelayAssignments).RelayOwner(ctx, c.SessionID())
		require.NoError(t, e)
		if a.Slot == "r1" {
			mu.Lock()
			faults = append(faults, sockets[len(sockets)-1])
			mu.Unlock()
			calls = append(calls, c)
		} else {
			other = append(other, c)
		}
	}
	for _, c := range other {
		_, e := c.Hangup(ctx)
		require.NoError(t, e)
	}
	return p, w, calls, faults
}

// TestRelayDrainSurvivesCallerRestartWriteRace forces the #491 failure: the
// caller's audio write races its own ICE restart and fails with
// io.ErrClosedPipe. The caller then stopped sending audio for good, so the
// worker had no audio to return on the new relay and the drain reported
// delivery_unconfirmed for MID 0 at the 10 s planned limit. A browser keeps
// sending through its ICE restart; so must the test caller.
func TestRelayDrainSurvivesCallerRestartWriteRace(t *testing.T) {
	forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		p, _, calls, faults := faultTopology(t, ctx, store, 1)
		watch := watchRelayAttempts(store, calls)
		sent := make(chan error, 1)
		go func() { sent <- watch.sendMedia(ctx, calls[0], 6*time.Second) }()
		time.Sleep(time.Second)
		// The probe runs on the caller's restart goroutine once its ICE
		// restart has started and before CreateOffer restarts the agent.
		// Holding it there makes the next audio write fail inside the restart.
		var raced atomic.Bool
		stop := relaytrace.Observe(func(_ time.Time, name, _ string, _ []byte) {
			if name == "offer-start-pair" && faults[0].mode.CompareAndSwap(faultOff, faultRace) {
				select {
				case <-faults[0].raced:
					raced.Store(true)
				case <-time.After(2 * time.Second):
				}
			}
		})
		defer stop()
		start := time.Now()
		err := p.DrainRelay(ctx, "r1")
		stop()
		watch.stop()
		require.NoError(t, err, "drain evidence:\n%s", watch.evidence(calls, start))
		require.True(t, raced.Load(), "the forced write race must have happened")
		require.NoError(t, <-sent, "a write that races the caller's own ICE restart drops one frame, not the track")
		_, a, err := store.(sessionstore.RelayAssignments).RelayOwner(ctx, calls[0].SessionID())
		require.NoError(t, err)
		require.Equal(t, sessionstore.RelayAssignment{Slot: "r2", Generation: 1}, a)
		_, err = calls[0].Hangup(ctx)
		require.NoError(t, err)
	})
}

// TestRelayDrainUnconfirmedCallEvidence records what happens today when one
// drained call's caller media really is missing on the new relay: the
// caller's audio never reaches r2. The other call confirms; the drain reports
// the one per-call delivery_unconfirmed error at the planned limit; control's
// saved attempt, the worker's per-MID evidence and the caller's report all
// name audio (MID 0) as the missing track; and the call is not ended.
func TestRelayDrainUnconfirmedCallEvidence(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for the 10 s planned limit")
	}
	forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		p, w, calls, faults := faultTopology(t, ctx, store, 2)
		watch := watchRelayAttempts(store, calls)
		media, stopMedia := context.WithCancel(ctx)
		defer stopMedia()
		sent := make(chan error, len(calls))
		for _, c := range calls {
			go func() { sent <- watch.sendMedia(media, c, 13*time.Second) }()
		}
		time.Sleep(time.Second)
		faults[0].mode.Store(faultDrop)
		start := time.Now()
		err := p.DrainRelay(ctx, "r1")
		elapsed := time.Since(start)
		confirmations := store.(sessionstore.RelayConfirmations)
		missing, confirmed := calls[0].SessionID(), calls[1].SessionID()
		after, afterRec, afterErr := confirmations.RelayConfirmation(ctx, missing)
		intents, intentsErr := store.(sessionstore.ControlStore).ControlIntents(ctx)
		watch.stop()
		t.Logf("drain evidence:\n%s", watch.evidence(calls, start))
		require.ErrorContains(t, err, "relay change unconfirmed: delivery_unconfirmed")
		require.Equal(t, 1, strings.Count(err.Error(), "delivery_unconfirmed"), "one per-call error, for the one call")
		require.GreaterOrEqual(t, elapsed, 10*time.Second, "the attempt fails at the planned limit (relayPlannedLimit)")

		a, rec, e := confirmations.RelayConfirmation(ctx, confirmed)
		require.NoError(t, e)
		require.Equal(t, sessionstore.RelayAssignment{Slot: "r2", Generation: 1}, a)
		require.Equal(t, sessionstore.RelayPhaseConfirmed, rec.Attempt.Phase)

		watch.mu.Lock()
		failed := watch.attempts[missing][1]
		watch.mu.Unlock()
		require.Equal(t, "r2", failed.Slot)
		require.Equal(t, []string{"0", "1"}, failed.Required)
		require.Equal(t, []string{"1"}, failed.Delivered, "video was delivered; audio has no evidence")

		report, e := w.RelayDelivery(ctx, missing, 1)
		require.NoError(t, e)
		require.True(t, report.Switched)
		require.Equal(t, "r2", report.Slot)
		require.Equal(t, []string{"0", "1"}, report.Required)
		require.Equal(t, []string{"1"}, report.Delivered)

		for _, track := range calls[0].rec.report().Tracks {
			if track.Kind == kindAudio {
				require.True(t, calls[0].rec.start.Add(track.LastArrival).Before(failed.CommitAt), "the caller really received no audio on r2")
			}
		}
		// The call's relay status right after the error: still on r2 at
		// generation 1 (a committed generation is never undone), the attempt
		// expired with only video delivered, and a recovery episode started
		// (ADR 0016 section 7: switched and unconfirmed at the planned
		// deadline). The call is not ended.
		require.NoError(t, afterErr)
		require.Equal(t, sessionstore.RelayAssignment{Slot: "r2", Generation: 1}, after)
		require.Equal(t, sessionstore.RelayPhaseExpired, afterRec.Attempt.Phase)
		require.Equal(t, []string{"1"}, afterRec.Attempt.Delivered)
		require.NoError(t, intentsErr)
		require.True(t, slices.ContainsFunc(intents, func(i sessionstore.ControlIntent) bool { return i.ID == "relay-recovery:"+missing }), "recovery owns the call after the planned limit")
		stopMedia()
		for range calls {
			<-sent
		}
		for _, c := range calls {
			_, e := c.Hangup(ctx)
			require.NoError(t, e)
		}
	})
}
