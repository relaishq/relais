package callharness

import (
	"math/rand/v2"
	"net/netip"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/relayleg"
	"github.com/relaishq/relais/pkg/relay"
	"github.com/stretchr/testify/require"
)

// legReorderer reorders caller media on one private leg, the way #127 needs:
//   - each stream's newest datagram waits (up to hold) for its successor;
//   - with probability overtake, the successor passes first, so the waiting
//     datagram lands below the worker's high water mark (a gap);
//   - a drain marker passes at once, and every waiting datagram follows it
//     late, after the old worker has exported (a tail or a gap).
//
// Non-moving calls just see bounded reordering inside the replay window.
type legReorderer struct {
	leg      *privateLeg
	hold     time.Duration
	late     time.Duration
	overtake float64

	mu      sync.Mutex
	rng     *rand.Rand
	pending map[legStream]pendingDatagram
	stop    chan struct{}
	done    chan struct{}

	delayed, swapped atomic.Uint64
}

type legStream struct {
	caller netip.AddrPort
	ssrc   uint32
}

type pendingDatagram struct {
	datagram []byte
	at       time.Time
}

func newLegReorderer(leg *privateLeg, seed uint64) *legReorderer {
	r := &legReorderer{leg: leg, hold: 15 * time.Millisecond, late: 30 * time.Millisecond, overtake: 0.3,
		rng: rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)), pending: make(map[legStream]pendingDatagram),
		stop: make(chan struct{}), done: make(chan struct{})}
	leg.setIntercept(r.intercept)
	go r.flushLoop()
	return r
}

func (r *legReorderer) close() {
	r.leg.setIntercept(nil)
	close(r.stop)
	<-r.done
	r.mu.Lock()
	defer r.mu.Unlock()
	for key, p := range r.pending {
		r.leg.deliver(p.datagram)
		delete(r.pending, key)
	}
}

func (r *legReorderer) flushLoop() {
	defer close(r.done)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-r.stop:
			return
		case now := <-ticker.C:
			r.mu.Lock()
			for key, p := range r.pending {
				if now.Sub(p.at) >= r.hold {
					r.leg.deliver(p.datagram)
					delete(r.pending, key)
				}
			}
			r.mu.Unlock()
		}
	}
}

func (r *legReorderer) intercept(datagram []byte) bool {
	if _, ack, ok := relayleg.ParseBarrier(datagram); ok && !ack {
		// The marker overtakes everything still waiting on this leg.
		r.mu.Lock()
		var late [][]byte
		for key, p := range r.pending {
			late = append(late, p.datagram)
			delete(r.pending, key)
		}
		r.mu.Unlock()
		if len(late) > 0 {
			r.delayed.Add(uint64(len(late)))
			time.AfterFunc(r.late, func() {
				for _, d := range late {
					r.leg.deliver(d)
				}
			})
		}
		return false
	}
	caller, payload, err := relay.ParseHeader(datagram)
	if err != nil || len(payload) < 12 || payload[0]>>6 != 2 || payload[1] >= 192 && payload[1] <= 223 {
		return false
	}
	key := legStream{caller: caller, ssrc: uint32(payload[8])<<24 | uint32(payload[9])<<16 | uint32(payload[10])<<8 | uint32(payload[11])}
	r.mu.Lock()
	defer r.mu.Unlock()
	if prev, waiting := r.pending[key]; waiting {
		if r.rng.Float64() < r.overtake {
			r.swapped.Add(1)
			return false // the newer datagram overtakes the waiting one
		}
		r.leg.deliver(prev.datagram)
	}
	r.pending[key] = pendingDatagram{datagram: datagram, at: time.Now()}
	return true
}

// Twenty real Pion callers drain off one worker, then move between two
// others in repeated rounds, while every private leg reorders caller media
// around drain markers. No content may be lost or duplicated (#127). The
// proxies forward every datagram through one goroutine per leg; under the
// race detector ten calls keep them from becoming the bottleneck. The full
// 20-call form runs without it in make test-harness-timing.
func TestPlannedDrainReorderedPrivateInput(t *testing.T) {
	if testing.Short() {
		t.Skip("20-call reorder stress runs in the full harness")
	}
	calls, rounds, media := 20, 6, 10*time.Second
	if densityRaceDetector {
		calls = 10
	}
	if v := os.Getenv("RELAIS_REORDER_ROUNDS"); v != "" {
		var err error
		rounds, err = strconv.Atoi(v)
		require.NoError(t, err)
		media = time.Duration(rounds)*time.Second + 4*time.Second
	}
	sys := startReorderTopology(t, reorderOptions{workers: 3, buffer: true})
	var reorderers []*legReorderer
	for i, leg := range sys.legs {
		reorderers = append(reorderers, newLegReorderer(leg, uint64(time.Now().UnixNano())+uint64(i)))
	}
	dropsBefore := kernelUDPDrops(t)
	t.Cleanup(func() {
		stats := sys.relay.Stats()
		t.Logf("REORDER_DRAIN_RELAY worker_no_flow=%d hold_drops=%d recovered=%d releases=%d output_drains=%d output_drain_timeouts=%d", stats.WorkerNoFlow, stats.HoldDrops, stats.PlannedRecovered, stats.PlannedReleases, stats.OutputDrains, stats.OutputDrainTimeouts)
	})
	// Sequences wrap during the drain and move rounds, staggered per call, so
	// rollover and reordering meet in the same window (P1-A of the review).
	wrap := func(i int) *RTPSequenceNumbers {
		return &RTPSequenceNumbers{Audio: uint16(65535 - 40 - 10*i), Video: uint16(65535 - 40 - 5*i)} //nolint:gosec // small i
	}
	var wrapped int
	reports := testPlannedDrainCalls(t, sys.h, plannedDrainOptions{external: true, calls: calls, sequences: wrap, wrapped: &wrapped, rounds: rounds, media: media,
		workers: []netip.AddrPort{sys.workers[0].LocalAddr(), sys.workers[1].LocalAddr(), sys.workers[2].LocalAddr()}})
	dropsAfter := kernelUDPDrops(t)
	var delayed, swapped uint64
	for _, r := range reorderers {
		r.close()
		delayed += r.delayed.Load()
		swapped += r.swapped.Load()
	}
	stats := sys.relay.Stats()
	var durations, locks, audioGaps []time.Duration
	recovered, moves := 0, 0
	for _, report := range reports {
		require.Zero(t, report.Track(kindAudio).DuplicatePackets)
		require.Zero(t, report.Track(kindVideo).DuplicatePackets)
		for _, m := range report.Moves {
			moves++
			recovered += m.Result.RecoveredPackets
			durations = append(durations, m.Result.Duration)
			locks = append(locks, m.Result.ReleaseLockHeld)
			for _, track := range m.Tracks {
				if track.Kind == kindAudio {
					audioGaps = append(audioGaps, track.Gap)
				}
			}
		}
	}
	for i := range sys.workers {
		require.Zero(t, sys.workerMetric(t, i, "relais_worker_decryption_failures_total"), "worker %d", i)
		require.Zero(t, sys.workerMetric(t, i, "relais_worker_replay_rejections_total"), "worker %d", i)
	}
	t.Logf("REORDER_DRAIN wrapped_streams=%d moves=%d recovered=%d relay_recovered=%d filtered=%d incomplete=%d delayed=%d swapped=%d move_p50=%s move_p99=%s move_max=%s lock_p50=%s lock_p99=%s lock_max=%s relay_lock_max=%s audio_gap_p50=%s audio_gap_p99=%s audio_gap_max=%s kernel_udp_drops=%d",
		wrapped, moves, recovered, stats.PlannedRecovered, stats.PlannedFiltered, stats.PlannedIncomplete, delayed, swapped,
		percentile(durations, 50), percentile(durations, 99), percentile(durations, 100),
		percentile(locks, 50), percentile(locks, 99), percentile(locks, 100), stats.PlannedReleaseLockMax,
		percentile(audioGaps, 50), percentile(audioGaps, 99), percentile(audioGaps, 100), dropsAfter-dropsBefore)
	require.Equal(t, 2*calls, wrapped, "every audio and video stream wrapped during the run")
	require.EqualValues(t, moves, stats.PlannedReleases, "every move released after its exact export")
	require.Positive(t, delayed, "the reorderer delayed datagrams past drain markers")
	require.Positive(t, recovered, "the relay recovered late pre-marker input")
	require.EqualValues(t, recovered, stats.PlannedRecovered)
	require.Zero(t, stats.PlannedIncomplete)
	require.Zero(t, stats.HoldDrops)
	require.Zero(t, stats.HoldSendFailures)
	require.Zero(t, stats.WorkerNoFlow, "the old owner's pre-fence output is never dropped as unrouted")
	require.EqualValues(t, moves, stats.OutputDrains, "every planned route move drained the exported source's output")
}

func percentile(values []time.Duration, p int) time.Duration {
	if len(values) == 0 {
		return 0
	}
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	index := (len(sorted)*p+99)/100 - 1
	return sorted[min(max(index, 0), len(sorted)-1)]
}

// kernelUDPDrops reads the host's UDP receive-buffer drop counter. It is a
// host-wide observation for the measurement log, never a gate.
func kernelUDPDrops(t *testing.T) uint64 {
	t.Helper()
	switch runtime.GOOS {
	case "linux":
		data, err := os.ReadFile("/proc/net/snmp")
		if err != nil {
			return 0
		}
		lines := strings.Split(string(data), "\n")
		for i := 0; i+1 < len(lines); i++ {
			if !strings.HasPrefix(lines[i], "Udp: ") || !strings.HasPrefix(lines[i+1], "Udp: ") {
				continue
			}
			names, values := strings.Fields(lines[i]), strings.Fields(lines[i+1])
			for j, name := range names {
				if name == "RcvbufErrors" && j < len(values) {
					v, _ := strconv.ParseUint(values[j], 10, 64)
					return v
				}
			}
		}
	case "darwin":
		out, err := exec.Command("netstat", "-s", "-p", "udp").Output()
		if err != nil {
			return 0
		}
		if m := regexp.MustCompile(`(\d+) dropped due to full socket buffers`).FindSubmatch(out); m != nil {
			v, _ := strconv.ParseUint(string(m[1]), 10, 64)
			return v
		}
	}
	return 0
}
