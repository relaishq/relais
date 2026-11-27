package callharness

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

func TestRelayLossEpisodeRetryCharacterization(t *testing.T) {
	for _, stage := range []string{"outstanding", "applied", "recovered", "expired"} {
		t.Run(stage, func(t *testing.T) {
			c := relayEpisodeCall()
			r := c.rec
			first := r.beginRelayRestart(r.start.Add(4 * time.Second))
			if stage == "applied" || stage == "recovered" {
				r.appliedRelayRestart(first, r.start.Add(5*time.Second), 1, "192.0.2.2:3478")
			}
			if stage == "recovered" {
				r.observeRelayRestarts(r.start.Add(7 * time.Second))
			}
			if stage == "expired" {
				r.observeRelayRestarts(r.start.Add(14 * time.Second))
			}
			before := c.RelayLossEpisodes()
			index := r.beginRelayRestart(r.start.Add(15 * time.Second))
			if stage == "outstanding" {
				require.Equal(t, first, index)
				require.Equal(t, before, c.RelayLossEpisodes())
			} else {
				require.Equal(t, 1, index)
				require.Len(t, c.RelayLossEpisodes(), 2)
				require.Equal(t, before[0], c.RelayLossEpisodes()[0], "a fresh restart must not repair or erase the first episode")
			}
		})
	}
}

// The host's public path disappears while its lease view remains live. This
// exercises real SDP/ICE and both caller trigger paths, without restoring it.
func TestRelayLossPublicIsolationCharacterization(t *testing.T) {
	characterizeRelayLossIsolation(t, false)
}

func TestRelayLossHostIsolationCharacterization(t *testing.T) {
	characterizeRelayLossIsolation(t, true)
}

func characterizeRelayLossIsolation(t *testing.T, stopPrimaryRenew bool) {
	t.Helper()
	store := sessionstore.NewMemory()
	p, w, h, relays, _ := relayChangeTopology(t, store, false, false)
	var blocked atomic.Bool
	h.external.CallerSocket = func() (net.PacketConn, error) {
		socket, err := net.ListenPacket("udp4", "127.0.0.1:0")
		return blockedRelaySocket{socket, &blocked, relays[0].PublicAddr().String()}, err
	}
	ctx, cancel := context.WithTimeout(t.Context(), 18*time.Second)
	defer cancel()
	leases := store
	var tokens []sessionstore.RelayLease
	for _, slot := range []string{"r1", "r2"} {
		token, err := leases.GetRelay(ctx, slot)
		require.NoError(t, err)
		tokens = append(tokens, token)
	}
	life, stopLife := context.WithCancel(ctx)
	renewed, detector := make(chan struct{}), make(chan error, 1)
	require.NoError(t, p.Heartbeat(w.LocalAddr()))
	go func() { detector <- p.Run(life) }()
	go func() {
		defer close(renewed)
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-life.Done():
				return
			case <-tick.C:
				_ = p.Heartbeat(w.LocalAddr())
				for i, token := range tokens {
					if stopPrimaryRenew && i == 0 && blocked.Load() {
						continue
					}
					tokens[i], _ = leases.RenewRelay(life, token, time.Minute)
				}
			}
		}
	}()
	defer func() { stopLife(); <-renewed; require.NoError(t, <-detector) }()
	call, err := h.Dial(ctx, CallOptions{Video: true, RelayChange: true, RelayLossEvidence: true, Recording: RecordingOptions{DecodeEvery: 30}})
	require.NoError(t, err)
	media, stop := context.WithCancel(ctx)
	sent := make(chan error, 1)
	go func() { defer close(sent); sent <- call.SendMedia(media, 15*time.Second) }()
	defer func() { stop(); <-sent }()
	require.Eventually(t, func() bool {
		r := call.RelaySnapshot()
		return len(r.Tracks) == 2 && r.Tracks[0].Packets > 20 && r.Tracks[1].Packets > 20
	}, 2*time.Second, 10*time.Millisecond)
	blocked.Store(true)
	require.Eventually(t, func() bool {
		_, a, e := store.RelayOwner(ctx, call.SessionID())
		return e == nil && a.Slot == "r2" && a.Pending == "" && a.Generation >= 1
	}, 6*time.Second, 10*time.Millisecond)
	require.Eventually(t, func() bool {
		rows := call.RelayLossEpisodes()
		return len(rows) > 0 && rows[len(rows)-1].FirstAudioNS > 0 && rows[len(rows)-1].FirstVideoNS > 0
	}, 2*time.Second, 10*time.Millisecond)
	stop()
	<-sent
	report, err := call.Hangup(ctx)
	require.NoError(t, err)
	rows := call.RelayLossEpisodes()
	t.Logf("episodes=%d iceRestarts=%d", len(rows), report.ICERestarts)
	for i, row := range rows {
		t.Logf("episode=%d generation=%d appliedNs=%d audioNs=%d videoNs=%d", i, row.Generation, row.AppliedNS, row.FirstAudioNS, row.FirstVideoNS)
	}
	require.NotEmpty(t, rows)
	require.Positive(t, report.ICERestarts)
	if !stopPrimaryRenew {
		require.Len(t, rows, 2)
	} else {
		require.LessOrEqual(t, len(rows), 2)
	}
	if len(rows) == 2 {
		require.EqualValues(t, 1, rows[0].Generation)
		require.EqualValues(t, 2, rows[1].Generation)
		require.Positive(t, rows[0].AppliedNS)
		require.Greater(t, rows[1].RestartStartNS, rows[0].AppliedNS)
		require.NotEqual(t, rows[0].AnswerCandidate, rows[1].AnswerCandidate)
	}
}
