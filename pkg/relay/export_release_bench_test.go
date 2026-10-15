package relay

import (
	"testing"
	"time"
)

// BenchmarkPlannedRelease measures the forwardMu work of one release with a
// typical retained ring (1.5 s of 50 pps audio and ~30 pps video) and ten
// held packets: the plain release, and the ADR 0009 release that also scans
// the ring, filters it against the export and orders the result.
func BenchmarkPlannedRelease(b *testing.B) {
	for _, exported := range []bool{false, true} {
		b.Run(map[bool]string{false: "plain", true: "after-export"}[exported], func(b *testing.B) {
			t := &testing.T{}
			sys := startTestRelay(t, Config{Buffer: &BufferConfig{}, HoldTimeout: time.Hour})
			defer func() { _ = sys.relay.Close() }()
			a, target := sys.worker(t, sessionA), sys.worker(t, sessionB)
			caller := newTestCaller(t)
			sys.connect(t, caller, a, sessionA)
			now := time.Now()
			var held []heldPacket
			sys.relay.forwardMu.Lock()
			for i := range 75 {
				sys.relay.buffer.add(sessionA, caller.addr(), bufferRTP(t, 42, uint16(1000+i)), now)
			}
			for i := range 45 {
				sys.relay.buffer.add(sessionA, caller.addr(), bufferRTP(t, 43, uint16(5000+i)), now)
			}
			for i := range 10 {
				packet := bufferRTP(t, 42, uint16(1075+i))
				datagram := make([]byte, MaxHeaderLen+len(packet))
				copy(datagram[MaxHeaderLen:], packet)
				held = append(held, heldPacket{caller: caller.addr(), datagram: datagram})
			}
			sys.relay.forwardMu.Unlock()
			input := map[uint32]ConsumedInput{42: {High: 1074, Window: ^uint64(0) &^ (1 << 3), Exact: true}, 43: {High: 5044, Window: ^uint64(0), Exact: true}}
			b.ResetTimer()
			for range b.N {
				sys.relay.forwardMu.Lock()
				h := &sessionHold{id: sessionA, source: a.addr(), worker: target.addr(), acknowledged: true, ready: make(chan struct{}), released: make(chan struct{}), timer: time.AfterFunc(time.Hour, func() {})}
				h.queue = append([]heldPacket(nil), held...)
				sys.relay.holds[sessionA] = h
				sys.relay.forwardMu.Unlock()
				if exported {
					if _, err := sys.relay.ReleaseAfterExport(sessionA, target.addr(), input); err != nil {
						b.Fatal(err)
					}
				} else if _, err := sys.relay.ReleaseSession(sessionA, target.addr()); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
		})
	}
}
