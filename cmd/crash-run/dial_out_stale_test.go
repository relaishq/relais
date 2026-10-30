//go:build !windows

package main

import (
	"context"
	"encoding/json"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"net/http"

	"github.com/pion/rtp"
	"github.com/relaishq/relais/internal/privateapi"
	"github.com/relaishq/relais/internal/redistestenv"
	"github.com/relaishq/relais/internal/workerlink"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/relay"
	"github.com/stretchr/testify/require"
)

// Observe before Pion decrypts or drops replayed SRTP, as in ADR 0008's
// TestPartitionedWorkerStoreTransferHasExclusiveCallerMedia and the paused
// worker fixture. RTP headers remain visible in ciphertext.
type processRawSocket struct {
	net.PacketConn
	mu      sync.Mutex
	seen    map[string]int
	headers []rtp.Header
}

func (s *processRawSocket) ReadFrom(b []byte) (int, net.Addr, error) {
	n, addr, err := s.PacketConn.ReadFrom(b)
	if err == nil {
		s.mu.Lock()
		s.seen[string(b[:n])]++
		var h rtp.Header
		if n >= 12 && b[0]>>6 == 2 && (b[1] < 192 || b[1] > 223) && validRTPHeader(&h, b[:n]) {
			s.headers = append(s.headers, h)
		}
		s.mu.Unlock()
	}
	return n, addr, err
}

func validRTPHeader(h *rtp.Header, b []byte) bool { _, err := h.Unmarshal(b); return err == nil }

func processCallerSocket(t *testing.T) *processRawSocket {
	t.Helper()
	c, err := net.ListenPacket("udp4", "127.0.0.1:0")
	require.NoError(t, err)
	return &processRawSocket{PacketConn: c, seen: make(map[string]int)}
}

// A transparent leg socket keeps the registered source address while the real
// worker store and command paths are partitioned. Withhold unique authentic
// audio packets (the inbound fixture also injects audio), then transmit those same
// ciphertext bytes after fencing; an SRTP rejection cannot hide a bad route.
type processStaleLeg struct {
	conn      *net.UDPConn
	relay     netip.AddrPort
	mu        sync.Mutex
	worker    netip.AddrPort
	withhold  bool
	packets   [][]byte
	forwarded []byte
	done      chan struct{}
}

func newProcessStaleLeg(t *testing.T, target string) *processStaleLeg {
	t.Helper()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	p := &processStaleLeg{conn: c, relay: netip.MustParseAddrPort(target), done: make(chan struct{})}
	go func() {
		defer close(p.done)
		b := make([]byte, 65536)
		for {
			n, from, err := c.ReadFromUDPAddrPort(b)
			if err != nil {
				return
			}
			p.mu.Lock()
			to := p.relay
			drop := false
			if from == p.relay {
				to = p.worker
			} else {
				p.worker = from
				_, payload, err := relay.ParseHeader(b[:n])
				if err == nil && len(payload) >= 12 && payload[0]>>6 == 2 && (payload[1] < 192 || payload[1] > 223) {
					if p.withhold && payload[1]&0x7f == 111 && len(p.packets) < 40 {
						p.packets = append(p.packets, append([]byte(nil), b[:n]...))
						drop = true
					} else if !p.withhold {
						p.forwarded = append(p.forwarded[:0], payload...)
					}
				}
			}
			p.mu.Unlock()
			if !drop && to.IsValid() {
				_, _ = c.WriteToUDPAddrPort(b[:n], to)
			}
		}
	}()
	t.Cleanup(func() {
		_ = c.Close()
		select {
		case <-p.done:
		case <-time.After(time.Second):
			t.Error("leg socket did not stop")
		}
	})
	return p
}

func TestDialOutProcessStaleReconnect(t *testing.T) {
	var armed atomic.Bool
	helloSeen := make(chan controlplane.LinkHello, 1)
	releaseHello := make(chan struct{})
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(releaseHello) }) })
	var staleCommands atomic.Uint64
	var rejoinMu sync.Mutex
	var challenge, ack string
	var phase atomic.Bool
	var leg *processStaleLeg
	var raw *processRawSocket
	var storePath *processStorePartition
	r := newConfiguredProcessRig(t, false, false, func(r *dialProcessRig) {
		storePath = newProcessStorePartition(t, redistestenv.Address(t))
		r.workerRedis = storePath.listener.Addr().String()
		leg = newProcessStaleLeg(t, r.topology.relayLeg)
		r.legOverride = leg.conn.LocalAddr().String()
		r.callerSocket = func() (net.PacketConn, error) { raw = processCallerSocket(t); return raw, nil }
		r.gate.workerVisit = func(ctx context.Context, m workerlink.Message) error {
			if m.Kind == "hello" && armed.CompareAndSwap(true, false) {
				var hello controlplane.LinkHello
				if err := json.Unmarshal(m.Body, &hello); err != nil {
					return err
				}

				helloSeen <- hello
				select {
				case <-releaseHello:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return nil
		}
		r.gate.heartbeatRequest = func(h controlplane.HeartbeatRequest) {
			if h.Token != "" {
				rejoinMu.Lock()
				ack = h.Token
				rejoinMu.Unlock()
			}
		}
		r.gate.heartbeatReply = func(h controlplane.HeartbeatReply) {
			if h.Token != "" {
				rejoinMu.Lock()
				challenge = h.Token
				rejoinMu.Unlock()
			}
		}
		r.gate.controlVisit = func(m workerlink.Message) error {
			if phase.Load() && m.Kind == "request" && m.Method != http.MethodGet {
				staleCommands.Add(1)
			}
			return nil
		}
	})
	c := r.caller(t, 30*time.Second)
	// Positive control: actual worker ciphertext reaches the caller raw socket.
	leg.mu.Lock()
	positive := string(leg.forwarded)
	leg.withhold = true
	leg.mu.Unlock()
	require.NotEmpty(t, positive)
	require.Eventually(t, func() bool { raw.mu.Lock(); defer raw.mu.Unlock(); return raw.seen[positive] > 0 }, time.Second, 10*time.Millisecond)
	require.Eventually(t, func() bool { leg.mu.Lock(); defer leg.mu.Unlock(); return len(leg.packets) == 40 }, 2*time.Second, 10*time.Millisecond)
	old, err := r.store.Get(r.ctx, c.SessionID())
	require.NoError(t, err)
	phase.Store(true)
	storePath.partition(true)
	r.gate.heartbeatsBlocked.Store(true)
	r.gate.fault(false)
	s, err := r.topology.waitControl(r.ctx, 2*time.Second, "partition takeover", func(s controlplane.Status) bool {
		return len(s.Takeovers) == 1 && len(s.Calls) == 1 && s.Calls[0].Owner == "1" && !pairWorker(s, "0").Recovering
	})
	require.NoError(t, err)
	require.Greater(t, s.Calls[0].Epoch, old.Epoch)
	armed.Store(true)
	r.gate.restore()
	var hello controlplane.LinkHello
	select {
	case hello = <-helloSeen:
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not produce reconnect hello")
	}
	require.Len(t, hello.Status.Inventory, 1)
	require.Equal(t, old.Epoch, hello.Status.Inventory[0].Epoch)
	require.Zero(t, hello.DeathCycle)
	leg.mu.Lock()
	packets := append([][]byte(nil), leg.packets...)
	leg.mu.Unlock()
	for _, p := range packets {
		_, err = leg.conn.WriteToUDPAddrPort(p, leg.relay)
		require.NoError(t, err)
	}
	release.Do(func() { close(releaseHello) })
	r.gate.heartbeatsBlocked.Store(false)
	r.ready(t)
	// Eligibility alone is not proof: the old worker must report no installed
	// tenure, then accept a move back without an already-running-session error.
	var inventory mediaworker.WorkerStatus
	require.Eventually(t, func() bool {
		e := privateapi.Do(r.ctx, &http.Client{Timeout: time.Second}, "http://"+r.topology.controlAddr, http.MethodGet, "/private/workers/0/status", nil, &inventory, nil)
		return e == nil && inventory.InventoryKnown && inventory.Sessions == 0 && len(inventory.Inventory) == 0
	}, 3*time.Second, 10*time.Millisecond)
	for _, p := range packets {
		_, err = leg.conn.WriteToUDPAddrPort(p, leg.relay)
		require.NoError(t, err)
	}
	time.Sleep(100 * time.Millisecond)
	raw.mu.Lock()
	accepted := 0
	for _, p := range packets {
		_, payload, e := relay.ParseHeader(p)
		require.NoError(t, e)
		accepted += raw.seen[string(payload)]
	}
	raw.mu.Unlock()
	require.Zero(t, accepted, "old ciphertext must not reach raw caller socket")
	require.Zero(t, staleCommands.Load())
	rejoinMu.Lock()
	gotChallenge, gotAck := challenge, ack
	rejoinMu.Unlock()
	require.NotEmpty(t, gotChallenge, "control must issue a rejoin challenge")
	require.Equal(t, gotChallenge, gotAck, "worker must echo the challenge after dropping old sessions")
	phase.Store(false)
	storePath.partition(false)
	require.NoError(t, c.MoveExternal(r.ctx, 0))
	time.Sleep(700 * time.Millisecond)
	r.cleanCall(t, c)
	t.Logf("DIAL_OUT_STALE old_cycle=%d old_epoch=%d new_epoch=%d stale_commands=0 stale_packets_sent=80 raw_received=0 rejoin=true false_takeovers=0 decryption_failures=0", hello.DeathCycle, old.Epoch, s.Calls[0].Epoch)
}
