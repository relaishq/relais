package relay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/relais/internal/privateapi"
	"github.com/relais/internal/relayleg"
	"github.com/stretchr/testify/require"
)

// exportPacket is one caller RTP packet in an ADR 0009 scenario.
type exportPacket struct {
	ssrc uint32
	seq  uint16
}

func (p exportPacket) raw(t *testing.T) []byte { return bufferRTP(t, p.ssrc, p.seq) }

// exportScenario sends pre-marker input to A (A's test socket stands in for
// a worker that consumed exactly what input says), acknowledges the drain
// marker, queues held input, moves the route to B and releases after export.
type exportScenario struct {
	cfg     Config
	restart bool // relay restarts before the call's media: a cold ring
	pre     []exportPacket
	held    []exportPacket
	input   map[uint32]ConsumedInput
	settle  time.Duration
}

func runExportScenario(t *testing.T, sc exportScenario) (ReleaseReply, []exportPacket, *testSystem) {
	t.Helper()
	if sc.cfg.HoldTimeout == 0 {
		sc.cfg.HoldTimeout = time.Hour
	}
	sys := startTestRelay(t, sc.cfg)
	a, b := sys.worker(t, sessionA), sys.worker(t, sessionB)
	caller := newTestCaller(t)
	if sc.restart {
		sys.restart(t)
	}
	sys.connect(t, caller, a, sessionA)
	for _, p := range sc.pre {
		caller.send(t, p.raw(t), sys.relay.PublicAddr())
		a.expect(t, caller.addr(), p.raw(t))
	}
	beginHold(t, sys, a, sessionA)
	for _, p := range sc.held {
		caller.send(t, p.raw(t), sys.relay.PublicAddr())
	}
	require.Eventually(t, func() bool { return sys.relay.Stats().HeldPackets == len(sc.held) }, receiveTimeout, time.Millisecond)
	time.Sleep(sc.settle)
	transferTestLease(t, sys.cfg.Owners, sessionA, b.addr())
	require.NoError(t, sys.relay.MoveSession(sessionA, a.addr(), b.addr()))
	reply, err := sys.relay.ReleaseAfterExport(sessionA, b.addr(), sc.input)
	require.NoError(t, err)
	var got []exportPacket
	for {
		datagram, ok := receive(t, b.conn, quietPeriod)
		if !ok {
			break
		}
		_, payload, err := ParseHeader(datagram)
		require.NoError(t, err)
		var header rtp.Header
		_, err = header.Unmarshal(payload)
		require.NoError(t, err)
		got = append(got, exportPacket{header.SSRC, header.SequenceNumber})
	}
	a.expectNothing(t)
	require.Zero(t, sys.relay.Stats().HeldPackets)
	require.Zero(t, sys.relay.Stats().HeldBytes)
	return reply, got, sys
}

func seqs(ssrc uint32, from, to uint16) []exportPacket {
	var out []exportPacket
	for seq := from; ; seq++ {
		out = append(out, exportPacket{ssrc, seq})
		if seq == to {
			return out
		}
	}
}

// window marks consumed indexes below high: bit i is high-i.
func window(high uint64, consumed ...uint64) uint64 {
	var w uint64
	for _, index := range consumed {
		w |= 1 << (high - index)
	}
	return w
}

func TestReleaseAfterExportRecoversUnconsumedInput(t *testing.T) {
	buffer := Config{Buffer: &BufferConfig{}}
	for _, tc := range []struct {
		name      string
		sc        exportScenario
		want      []exportPacket
		recovered int
		filtered  int
		complete  bool
	}{
		{
			name:      "tail: delayed behind the marker, above the export",
			sc:        exportScenario{pre: seqs(42, 10, 13), held: seqs(42, 14, 15), input: map[uint32]ConsumedInput{42: {High: 12, Window: window(12, 10, 11, 12), Exact: true}}},
			want:      seqs(42, 13, 15),
			recovered: 1, complete: true,
		},
		{
			name:      "gap: overtaken by later pre-marker input",
			sc:        exportScenario{pre: seqs(42, 10, 13), held: seqs(42, 14, 14), input: map[uint32]ConsumedInput{42: {High: 13, Window: window(13, 10, 11, 13), Exact: true}}},
			want:      []exportPacket{{42, 12}, {42, 14}},
			recovered: 1, complete: true,
		},
		{
			name: "sparse windows on independent audio and video SSRCs",
			sc: exportScenario{
				pre:   []exportPacket{{42, 10}, {43, 100}, {42, 11}, {43, 101}, {42, 12}, {43, 102}, {42, 13}, {43, 103}, {42, 14}, {42, 15}},
				held:  []exportPacket{{43, 104}, {42, 16}},
				input: map[uint32]ConsumedInput{42: {High: 15, Window: window(15, 10, 12, 15), Exact: true}, 43: {High: 103, Window: window(103, 100, 101, 103), Exact: true}},
			},
			want:      []exportPacket{{42, 11}, {43, 102}, {42, 13}, {42, 14}, {43, 104}, {42, 16}},
			recovered: 4, complete: true,
		},
		{
			name: "H-63 is recovered, H-64 is never sent",
			sc: exportScenario{pre: seqs(42, 100, 170), input: map[uint32]ConsumedInput{42: {High: 170, Exact: true, Window: func() uint64 {
				w := ^uint64(0)
				return w &^ (1 << 63)
			}()}}},
			want:      []exportPacket{{42, 107}},
			recovered: 1, complete: true,
		},
		{
			name:      "index 0 and a high water mark below the window",
			sc:        exportScenario{pre: seqs(42, 0, 1), input: map[uint32]ConsumedInput{42: {High: 1, Window: window(1, 1), Exact: true}}},
			want:      []exportPacket{{42, 0}},
			recovered: 1, complete: true,
		},
		{
			name:      "an SSRC the export never authenticated",
			sc:        exportScenario{pre: append(seqs(42, 10, 11), seqs(45, 7, 8)...), input: map[uint32]ConsumedInput{42: {High: 11, Window: window(11, 10, 11), Exact: true}}},
			want:      seqs(45, 7, 8),
			recovered: 2, complete: true,
		},
		{
			name:     "held duplicates of consumed input are filtered",
			sc:       exportScenario{pre: seqs(42, 10, 12), held: []exportPacket{{42, 12}, {42, 13}, {42, 13}}, input: map[uint32]ConsumedInput{42: {High: 12, Window: window(12, 10, 11, 12), Exact: true}}},
			want:     seqs(42, 13, 13),
			filtered: 2, complete: true,
		},
		{
			name:      "inexact export: only input above it is recovered, held input is kept",
			sc:        exportScenario{pre: seqs(42, 10, 13), held: []exportPacket{{42, 11}, {42, 14}}, input: map[uint32]ConsumedInput{42: {High: 12}}},
			want:      []exportPacket{{42, 11}, {42, 13}, {42, 14}},
			recovered: 1, complete: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.sc.cfg = buffer
			reply, got, sys := runExportScenario(t, tc.sc)
			require.Equal(t, tc.want, got)
			require.Equal(t, len(tc.sc.held), reply.Packets)
			require.Equal(t, tc.recovered, reply.Recovered)
			require.Equal(t, tc.filtered, reply.Filtered)
			require.Equal(t, tc.complete, reply.Complete)
			stats := sys.relay.Stats()
			require.EqualValues(t, 1, stats.PlannedReleases)
			require.EqualValues(t, tc.recovered, stats.PlannedRecovered)
			require.EqualValues(t, tc.filtered, stats.PlannedFiltered)
			require.Positive(t, stats.PlannedReleaseLockMax)
		})
	}
}

// Without a caller buffer nothing forwarded is retained: only held input
// is sent, still filtered against an exact export window (ADR 0009).
func TestReleaseAfterExportWithoutBuffer(t *testing.T) {
	reply, got, sys := runExportScenario(t, exportScenario{pre: seqs(42, 10, 13), held: []exportPacket{{42, 12}, {42, 14}},
		input: map[uint32]ConsumedInput{42: {High: 12, Window: window(12, 10, 11, 12), Exact: true}}})
	require.Equal(t, []exportPacket{{42, 14}}, got, "13 reached A after its export and is lost without a buffer")
	require.Equal(t, ReleaseReply{Packets: 2, Filtered: 1, LockHeld: reply.LockHeld}, reply)
	require.EqualValues(t, 1, sys.relay.Stats().PlannedIncomplete)
}

// Eviction can remove unconsumed input before the release; it is reported.
func TestReleaseAfterExportReportsEviction(t *testing.T) {
	reply, got, _ := runExportScenario(t, exportScenario{cfg: Config{Buffer: &BufferConfig{Window: 20 * time.Millisecond}}, settle: 60 * time.Millisecond,
		pre: seqs(42, 10, 13), input: map[uint32]ConsumedInput{42: {High: 13, Window: window(13, 10, 11, 13), Exact: true}}})
	require.Empty(t, got)
	require.Zero(t, reply.Recovered)
	require.False(t, reply.Complete)
}

// A relay restarted mid-call starts its ROC at zero. Reordered input around
// a sequence wrap cannot be aligned with a single offset; each retained index
// is rebuilt around the worker's anchor, for planned release and crash replay.
func TestColdRingRolloverAlignment(t *testing.T) {
	for _, tc := range []struct {
		name   string
		pre    []exportPacket
		high   uint64
		window uint64
		want   []exportPacket
	}{
		// Ring [0, 65534]: seq 0 is index 65536 and must not look too old.
		{name: "seq 0 then 65534 at H=65535", pre: []exportPacket{{42, 0}, {42, 65534}}, high: 65535, window: window(65535, 65535, 65534), want: []exportPacket{{42, 0}}},
		// Ring [1, 65535]: 65535 is unconsumed below H, seq 1 is above it.
		{name: "seq 1 then 65535 at H=65536", pre: []exportPacket{{42, 1}, {42, 65535}}, high: 65536, window: window(65536, 65536), want: []exportPacket{{42, 65535}, {42, 1}}},
	} {
		t.Run("planned/"+tc.name, func(t *testing.T) {
			reply, got, _ := runExportScenario(t, exportScenario{cfg: Config{Buffer: &BufferConfig{}}, restart: true, pre: tc.pre,
				input: map[uint32]ConsumedInput{42: {High: tc.high, Window: tc.window, Exact: true}}})
			require.Equal(t, tc.want, got)
			require.Equal(t, len(tc.want), reply.Recovered)
		})
		t.Run("crash/"+tc.name, func(t *testing.T) {
			sys := startTestRelay(t, Config{Buffer: &BufferConfig{}, HoldTimeout: time.Hour})
			a, b := sys.worker(t, sessionA), sys.worker(t, sessionB)
			caller := newTestCaller(t)
			sys.restart(t)
			sys.connect(t, caller, a, sessionA)
			for _, p := range tc.pre {
				caller.send(t, p.raw(t), sys.relay.PublicAddr())
				a.expect(t, caller.addr(), p.raw(t))
			}
			_, err := sys.relay.BeginReplay(context.Background(), sessionA, a.addr(), nil)
			require.NoError(t, err)
			transferTestLease(t, sys.cfg.Owners, sessionA, b.addr())
			require.NoError(t, sys.relay.MoveSession(sessionA, a.addr(), b.addr()))
			// Crash replay sends every retained index above the checkpoint.
			_, err = sys.relay.BeginReplay(context.Background(), sessionA, a.addr(), map[uint32]uint64{42: tc.high})
			require.NoError(t, err)
			result, err := sys.relay.ReplaySession(context.Background(), sessionA, b.addr())
			require.NoError(t, err)
			var above []exportPacket
			for _, p := range tc.want {
				if index := inboundIndex(tc.high, p.seq); index > tc.high {
					above = append(above, p)
				}
			}
			require.Equal(t, len(above), result.Packets)
			for _, p := range above {
				b.expect(t, caller.addr(), p.raw(t))
			}
			b.expectNothing(t)
		})
	}
}

func TestReleaseAfterExportEdges(t *testing.T) {
	t.Run("expired hold", func(t *testing.T) {
		sys := startTestRelay(t, Config{Buffer: &BufferConfig{}})
		_, err := sys.relay.ReleaseAfterExport(sessionA, netip.MustParseAddrPort("127.0.0.1:9"), nil)
		require.ErrorIs(t, err, ErrHoldExpired)
	})
	t.Run("index above 48 bits", func(t *testing.T) {
		sys := startTestRelay(t, Config{Buffer: &BufferConfig{}})
		_, err := sys.relay.ReleaseAfterExport(sessionA, netip.MustParseAddrPort("127.0.0.1:9"), map[uint32]ConsumedInput{1: {High: 1 << 48}})
		require.Error(t, err)
	})
}

// The private HTTP endpoint carries the export input and the reply.
func TestReleaseAfterExportHTTP(t *testing.T) {
	sys := startTestRelay(t, Config{Buffer: &BufferConfig{}, HoldTimeout: time.Hour})
	a, b := sys.worker(t, sessionA), sys.worker(t, sessionB)
	caller := newTestCaller(t)
	sys.connect(t, caller, a, sessionA)
	for _, p := range seqs(42, 10, 13) {
		caller.send(t, p.raw(t), sys.relay.PublicAddr())
		a.expect(t, caller.addr(), p.raw(t))
	}
	beginHold(t, sys, a, sessionA)
	transferTestLease(t, sys.cfg.Owners, sessionA, b.addr())
	require.NoError(t, sys.relay.MoveSession(sessionA, a.addr(), b.addr()))
	server := httptest.NewServer(sys.relay.PrivateHandler())
	defer server.Close()
	var reply ReleaseReply
	err := privateapi.Do(context.Background(), server.Client(), server.URL, http.MethodPost, "/sessions/"+sessionA+"/release-after-export",
		ExportReleaseRequest{To: b.addr(), Input: map[uint32]ConsumedInput{42: {High: 12, Window: window(12, 10, 11, 12), Exact: true}}}, &reply, RemoteErrors)
	require.NoError(t, err)
	require.Equal(t, 1, reply.Recovered)
	require.True(t, reply.Complete)
	b.expect(t, caller.addr(), bufferRTP(t, 42, 13))
	err = privateapi.Do(context.Background(), server.Client(), server.URL, http.MethodPost, "/sessions/"+sessionA+"/release-after-export",
		ExportReleaseRequest{To: b.addr()}, &reply, RemoteErrors)
	require.ErrorIs(t, err, ErrHoldExpired, "the hold is released once; a retry sends nothing")
	b.expectNothing(t)
}

// A datagram reordered behind the drain marker can reach the source before
// its export fence. Its echo follows the first acknowledgement on the worker
// leg; the post-export marker makes the relay forward that output before the
// route moves, rather than dropping it as unrouted (ADR 0009).
func TestMoveSessionAfterExportForwardsPreFenceOutput(t *testing.T) {
	sys := startTestRelay(t, Config{HoldTimeout: time.Hour})
	a, b := sys.worker(t, sessionA), sys.worker(t, sessionB)
	caller := newTestCaller(t)
	sys.connect(t, caller, a, sessionA)
	beginHold(t, sys, a, sessionA)
	transferTestLease(t, sys.cfg.Owners, sessionA, b.addr())
	done := make(chan error, 1)
	go func() { done <- sys.relay.MoveSessionAfterExport(sessionA, a.addr(), b.addr(), 0) }()
	datagram, ok := receive(t, a.conn, receiveTimeout)
	require.True(t, ok)
	nonce, ack, valid := relayleg.ParseBarrier(datagram)
	require.True(t, valid)
	require.False(t, ack)
	select {
	case err := <-done:
		t.Fatalf("route moved before the source drained its output: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	echo := []byte{0x80, 0x6f, 0, 7}
	a.sendTo(t, caller.addr(), echo, sys.relay.WorkerAddr())
	_, err := a.conn.WriteToUDPAddrPort(relayleg.Barrier(nonce, true), sys.relay.WorkerAddr())
	require.NoError(t, err)
	require.NoError(t, <-done)
	caller.expect(t, sys.relay.PublicAddr(), echo)
	stats := sys.relay.Stats()
	require.Zero(t, stats.WorkerNoFlow)
	require.EqualValues(t, 1, stats.OutputDrains)
	// The route has moved: the old owner's output is fenced again.
	a.sendTo(t, caller.addr(), echo, sys.relay.WorkerAddr())
	caller.expectNothing(t)
	require.EqualValues(t, 1, sys.relay.Stats().WorkerNoFlow)
}

func TestMoveSessionAfterExportBounds(t *testing.T) {
	t.Run("a silent source costs at most the drain timeout", func(t *testing.T) {
		sys := startTestRelay(t, Config{HoldTimeout: time.Hour, OutputDrainTimeout: 60 * time.Millisecond})
		a, b := sys.worker(t, sessionA), sys.worker(t, sessionB)
		caller := newTestCaller(t)
		sys.connect(t, caller, a, sessionA)
		beginHold(t, sys, a, sessionA)
		started := time.Now()
		require.NoError(t, sys.relay.MoveSessionAfterExport(sessionA, a.addr(), b.addr(), 0))
		require.GreaterOrEqual(t, time.Since(started), 60*time.Millisecond)
		require.Less(t, time.Since(started), time.Second)
		require.EqualValues(t, 1, sys.relay.Stats().OutputDrainTimeouts)
		var markers int
		for {
			datagram, ok := receive(t, a.conn, quietPeriod)
			if !ok {
				break
			}
			_, ack, valid := relayleg.ParseBarrier(datagram)
			require.True(t, valid)
			require.False(t, ack)
			markers++
		}
		require.GreaterOrEqual(t, markers, 2, "a lost marker is resent")
	})
	t.Run("without a planned hold it is MoveSessionAtEpoch", func(t *testing.T) {
		sys := startTestRelay(t, Config{})
		a, b := sys.worker(t, sessionA), sys.worker(t, sessionB)
		caller := newTestCaller(t)
		sys.connect(t, caller, a, sessionA)
		require.NoError(t, sys.relay.MoveSessionAfterExport(sessionA, a.addr(), b.addr(), 0))
		a.expectNothing(t)
		require.Zero(t, sys.relay.Stats().OutputDrains+sys.relay.Stats().OutputDrainTimeouts)
	})
	t.Run("private HTTP route", func(t *testing.T) {
		sys := startTestRelay(t, Config{})
		a, b := sys.worker(t, sessionA), sys.worker(t, sessionB)
		caller := newTestCaller(t)
		sys.connect(t, caller, a, sessionA)
		server := httptest.NewServer(sys.relay.PrivateHandler())
		defer server.Close()
		require.NoError(t, privateapi.Do(context.Background(), server.Client(), server.URL, http.MethodPost, "/sessions/"+sessionA+"/move-after-export",
			RouteRequest{From: a.addr(), To: b.addr()}, nil, RemoteErrors))
		caller.send(t, bufferRTP(t, 42, 1), sys.relay.PublicAddr())
		b.expect(t, caller.addr(), bufferRTP(t, 42, 1))
	})
}
