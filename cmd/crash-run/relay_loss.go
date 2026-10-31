//go:build !windows

package main

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/relaishq/relais/internal/clusterprocess"
	"github.com/relaishq/relais/internal/relaytrace"
	"github.com/relaishq/relais/pkg/callharness"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/relaishq/relais/pkg/storage"
)

type lossProbe struct {
	net.PacketConn
	mu     sync.Mutex
	packet []byte
}

func (p *lossProbe) WriteTo(b []byte, a net.Addr) (int, error) {
	if len(b) > 12 && b[0]>>6 == 2 && b[1]&127 == 111 {
		p.mu.Lock()
		p.packet = append(p.packet[:0], b...)
		p.mu.Unlock()
	}
	return p.PacketConn.WriteTo(b, a)
}
func (p *lossProbe) replay(a net.Addr) error {
	p.mu.Lock()
	b := append([]byte(nil), p.packet...)
	p.mu.Unlock()
	if len(b) == 0 {
		return fmt.Errorf("no caller ciphertext captured")
	}
	for range 20 {
		if _, err := p.PacketConn.WriteTo(b, a); err != nil {
			return err
		}
	}
	return nil
}

func runRelayLossTrials(ctx context.Context, manager *clusterprocess.Manager, bin, dir, key, addr string, runs int, after time.Duration) error {
	for i := range runs {
		for _, freeze := range []bool{true, false} {
			mode := "SIGKILL"
			if freeze {
				mode = "SIGSTOP-SIGCONT"
			}
			trialDir := filepath.Join(dir, fmt.Sprintf("relay-loss-%s-%02d", mode, i+1))
			if err := os.Mkdir(trialDir, 0700); err != nil {
				return err
			}
			bounded, cancel := context.WithTimeout(ctx, after+35*time.Second)
			err := relayLossTrial(bounded, manager, bin, trialDir, key, addr, after, freeze)
			cancel()
			if err != nil {
				return fmt.Errorf("relay loss %s trial %d: %w (logs %s)", mode, i+1, err, trialDir)
			}
		}
	}
	fmt.Printf("PASS: %d interleaved trials per relay-loss mode; 8 calls per trial\n", runs)
	return nil
}

func relayLossTrial(ctx context.Context, manager *clusterprocess.Manager, bin, dir, keyHex, addr string, after time.Duration, freeze bool) (trialErr error) {
	prefix := fmt.Sprintf("relais:slot-loss:%s:", filepath.Base(dir))
	env := processEnv(keyHex, addr, prefix)
	key, err := hex.DecodeString(keyHex)
	if err != nil {
		return err
	}
	store, err := sessionstore.NewRedis(ctx, storage.RedisConfig{Addr: addr, Prefix: prefix}, key)
	if err != nil {
		return err
	}
	defer store.Close()
	var children []*clusterprocess.Child
	defer func() {
		for i := len(children) - 1; i >= 0; i-- {
			children[i].Stop()
		}
	}()
	start := func(name string, args ...string) (*clusterprocess.Child, clusterprocess.Ready, error) {
		child, e := manager.Start(dir, name, withoutKey(env), args...)
		if e != nil {
			return nil, clusterprocess.Ready{}, e
		}
		children = append(children, child)
		ready, e := child.Ready(ctx)
		return child, ready, e
	}
	var bindings []controlplane.RelayBinding
	var active *clusterprocess.Child
	var first clusterprocess.Ready
	for _, id := range []string{"r1", "r2"} {
		child, ready, e := start(id, filepath.Join(bin, "relais-relay"), "-buffer-off", "-relay-lease", id, "-relay-slot", id, "-media", "127.0.0.1:0", "-leg", "127.0.0.1:0", "-http", "127.0.0.1:0")
		if e != nil {
			return e
		}
		if id == "r1" {
			active, first = child, ready
		}
		bindings = append(bindings, controlplane.RelayBinding{Slot: mediaworker.RelaySlot{ID: id, Leg: netip.MustParseAddrPort(ready.Leg), Public: netip.MustParseAddrPort(ready.Media)}, Relay: &controlplane.RemoteRelay{URL: ready.HTTP}})
	}
	// A waiting standby emits no READY, because it binds no sockets. Keep its
	// real child handle; pausing/killing both prevents same-slot takeover.
	standby, err := manager.Start(dir, "standby-r1", withoutKey(env), filepath.Join(bin, "relais-relay"), "-standby", "-buffer-off", "-relay-lease", "r1", "-relay-slot", "r1", "-media", first.Media, "-leg", first.Leg, "-http", strings.TrimPrefix(first.HTTP, "http://"))
	if err != nil {
		return err
	}
	children = append(children, standby)
	w, err := mediaworker.New(mediaworker.Config{Relay: &mediaworker.RelayConfig{Owners: store, Addr: bindings[0].Slot.Leg, PublicAddr: bindings[0].Slot.Public}})
	if err != nil {
		return err
	}
	defer w.Close()
	p := controlplane.New(bindings[0].Relay, store)
	if err = p.Register("0", w.LocalAddr(), w); err != nil {
		return err
	}
	second, err := mediaworker.New(mediaworker.Config{Relay: &mediaworker.RelayConfig{Owners: store, Addr: bindings[0].Slot.Leg, PublicAddr: bindings[0].Slot.Public}})
	if err != nil {
		return err
	}
	defer second.Close()
	if err = p.Register("1", second.LocalAddr(), second); err != nil {
		return err
	}
	workers := []*mediaworker.Worker{w, second}
	for _, b := range bindings {
		for _, worker := range workers {
			if err = b.Relay.(*controlplane.RemoteRelay).AddWorker(ctx, worker.LocalAddr()); err != nil {
				return err
			}
		}
	}
	if err = p.ConfigureRelaySet(ctx, bindings); err != nil {
		return err
	}
	server := httptest.NewServer(p.Handler())
	defer server.Close()
	var probes []*lossProbe
	h, err := callharness.Start(callharness.Options{External: &callharness.ExternalTopology{SignalingURL: server.URL + "/calls", RelayAddr: bindings[0].Slot.Public, CallerSocket: func() (net.PacketConn, error) {
		socket, e := net.ListenPacket("udp4", "127.0.0.1:0")
		if e != nil {
			return nil, e
		}
		probe := &lossProbe{PacketConn: socket}
		probes = append(probes, probe)
		return probe, nil
	}}})
	if err != nil {
		return err
	}
	defer h.Close()
	fmt.Printf("RELAY_LOSS_STAGE freeze=%t phase=ready\n", freeze)
	var calls, other []*callharness.Call
	var oldProbes []*lossProbe
	for len(calls) < 8 {
		c, e := h.Dial(ctx, callharness.CallOptions{Video: true, RelayChange: true, Worker: (len(probes) / 2) % 2})
		if e != nil {
			return e
		}
		_, assignment, e := store.RelayOwner(ctx, c.SessionID())
		if e != nil {
			return e
		}
		if assignment.Slot == "r1" {
			calls = append(calls, c)
			oldProbes = append(oldProbes, probes[len(probes)-1])
		} else {
			other = append(other, c)
		}
	}
	for _, c := range other {
		if _, err = c.Hangup(ctx); err != nil {
			return err
		}
	}
	life, stop := context.WithCancel(ctx)
	defer stop()
	done := make(chan error, 1)
	for _, worker := range workers {
		_ = p.Heartbeat(worker.LocalAddr())
	}
	go func() { done <- p.Run(life) }()
	defer func() { stop(); <-done }()
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-life.Done():
				return
			case <-tick.C:
				for _, worker := range workers {
					_ = p.Heartbeat(worker.LocalAddr())
				}
			}
		}
	}()
	defer func() { stop(); <-heartbeatDone }()
	media, stopMedia := context.WithCancel(ctx)
	sent := make(chan error, 8)
	for _, c := range calls {
		go func() {
			e := c.SendMedia(media, after+15*time.Second)
			if e != nil && media.Err() == nil {
				fmt.Printf("RELAY_LOSS_MEDIA_ERROR %v\n", e)
			}
			sent <- e
		}()
	}
	defer func() {
		stopMedia()
		for range calls {
			e := <-sent
			causes := []error{e}
			if joined, ok := e.(interface{ Unwrap() []error }); ok {
				causes = joined.Unwrap()
			}
			for _, cause := range causes {
				if cause != nil && !errors.Is(cause, context.Canceled) && !errors.Is(cause, context.DeadlineExceeded) {
					trialErr = errors.Join(trialErr, cause)
				}
			}
		}
	}()
	if err = lossWait(ctx, func() bool {
		for _, c := range calls {
			r := c.RelaySnapshot()
			if len(r.Tracks) != 2 {
				return false
			}
			for _, t := range r.Tracks {
				if t.Packets < 20 {
					return false
				}
			}
		}
		return true
	}); err != nil {
		return err
	}
	var cutoff atomic.Bool
	var oldInput, oldOutput atomic.Uint64
	oldLeg := bindings[0].Slot.Leg.String()
	stopTrace := relaytrace.Observe(func(_ time.Time, name, path string, _ []byte) {
		if path == oldLeg && name == "worker-relay-cutoff" {
			cutoff.Store(true)
		}
		if cutoff.Load() && path == oldLeg {
			if name == "worker-input" || name == "worker-check" {
				oldInput.Add(1)
			}
			if name == "worker-leg-output" {
				oldOutput.Add(1)
			}
		}
	})
	defer stopTrace()
	fmt.Printf("RELAY_LOSS_STAGE freeze=%t phase=baseline\n", freeze)
	fault := time.Now()
	signal := syscall.SIGKILL
	if freeze {
		signal = syscall.SIGSTOP
	}
	if err = standby.Signal(signal); err != nil {
		return err
	}
	if err = active.Signal(signal); err != nil {
		return err
	}
	fmt.Printf("RELAY_LOSS_STAGE freeze=%t phase=fault\n", freeze)
	recoveryCtx, stopRecovery := context.WithDeadline(ctx, fault.Add(5*time.Second))
	defer stopRecovery()
	var gaps []time.Duration
	for _, c := range calls {
		if err = lossWait(recoveryCtx, func() bool {
			_, a, e := store.RelayOwner(ctx, c.SessionID())
			return e == nil && a.Slot == "r2" && a.Pending == ""
		}); err != nil {
			_, a, readErr := store.RelayOwner(context.WithoutCancel(ctx), c.SessionID())
			return fmt.Errorf("assignment progress: %w; assignment=%+v read=%v; %s", err, a, readErr, c.RelaySnapshot().Summary())
		}
		fmt.Printf("RELAY_LOSS_STAGE freeze=%t phase=committed\n", freeze)
		observed := time.Now()
		var gap time.Duration
		if err = lossWait(recoveryCtx, func() bool { gap = c.RelayRecoveryGap(fault, observed); return gap > 0 }); err != nil {
			return fmt.Errorf("decoded progress: %w; %s", err, c.RelaySnapshot().Summary())
		}
		if gap > 5*time.Second {
			return fmt.Errorf("recovery exceeded 5s: %s", gap)
		}
		gaps = append(gaps, gap)
	}
	if !cutoff.Load() {
		return fmt.Errorf("worker cut-off boundary missing")
	}
	if freeze {
		remote := bindings[0].Relay.(*controlplane.RemoteRelay)
		if err = active.Signal(syscall.SIGCONT); err != nil {
			return err
		}
		if err = standby.Signal(syscall.SIGCONT); err != nil {
			return err
		}
		before, e := remote.Status(ctx)
		if e != nil {
			return e
		}
		target := net.UDPAddrFromAddrPort(bindings[0].Slot.Public)
		for _, probe := range oldProbes {
			if err = probe.replay(target); err != nil {
				return err
			}
		}
		// Require real old-relay forwarding, so the receive-fence assertion is not
		// vacuous. The ciphertext is replayed unchanged and must not reach decrypt.
		if err = lossWait(ctx, func() bool {
			s, e := remote.Status(ctx)
			return e == nil && s.Stats.CallerPackets >= before.Stats.CallerPackets+160
		}); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
		last, e := remote.Status(ctx)
		if e != nil {
			return e
		}
		if last.Stats.WorkerPackets != before.Stats.WorkerPackets {
			return fmt.Errorf("worker sent packets to resumed old slot")
		}
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(after):
	}
	for _, c := range calls {
		lease, e := store.Get(ctx, c.SessionID())
		if e != nil {
			return e
		}
		var failures uint64
		for _, worker := range workers {
			if worker.LocalAddr() == lease.Worker {
				failures, e = worker.SessionDecryptFailures(c.SessionID())
				if e != nil {
					return e
				}
			}
		}
		if failures != 0 {
			return fmt.Errorf("worker auth failures: %d", failures)
		}
		report := c.RelaySnapshot()
		if report.DecryptionFailures.Total() != 0 {
			return fmt.Errorf("caller auth failure")
		}
		for _, t := range report.Tracks {
			if t.DuplicatePackets != 0 {
				return fmt.Errorf("duplicate caller RTP")
			}
		}
	}
	if oldInput.Load() != 0 || oldOutput.Load() != 0 {
		return fmt.Errorf("old slot accepted=%d sent=%d", oldInput.Load(), oldOutput.Load())
	}
	sort.Slice(gaps, func(i, j int) bool { return gaps[i] < gaps[j] })
	fmt.Printf("RELAY_LOSS freeze=%t n=8 decoded_upper_bounds p50=%s p95=%s max=%s samples=%v old_accepted=0 old_sent=0 auth=0 duplicates=0\n", freeze, gaps[3], gaps[7], gaps[7], gaps)
	return nil
}
func lossWait(ctx context.Context, ready func() bool) error {
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		if ready() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}
