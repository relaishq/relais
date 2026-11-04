package mediaworker

import (
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/srtp/v3"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/relaishq/relais/internal/udpbuffer"
	"github.com/relaishq/relais/pkg/relay"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

// burstPayload is a keyframe-sized video payload: a packet on the wire is
// this plus the RTP header and the GCM tag, about 1130 bytes.
const burstPayload = 1100

// burstDatagramCharge is a conservative bound on the receive-buffer memory
// the kernel charges for one queued datagram of about 1.2 KB, including its
// bookkeeping (Linux skb truesize, macOS mbuf cluster).
const burstDatagramCharge = 4096

// unitBurst is the burst size for unit tests: 16 datagrams need at most
// 64 KiB, well inside even a default receive buffer (about 208 KiB on Linux),
// whatever the host grants the worker's 8 MiB request.
const unitBurst = 16

// burstFixture sends fixed bursts of caller SRTP video over real UDP into a
// worker's read loop and times how long the loop takes to echo all of them
// (issue #139). Behind a relay, one fake relay socket sends every caller's
// packets with relay-leg headers and receives the echoes, as in the soak.
// Directly, each caller has its own socket and the echoes go to one sink.
type burstFixture struct {
	w        *Worker
	relayed  bool
	callers  []*burstCaller
	senders  []*net.UDPConn // one per caller directly; the fake relay behind a relay
	sink     *net.UDPConn   // where echoes arrive
	sinkBuf  udpbuffer.Result
	target   netip.AddrPort // the worker's UDP socket
	datagram []byte
}

type burstCaller struct {
	addr netip.AddrPort // the caller address the worker sees
	ctx  *srtp.Context  // the caller's outbound SRTP
	seq  uint16
	ts   uint32
}

func newBurstFixture(tb testing.TB, relayed bool, callers int) *burstFixture {
	tb.Helper()
	listen := func() *net.UDPConn {
		conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		require.NoError(tb, err)
		tb.Cleanup(func() { _ = conn.Close() })
		return conn
	}
	addrOf := func(conn *net.UDPConn) netip.AddrPort { return conn.LocalAddr().(*net.UDPAddr).AddrPort() }

	f := &burstFixture{relayed: relayed, datagram: make([]byte, 0, relay.MaxHeaderLen+receiveMTU)}
	cfg := Config{}
	if relayed {
		leg := listen()
		f.senders, f.sink = []*net.UDPConn{leg}, leg
		cfg.Relay = &RelayConfig{
			Addr: addrOf(leg), PublicAddr: netip.MustParseAddrPort("127.0.0.1:9"),
			Owners: sessionstore.NewMemory(), LeaseRenewInterval: time.Hour, // sessions here hold no lease
		}
	} else {
		f.sink = listen()
	}
	// The echoes of a whole burst must fit, or the sink, not the worker,
	// drops. Like the worker's, this request is best effort.
	f.sinkBuf = udpbuffer.Apply(f.sink, udpbuffer.Worker)
	w, err := New(cfg)
	require.NoError(tb, err)
	tb.Cleanup(func() { _ = w.Close() })
	f.w, f.target = w, w.LocalAddr()

	offer, err := parseOffer(testOffer(testOfferAttrs{}))
	require.NoError(tb, err)
	profile := srtp.ProtectionProfileAeadAes128Gcm
	for i := range callers {
		s, err := newSession(w, offer)
		require.NoError(tb, err)
		caller := &burstCaller{addr: netip.AddrPortFrom(netip.AddrFrom4([4]byte{192, 0, 2, byte(i + 1)}), 40000)}
		if !relayed {
			conn := listen()
			f.senders = append(f.senders, conn)
			caller.addr = addrOf(conn)
		}
		key, salt := make([]byte, 16), make([]byte, 12)
		key[0], salt[0] = byte(i), byte(i)
		s.srtpIn, err = srtp.CreateContext(key, salt, profile, srtp.SRTPReplayProtection(replayWindow))
		require.NoError(tb, err)
		s.srtpOut, err = srtp.CreateContext(key, salt, profile)
		require.NoError(tb, err)
		caller.ctx, err = srtp.CreateContext(key, salt, profile)
		require.NoError(tb, err)
		s.mu.Lock()
		s.consent.Stop() // a benchmark can outlast the consent timeout
		s.state.ICE.RemoteAddr = caller.addr
		if !relayed {
			s.state.ICE.RemoteAddr = addrOf(f.sink)
		}
		s.mu.Unlock()
		w.mu.Lock()
		w.sessions[s.id] = s
		w.byAddr[caller.addr] = s
		w.mu.Unlock()
		f.callers = append(f.callers, caller)
	}
	return f
}

// packets encrypts the next burst outside any timing: size video packets,
// round-robin across the callers, as in a keyframe burst from many calls.
func (f *burstFixture) packets(tb testing.TB, size int) [][]byte {
	tb.Helper()
	payload := make([]byte, burstPayload)
	out := make([][]byte, size)
	for i := range out {
		c := f.callers[i%len(f.callers)]
		c.seq++
		raw, err := (&rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 96, SSRC: 789, SequenceNumber: c.seq, Timestamp: c.ts}, Payload: payload}).Marshal()
		require.NoError(tb, err)
		out[i], err = c.ctx.EncryptRTP(nil, raw, nil)
		require.NoError(tb, err)
	}
	for _, c := range f.callers {
		c.ts += 3000
	}
	return out
}

func (f *burstFixture) send(tb testing.TB, i int, pkt []byte) {
	tb.Helper()
	c := f.callers[i%len(f.callers)]
	var err error
	if f.relayed {
		f.datagram = append(relay.AppendHeader(f.datagram[:0], c.addr), pkt...)
		_, err = f.senders[0].WriteToUDPAddrPort(f.datagram, f.target)
	} else {
		_, err = f.senders[i%len(f.senders)].WriteToUDPAddrPort(pkt, f.target)
	}
	require.NoError(tb, err)
}

// burstRun is one burst: Drain runs from the start (the release of a
// queued burst, or the first send) to the arrival of the last echo.
type burstRun struct {
	Drain, Send time.Duration
	Echoes      int
}

// skipUnlessRoom skips a benchmark whose burst might not fit in the
// worker's or the sink's effective receive buffer: the host may clamp the
// request silently, and a dropped packet would fail the burst at its
// deadline instead of measuring it. Zero is an unknown size.
func (f *burstFixture) skipUnlessRoom(b *testing.B, size int) {
	b.Helper()
	need := size * burstDatagramCharge
	worker, sink := f.w.cfg.socketBuffers.Effective.Read, f.sinkBuf.Effective.Read
	if worker < need || sink < need {
		b.Skipf("burst of %d may need %d bytes of receive buffer; the worker socket has %d and the sink %d (0 is unknown). Raise net.core.rmem_max (Linux) or kern.ipc.maxsockbuf (macOS) to measure it",
			size, need, worker, sink)
	}
}

// run sends one burst and waits for its echoes. When queued, the read loop
// is held on the worker lock while the whole burst lands in the socket's
// receive buffer, so Drain is the loop's own drain time; otherwise the loop
// races the sender.
func (f *burstFixture) run(tb testing.TB, size int, queued bool) burstRun {
	tb.Helper()
	pkts := f.packets(tb, size)
	type result struct {
		n    int
		last time.Time
	}
	echoes := make(chan result, 1)
	go func() {
		buf := make([]byte, receiveMTU+relay.MaxHeaderLen)
		_ = f.sink.SetReadDeadline(time.Now().Add(10 * time.Second))
		n := 0
		for n < size {
			if _, _, err := f.sink.ReadFromUDPAddrPort(buf); err != nil {
				break
			}
			n++
		}
		echoes <- result{n, time.Now()}
	}()

	if queued {
		f.w.mu.Lock() // the loop reads at most one packet, then waits in sessionAt
	}
	start := time.Now()
	for i, pkt := range pkts {
		f.send(tb, i, pkt)
	}
	sent := time.Now()
	if queued {
		start = sent
		f.w.mu.Unlock()
	}
	r := <-echoes
	return burstRun{Drain: r.last.Sub(start), Send: sent.Sub(start), Echoes: r.n}
}

// TestReadLoopBurstDrain checks the fixture itself: every packet of a small
// queued burst comes back, on both transports. Timing is not asserted.
func TestReadLoopBurstDrain(t *testing.T) {
	for _, relayed := range []bool{false, true} {
		t.Run(fmt.Sprintf("relayed=%t", relayed), func(t *testing.T) {
			f := newBurstFixture(t, relayed, 4)
			require.Equal(t, unitBurst, f.run(t, unitBurst, true).Echoes)
		})
	}
}

// BenchmarkReadLoopBurst reports how long one worker read loop takes to
// drain a fixed burst of 28 callers' keyframe-sized SRTP video, as a median
// over the iterations. drain-ms is the median burst drain, us/pkt that over
// the burst size, and p90-drain-ms the 90th percentile. "queued" holds the
// loop while the whole burst lands in the receive buffer; "open" lets it race
// the sender. Run with a fixed count, for example:
//
//	go test -run '^$' -bench BenchmarkReadLoopBurst -benchtime 40x -count 5 ./pkg/mediaworker/
func BenchmarkReadLoopBurst(b *testing.B) {
	for _, relayed := range []bool{true, false} {
		for _, queued := range []bool{true, false} {
			for _, size := range []int{64, 256, 1024} {
				transport, mode := "direct", "open"
				if relayed {
					transport = "relay"
				}
				if queued {
					mode = "queued"
				}
				b.Run(fmt.Sprintf("%s/%s/burst=%d", transport, mode, size), func(b *testing.B) {
					f := newBurstFixture(b, relayed, 28)
					f.skipUnlessRoom(b, size)
					f.run(b, size, queued) // warm up: anchor the tracks and SRTP contexts
					drains := make([]time.Duration, 0, b.N)
					b.ResetTimer()
					for range b.N {
						run := f.run(b, size, queued)
						if run.Echoes != size {
							b.Fatalf("burst of %d: %d echoes", size, run.Echoes)
						}
						drains = append(drains, run.Drain)
					}
					b.StopTimer()
					slices.Sort(drains)
					median := drains[len(drains)/2]
					b.ReportMetric(float64(median.Nanoseconds())/1e6, "drain-ms")
					b.ReportMetric(float64(drains[len(drains)*9/10].Nanoseconds())/1e6, "p90-drain-ms")
					b.ReportMetric(float64(median.Nanoseconds())/1e3/float64(size), "us/pkt")
				})
			}
		}
	}
}

func histogramOf(t *testing.T, h *burstHistogram) *dto.Histogram {
	t.Helper()
	ch := make(chan prometheus.Metric, 1)
	h.Collect(ch)
	var m dto.Metric
	require.NoError(t, (<-ch).Write(&m))
	return m.GetHistogram()
}

// Bucket bounds are inclusive, as Prometheus defines them, and every
// observation is counted once in the cumulative buckets.
func TestBurstHistogramBuckets(t *testing.T) {
	var h burstHistogram
	h.observe(50 * time.Microsecond) // on the first bound
	h.observe(51 * time.Microsecond)
	h.observe(time.Second) // over the last bound: +Inf only
	got := histogramOf(t, &h)
	require.EqualValues(t, 3, got.GetSampleCount())
	require.InDelta(t, (50*time.Microsecond + 51*time.Microsecond + time.Second).Seconds(), got.GetSampleSum(), 1e-12)
	cumulative := map[float64]uint64{}
	for _, b := range got.GetBucket() {
		cumulative[b.GetUpperBound()] = b.GetCumulativeCount()
	}
	require.EqualValues(t, 1, cumulative[0.00005])
	require.EqualValues(t, 2, cumulative[0.0001])
	require.EqualValues(t, 2, cumulative[0.25])
}

// A burst runs while the loop waits at most readBurstGap for each next
// packet, and is recorded when a longer wait ends it.
func TestReadBurstsObserveBusyPeriods(t *testing.T) {
	us := func(n int) time.Duration { return time.Duration(n) * time.Microsecond }
	t0 := time.Unix(1000, 0)
	bursts := readBursts{drain: &burstHistogram{}}
	packet := func(read, done time.Duration) {
		bursts.read(t0.Add(read))
		bursts.processed(t0.Add(done))
	}

	packet(0, us(10))
	packet(us(20), us(30))               // a 10 µs wait
	packet(us(30)+readBurstGap, us(140)) // a wait of exactly the gap: same burst
	require.Zero(t, histogramOf(t, bursts.drain).GetSampleCount(), "a burst is recorded only once it ends")
	lone := us(140) + readBurstGap + 1
	packet(lone, us(250)) // just over the gap: ends the 140 µs burst
	h := histogramOf(t, bursts.drain)
	require.EqualValues(t, 1, h.GetSampleCount())
	require.InDelta(t, us(140).Seconds(), h.GetSampleSum(), 1e-12)

	packet(us(5000), us(5010)) // the lone packet before it was a burst of its own
	h = histogramOf(t, bursts.drain)
	require.EqualValues(t, 2, h.GetSampleCount())
	require.InDelta(t, (us(140) + us(250) - lone).Seconds(), h.GetSampleSum(), 1e-12)

	// When the loop stops, the open burst (the 5000 µs packet) is recorded.
	bursts.end()
	h = histogramOf(t, bursts.drain)
	require.EqualValues(t, 3, h.GetSampleCount())
	require.InDelta(t, (us(140) + us(250) - lone + us(10)).Seconds(), h.GetSampleSum(), 1e-12)
}

// A busy period is not a backlog: packets 90 µs apart, each finished in
// 10 µs, never queue, yet they form one period that includes the waits.
func TestReadBurstsCountShortWaitsAsBusy(t *testing.T) {
	us := func(n int) time.Duration { return time.Duration(n) * time.Microsecond }
	t0 := time.Unix(1000, 0)
	bursts := readBursts{drain: &burstHistogram{}}
	for i := range 10 {
		bursts.read(t0.Add(us(90 * i)))
		bursts.processed(t0.Add(us(90*i + 10)))
	}
	bursts.end()
	h := histogramOf(t, bursts.drain)
	require.EqualValues(t, 1, h.GetSampleCount())
	require.InDelta(t, us(820).Seconds(), h.GetSampleSum(), 1e-12)
}

// A loop that read no packet records nothing when it stops.
func TestReadBurstsEndWithoutPackets(t *testing.T) {
	bursts := readBursts{drain: &burstHistogram{}}
	bursts.end()
	require.Zero(t, histogramOf(t, bursts.drain).GetSampleCount())
}

// The worker's scrape exposes the histogram and records the read loop's
// bursts, also after Close. Durations and burst boundaries are proven with
// controlled clocks in TestReadBurstsObserveBusyPeriods, not here, where the
// scheduler decides.
func TestWorkerMetricsReadBurstDrain(t *testing.T) {
	f := newBurstFixture(t, true, 4)
	require.Equal(t, unitBurst, f.run(t, unitBurst, true).Echoes)
	// Close waits for the read loop to exit, which records the open burst.
	require.NoError(t, f.w.Close())

	var parser expfmt.TextParser
	families, err := parser.TextToMetricFamilies(strings.NewReader(metricsBody(f.w)))
	require.NoError(t, err)
	family := families["relais_worker_read_burst_drain_seconds"]
	require.NotNil(t, family)
	require.Equal(t, dto.MetricType_HISTOGRAM, family.GetType())
	require.Positive(t, family.Metric[0].GetHistogram().GetSampleCount())
}

// BenchmarkReadBursts is the metric's cost per packet in the read loop: two
// clock reads, and inside a burst nothing else; at worst, when every packet
// is a burst of its own, also one histogram observation.
func BenchmarkReadBursts(b *testing.B) {
	b.Run("in-burst", func(b *testing.B) {
		bursts := readBursts{drain: &burstHistogram{}}
		b.ReportAllocs()
		for range b.N {
			bursts.read(time.Now())
			bursts.processed(time.Now())
		}
	})
	b.Run("burst-per-packet", func(b *testing.B) {
		bursts := readBursts{drain: &burstHistogram{}}
		b.ReportAllocs()
		for range b.N {
			bursts.read(time.Now())
			bursts.processed(time.Now())
			bursts.done = bursts.done.Add(-time.Second) // as if the loop then waited
		}
	})
}
