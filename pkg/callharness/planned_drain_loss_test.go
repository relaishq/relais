package callharness

import (
	"context"
	"net/netip"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/relais/internal/workerprobe"
	"github.com/relais/pkg/controlplane"
	"github.com/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

// Twenty real Pion callers move together through the production drain/barrier
// path. Content identities detect a missing input even when move timing is good.
func TestPlannedDrainTwentyCallsNoContentLoss(t *testing.T) {
	if os.Getenv("RELAIS_PLANNED_MOVE_STRESS") != "1" {
		t.Skip("set RELAIS_PLANNED_MOVE_STRESS=1 for the 20-call investigation")
	}

	t.Run("native-memory", func(t *testing.T) {
		h, err := Start(Options{Relay: true, Workers: 3})
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, h.Close()) })
		testPlannedDrainCalls(t, h, plannedDrainOptions{workers: []netip.AddrPort{h.workers.list[0].LocalAddr(), h.workers.list[1].LocalAddr(), h.workers.list[2].LocalAddr()}})
	})
	t.Run("http-buffer", func(t *testing.T) {
		relayBufferStores(t, func(t *testing.T, store sessionstore.Store) {
			sys := startBufferFailureSystem(t, store, bufferFailureConfig{workers: 3, persist: true, cache: true})
			testPlannedDrainCalls(t, sys.h, plannedDrainOptions{external: true, workers: []netip.AddrPort{sys.workers[0].LocalAddr(), sys.workers[1].LocalAddr(), sys.workers[2].LocalAddr()}})
			require.Zero(t, sys.r.Stats().HoldDrops)
			require.Zero(t, sys.r.Stats().HoldSendFailures)
			require.Zero(t, sys.r.Stats().WorkerNoFlow)
		})
	})
}

// plannedDrainOptions shape the 20-call drain. Zero rounds and media select
// the original 16 move rounds and 25 s of media.
type plannedDrainOptions struct {
	external bool
	calls    int // 20 when zero
	// sequences optionally fixes call i's first RTP sequence numbers;
	// wrapped then receives how many caller SSRCs crossed a sequence wrap.
	sequences func(i int) *RTPSequenceNumbers
	wrapped   *int
	rounds    int
	media    time.Duration
	workers  []netip.AddrPort
}

func testPlannedDrainCalls(t *testing.T, h *Harness, opts plannedDrainOptions) []*Report {
	external, workers := opts.external, opts.workers
	rounds, mediaDuration := opts.rounds, opts.media
	if rounds == 0 {
		rounds = 16
	}
	if mediaDuration == 0 {
		mediaDuration = 25 * time.Second
	}
	trace := newPlannedMovePacketTrace(t, workers)
	ctx, cancel := context.WithTimeout(context.Background(), mediaDuration+35*time.Second)
	defer cancel()
	callCount := opts.calls
	if callCount == 0 {
		callCount = 20
	}
	calls := make([]*Call, callCount)
	sent := make(chan error, len(calls))
	var wg sync.WaitGroup
	slots := make(chan struct{}, 4)
	errs := make(chan error, len(calls))
	for i := range calls {
		wg.Go(func() {
			slots <- struct{}{}
			defer func() { <-slots }()
			var dialErr error
			var sequences *RTPSequenceNumbers
			if opts.sequences != nil {
				sequences = opts.sequences(i)
			}
			calls[i], dialErr = h.Dial(ctx, CallOptions{Video: true, Worker: 0, InitialSequenceNumbers: sequences, Recording: RecordingOptions{History: 15 * time.Second, DecodeEvery: 30}})
			errs <- dialErr
		})
	}
	wg.Wait()
	for range calls {
		require.NoError(t, <-errs)
	}
	t.Logf("%d callers connected", len(calls))
	for _, c := range calls {
		trace.observeCaller(c)
	}
	for _, c := range calls {
		go func() { sent <- c.SendMedia(ctx, mediaDuration) }()
	}
	time.Sleep(time.Second)
	if external {
		h.statusPollMu.RLock()
		var result controlplane.DrainResult
		err := h.controlRequest(ctx, "POST", "/workers/0/drain", &result)
		h.statusPollMu.RUnlock()
		require.NoError(t, err)
		require.Empty(t, result.Error)
		require.Len(t, result.Moves, len(calls))
		for _, m := range result.Moves {
			for _, c := range calls {
				if c.SessionID() == m.ID {
					c.RecordExternalMove(m)
				}
			}
		}
	} else {
		require.NoError(t, h.Drain(0))
	}
	t.Log("drain complete")
	for range rounds {
		for _, c := range calls {
			wg.Go(func() {
				c.rec.mu.Lock()
				last := c.rec.moves[len(c.rec.moves)-1].to
				c.rec.mu.Unlock()
				if external {
					errs <- c.MoveExternal(ctx, 3-last)
				} else {
					errs <- c.Handover(HandoverOptions{To: 3 - last})
				}
			})
		}
		wg.Wait()
		for range calls {
			require.NoError(t, <-errs)
		}
		time.Sleep(100 * time.Millisecond)
	}
	for range calls {
		require.NoError(t, <-sent)
	}
	t.Log("media send complete")
	var reports []*Report
	for i, c := range calls {
		report, err := c.Hangup(ctx)
		require.NoError(t, err)
		reports = append(reports, report)
		require.Len(t, report.Moves, rounds+1)
		for _, m := range report.Moves {
			if m.Measurement.AudioLoss.Failed || m.Measurement.VideoLoss.Failed {
				trace.logMissing(t, c.SessionID())
				c.rec.mu.Lock()
				for key, u := range c.rec.sentAudioUnits {
					if missingInWindow(u, m.Measurement) {
						t.Logf("call=%d missing audio key=%x unit=%+v", i, key, u)
					}
				}
				for key, f := range c.rec.sentVideoFrames {
					if missingInWindow(f.unit(), m.Measurement) {
						t.Logf("call=%d missing video id=%d at=%s returned=%s", i, key, f.at, f.returnedAt)
					}
				}
				c.rec.mu.Unlock()
			}
			require.True(t, m.Measurement.AudioLoss.Trusted)
			require.True(t, m.Measurement.VideoLoss.Trusted)
			require.False(t, m.Measurement.AudioLoss.Failed, "call=%d %s", i, m.Measurement.Summary())
			require.False(t, m.Measurement.VideoLoss.Failed, "call=%d %s", i, m.Measurement.Summary())
		}
		require.Zero(t, report.DecryptionFailures.Total())
	}
	if opts.wrapped != nil {
		trace.mu.Lock()
		*opts.wrapped = len(trace.wrapped)
		trace.mu.Unlock()
	}
	return reports
}

// Retain ciphertext headers at both caller socket boundaries and the existing
// authenticated worker probes. On failure, identify the first missing stage.
// This observer changes no pacing, forwarding, cryptographic or export logic.
type plannedPacketKey struct {
	ssrc uint32
	seq  uint16
}
type plannedMovePacketTrace struct {
	mu             sync.Mutex
	sent, returned map[string]map[plannedPacketKey]int
	decrypted      map[plannedPacketKey]int
	wrapped        map[uint32]bool // caller SSRCs authenticated past one ROC
	echoed         map[string]map[plannedPacketKey]int
}

func newPlannedMovePacketTrace(t *testing.T, workers []netip.AddrPort) *plannedMovePacketTrace {
	p := &plannedMovePacketTrace{sent: map[string]map[plannedPacketKey]int{}, returned: map[string]map[plannedPacketKey]int{}, decrypted: map[plannedPacketKey]int{}, wrapped: map[uint32]bool{}, echoed: map[string]map[plannedPacketKey]int{}}
	for _, w := range workers {
		require.NoError(t, workerprobe.SetAfterDecrypt(w, func(_ string, ssrc uint32, index uint64) {
			p.mu.Lock()
			p.decrypted[plannedPacketKey{ssrc: ssrc, seq: uint16(index)}]++
			if index > 0xffff {
				p.wrapped[ssrc] = true
			}
			p.mu.Unlock()
		}))
		require.NoError(t, workerprobe.SetAfterEcho(w, func(_ context.Context, id string, raw []byte) { p.note(p.echoed, id, raw) }))
	}
	return p
}
func (p *plannedMovePacketTrace) note(dst map[string]map[plannedPacketKey]int, id string, raw []byte) {
	if len(raw) < 12 || raw[0]>>6 != 2 || raw[1] >= 192 && raw[1] <= 223 {
		return
	}
	var h rtp.Header
	if _, err := h.Unmarshal(raw); err != nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if dst[id] == nil {
		dst[id] = map[plannedPacketKey]int{}
	}
	dst[id][plannedPacketKey{ssrc: h.SSRC, seq: h.SequenceNumber}]++
}
func (p *plannedMovePacketTrace) observeCaller(c *Call) {
	id := c.SessionID()
	c.socket.observer.mu.Lock()
	defer c.socket.observer.mu.Unlock()
	c.socket.observer.inspectSend = func(raw []byte) { p.note(p.sent, id, raw) }
	c.socket.observer.inspect = func(raw []byte) { p.note(p.returned, id, raw) }
}
func (p *plannedMovePacketTrace) logMissing(t *testing.T, id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for k := range p.sent[id] {
		if p.decrypted[k] == 0 {
			t.Logf("session=%s sent caller SSRC=%d seq=%d never authenticated by any worker", id, k.ssrc, k.seq)
		}
	}
	for k := range p.echoed[id] {
		if p.returned[id][k] == 0 {
			t.Logf("session=%s echoed worker SSRC=%d seq=%d never reached caller raw UDP", id, k.ssrc, k.seq)
		}
	}
	t.Logf("session=%s raw_sent=%d raw_returned=%d worker_echoed=%d", id, len(p.sent[id]), len(p.returned[id]), len(p.echoed[id]))
}
