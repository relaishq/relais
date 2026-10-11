package callharness

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"
)

func TestSendTimingObservesRealMediaPackets(t *testing.T) {
	system, err := Start(Options{Relay: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = system.Close() }()
	factoryCalls := 0
	h, err := Start(Options{External: &ExternalTopology{SignalingURL: system.SignalingURL(), RelayAddr: system.RelayAddr(), CallerSocket: func() (net.PacketConn, error) { factoryCalls++; return net.ListenPacket("udp4", "127.0.0.1:0") }}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.Close() }()
	ctx, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	var mu sync.Mutex
	counts := map[string]int{}
	var bad bool
	c, err := h.Dial(ctx, CallOptions{Video: true, Worker: -1, SendTiming: func(kind string, scheduled, actual time.Time) {
		mu.Lock()
		defer mu.Unlock()
		counts[kind]++
		if scheduled.IsZero() || actual.Before(scheduled) {
			bad = true
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SendMedia(ctx, 300*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Hangup(ctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if counts[kindAudio] < 10 || counts[kindVideo] < 5 || bad || factoryCalls != 1 {
		t.Fatalf("counts=%v bad=%t", counts, bad)
	}
}
func TestSendTimingExcludesControlPackets(t *testing.T) {
	n := 0
	o := &stunObserver{sendTiming: func(string, time.Time, time.Time) { n++ }}
	at := time.Now()
	o.audioSchedule.Store(&at)
	o.videoSchedule.Store(&at)
	for _, packet := range [][]byte{{}, make([]byte, 12), {0x80, 200, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}, {0x80, 127, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}} {
		o.observeMediaSend(packet, at)
	}
	if n != 0 {
		t.Fatal(n)
	}
	o.observeMediaSend([]byte{0x80, 111, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}, at)
	if n != 1 {
		t.Fatal(n)
	}
}
