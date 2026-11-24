//go:build !windows

package main

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
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

	"github.com/pion/logging"
	"github.com/relaishq/relais/internal/clusterprocess"
	"github.com/relaishq/relais/internal/nettopology"
	"github.com/relaishq/relais/internal/privateapi"
	"github.com/relaishq/relais/internal/relaytrace"
	"github.com/relaishq/relais/pkg/callharness"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/relay"
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

func runRelayLossTrials(ctx context.Context, manager *clusterprocess.Manager, bin, dir, key, addr string, runs int, after time.Duration, shipped bool, topology *nettopology.Topology, password string) error {
	modes := []bool{true, false}
	if topology != nil {
		modes = []bool{false}
	}
	for i := range runs {
		for _, freeze := range modes {
			mode := "SIGKILL"
			if freeze {
				mode = "SIGSTOP-SIGCONT"
			}
			if topology != nil {
				mode = "netns-blackhole"
			}
			trialDir := filepath.Join(dir, fmt.Sprintf("relay-loss-%s-%02d", mode, i+1))
			if err := os.Mkdir(trialDir, 0700); err != nil {
				return err
			}
			bounded, cancel := context.WithTimeout(ctx, after+35*time.Second)
			err := relayLossTrial(bounded, manager, bin, trialDir, key, addr, after, freeze, shipped, topology, password)
			cancel()
			if err != nil {
				return fmt.Errorf("relay loss %s trial %d: %w (logs %s)", mode, i+1, err, trialDir)
			}
		}
	}
	fmt.Printf("PASS: %d trials per relay-loss mode; modes=%d; 8 calls per trial\n", runs, len(modes))
	return nil
}

func relayLossTrial(ctx context.Context, manager *clusterprocess.Manager, bin, dir, keyHex, addr string, after time.Duration, freeze, shipped bool, topology *nettopology.Topology, password string) (trialErr error) {
	prefix := fmt.Sprintf("relais:slot-loss:%s:", filepath.Base(dir))
	env := processEnv(keyHex, addr, prefix)
	if topology != nil {
		env = append(env, "RELAIS_PRIVATE_NETS="+topology.Plan.PrivateNets(), "RELAIS_REDIS_PASSWORD="+password)
	}
	key, err := hex.DecodeString(keyHex)
	if err != nil {
		return err
	}
	store, err := sessionstore.NewRedis(ctx, storage.RedisConfig{Addr: addr, Prefix: prefix, Password: password}, key)
	if err != nil {
		return err
	}
	defer store.Close()
	if !shipped {
		// Workers and control run in this process; child relay logs are already
		// retained by Manager.Start. Keep control output in the trial artifact too.
		controlLog, err := os.OpenFile(filepath.Join(dir, "control.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
		if err != nil {
			return err
		}
		previousLog := log.Writer()
		log.SetOutput(io.MultiWriter(previousLog, controlLog))
		defer func() { log.SetOutput(previousLog); _ = controlLog.Close() }()
	}
	var children []*clusterprocess.Child
	defer func() {
		for i := len(children) - 1; i >= 0; i-- {
			children[i].Stop()
		}
	}()
	start := func(name string, args ...string) (*clusterprocess.Child, clusterprocess.Ready, error) {
		childEnv := withoutKey(env)
		if name == "control" || strings.HasPrefix(name, "worker-") {
			childEnv = env
		}
		role := name
		switch name {
		case "r1":
			role = "relay"
		case "r2":
			role = "relay-spare"
		}
		var child *clusterprocess.Child
		var e error
		if topology == nil {
			child, e = manager.Start(dir, name, childEnv, args...)
		} else {
			child, e = manager.StartInNamespace(dir, name, topology.Plan.Roles[role].Namespace, childEnv, args...)
		}
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
		role := "relay"
		if id == "r2" {
			role = "relay-spare"
		}
		child, ready, e := start(id, filepath.Join(bin, "relais-relay"), "-buffer-off", "-relay-lease", id, "-relay-slot", id, "-media", roleBind(topology, role, true), "-leg", roleBind(topology, role, false), "-http", roleBind(topology, role, false))
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
	var standby *clusterprocess.Child
	if topology == nil {
		standby, err = manager.Start(dir, "standby-r1", withoutKey(env), filepath.Join(bin, "relais-relay"), "-standby", "-buffer-off", "-relay-lease", "r1", "-relay-slot", "r1", "-media", first.Media, "-leg", first.Leg, "-http", strings.TrimPrefix(first.HTTP, "http://"))
		if err != nil {
			return err
		}
		children = append(children, standby)
	}
	var p *controlplane.Plane
	var workers []*mediaworker.Worker
	var workerURLs []string
	var signalingURL string
	if shipped {
		controlAddr, e := controlAddress(topology)
		if e != nil {
			return e
		}
		signalingURL = "http://" + controlAddr
		args := []string{filepath.Join(bin, "relais-control"), "-http", controlAddr}
		for _, b := range bindings {
			args = append(args, "-relay", b.Slot.ID+"="+b.Relay.(*controlplane.RemoteRelay).URL)
		}
		for i := range 2 {
			name := fmt.Sprint(i)
			_, ready, e := start("worker-"+name, filepath.Join(bin, "relais-worker"), "-name", name, "-media", roleBind(topology, "worker-"+name, false), "-http", roleBind(topology, "worker-"+name, false), "-control", signalingURL, "-relay-leg", first.Leg, "-relay-media", first.Media, "-agent", "echo")
			if e != nil {
				return e
			}
			workerURLs = append(workerURLs, ready.HTTP)
			args = append(args, "-worker", name+"="+ready.HTTP)
		}
		if _, _, e := start("control", args...); e != nil {
			return e
		}
		if e := lossWait(ctx, func() bool {
			var status controlplane.Status
			err := privateapi.Do(ctx, http.DefaultClient, signalingURL, http.MethodGet, "/status", nil, &status, nil)
			return err == nil && status.Ready && len(status.Workers) == 2
		}); e != nil {
			return e
		}
	} else {
		p = controlplane.New(bindings[0].Relay, store)
		for i := range 2 {
			workerLog, e := os.OpenFile(filepath.Join(dir, fmt.Sprintf("worker-%d.log", i)), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
			if e != nil {
				return e
			}
			defer workerLog.Close()
			// Fix the level explicitly: environment-enabled Pion debug/trace logs
			// can contain credentials and must not enter uploaded artifacts.
			factory := &logging.DefaultLoggerFactory{Writer: workerLog, DefaultLogLevel: logging.LogLevelInfo}
			worker, e := mediaworker.New(mediaworker.Config{LoggerFactory: factory, Relay: &mediaworker.RelayConfig{Owners: store, Addr: bindings[0].Slot.Leg, PublicAddr: bindings[0].Slot.Public}})
			if e != nil {
				return e
			}
			defer worker.Close()
			if err = p.Register(fmt.Sprint(i), worker.LocalAddr(), worker); err != nil {
				return err
			}
			workers = append(workers, worker)
		}
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
		signalingURL = server.URL
	}
	if topology != nil {
		if err := topology.AllowMedia(ctx, bindings[0].Slot.Public, bindings[1].Slot.Public); err != nil {
			return err
		}
		if err := topology.VerifyPrivateAPIs(ctx, []string{
			strings.TrimPrefix(bindings[0].Relay.(*controlplane.RemoteRelay).URL, "http://"),
			strings.TrimPrefix(bindings[1].Relay.(*controlplane.RemoteRelay).URL, "http://"),
		}); err != nil {
			return fmt.Errorf("phase=private-api-isolation: %w", err)
		}
	}
	var probes []*lossProbe
	h, err := callharness.Start(callharness.Options{External: &callharness.ExternalTopology{SignalingURL: signalingURL + "/calls", RelayAddr: bindings[0].Slot.Public, CallerSocket: func() (net.PacketConn, error) {
		var socket net.PacketConn
		var e error
		if topology == nil {
			socket, e = net.ListenPacket("udp4", "127.0.0.1:0")
		} else {
			socket, e = topology.CallerSocket()
		}
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
	if !shipped {
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
	}
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
	// In the in-process lane, observe successful worker leg writes directly,
	// independently of the child relay's receive queue. The process lane uses
	// relay forwarding counters and worker metrics instead.
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
	blackholed := false
	if topology != nil {
		if err = topology.BlackholeRelay(ctx, true); err != nil {
			return fmt.Errorf("phase=blackhole: %w", err)
		}
		blackholed = true
		defer func() {
			if blackholed {
				cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				trialErr = errors.Join(trialErr, topology.BlackholeRelay(cleanup, false))
			}
		}()
	} else {
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
			report := c.RelaySnapshot()
			return relayLossCallError(c.SessionID(), "assignment", relayLossObservedGap(report, time.Now()), fmt.Errorf("%w; assignment=%+v read=%v; recovery unobserved; caller media: %s", err, a, readErr, report.Summary()))
		}
		fmt.Printf("RELAY_LOSS_STAGE freeze=%t phase=committed\n", freeze)
		observed := time.Now()
		var gap time.Duration
		if err = lossWait(recoveryCtx, func() bool { gap = c.RelayRecoveryGap(fault, observed); return gap > 0 }); err != nil {
			report := c.RelaySnapshot()
			return relayLossCallError(c.SessionID(), "decoded-progress", relayLossObservedGap(report, time.Now()), fmt.Errorf("%w; decoded recovery unobserved; caller media: %s", err, report.Summary()))
		}
		if gap > 5*time.Second {
			return relayLossCallError(c.SessionID(), "recovery", gap, errors.New("exceeded 5s"))
		}
		gaps = append(gaps, gap)
		fmt.Printf("RELAY_LOSS_CALL call=%s phase=recovered gap=%s\n", c.SessionID(), gap)
	}
	if !shipped && !cutoff.Load() {
		return fmt.Errorf("worker cut-off boundary missing")
	}
	var resumedBaseline relay.RelayStatus
	if freeze || topology != nil {
		remote := bindings[0].Relay.(*controlplane.RemoteRelay)
		if freeze {
			if err = active.Signal(syscall.SIGCONT); err != nil {
				return err
			}
			if err = standby.Signal(syscall.SIGCONT); err != nil {
				return err
			}
		} else {
			lease, e := store.GetRelay(ctx, "r1")
			if e != nil {
				return fmt.Errorf("phase=before-network-restore: %w", e)
			}
			if err = topology.BlackholeRelay(ctx, false); err != nil {
				return fmt.Errorf("phase=network-restore: %w", err)
			}
			blackholed = false
			renewalCtx, stopRenewal := context.WithTimeout(ctx, 3*time.Second)
			err = lossWait(renewalCtx, func() bool {
				next, e := store.GetRelay(renewalCtx, "r1")
				return e == nil && next.Holder == lease.Holder && next.Epoch == lease.Epoch && !next.Expired && next.ExpiresAt.After(lease.ExpiresAt)
			})
			stopRenewal()
			if err != nil {
				return fmt.Errorf("phase=restored-relay-renewal: %w", err)
			}
			fmt.Println("RELAY_LOSS_STAGE phase=network-restored renewal=observed")
		}
		// SIGSTOP leaves pre-cut-off worker datagrams queued in r1's UDP
		// socket. Its HTTP status can answer before that backlog drains.
		before, e := lossStableStatus(ctx, remote.Status)
		if e != nil {
			return fmt.Errorf("%w; oldInput=%d oldOutput=%d cutoff=%t", e, oldInput.Load(), oldOutput.Load(), cutoff.Load())
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
		if err := lossResumedWorkerFence(before, last); err != nil {
			return err
		}
		if oldOutput.Load() != 0 {
			return fmt.Errorf("resumed old slot worker fence failed: oldOutput=%d", oldOutput.Load())
		}
		resumedBaseline = before
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(after):
	}
	if topology != nil {
		last, e := bindings[0].Relay.(*controlplane.RemoteRelay).Status(ctx)
		if e != nil {
			return fmt.Errorf("phase=restored-relay-observation: %w", e)
		}
		if err := lossResumedWorkerFence(resumedBaseline, last); err != nil {
			return err
		}
	}
	for _, endpoint := range workerURLs {
		var callIDs []string
		var maxGap time.Duration
		for index, c := range calls {
			callIDs = append(callIDs, c.SessionID())
			maxGap = max(maxGap, gaps[index])
		}
		values, _, e := scrapeMetrics(ctx, http.DefaultClient, endpoint+"/metrics")
		if e != nil {
			return relayLossCallError(strings.Join(callIDs, ","), "worker-decryption-evidence", maxGap, e)
		}
		failures, ok := values["relais_worker_decryption_failures_total"]
		if !ok || failures != 0 {
			return relayLossCallError(strings.Join(callIDs, ","), "worker-decryption", maxGap, fmt.Errorf("worker=%s present=%t failures=%g; counter is worker-wide", endpoint, ok, failures))
		}
	}
	for index, c := range calls {
		lease, e := store.Get(ctx, c.SessionID())
		if e != nil {
			return relayLossCallError(c.SessionID(), "post-recovery-lease", gaps[index], e)
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
			return relayLossCallError(c.SessionID(), "worker-decryption", gaps[index], fmt.Errorf("failures=%d", failures))
		}
		report := c.RelaySnapshot()
		if topology != nil {
			if err := relayLossMediaProgress(c.SessionID(), report, time.Now()); err != nil {
				return err
			}
		}
		if report.DecryptionFailures.Total() != 0 {
			return relayLossCallError(c.SessionID(), "caller-decryption", gaps[index], errors.New("authentication failure"))
		}
		for _, t := range report.Tracks {
			if t.DuplicatePackets != 0 {
				return relayLossCallError(c.SessionID(), "duplicates", gaps[index], fmt.Errorf("track=%s duplicates=%d", t.Kind, t.DuplicatePackets))
			}
		}
	}
	if oldInput.Load() != 0 || oldOutput.Load() != 0 {
		return fmt.Errorf("old slot oldInput=%d oldOutput=%d cutoff=%t", oldInput.Load(), oldOutput.Load(), cutoff.Load())
	}
	sort.Slice(gaps, func(i, j int) bool { return gaps[i] < gaps[j] })
	if topology != nil {
		select {
		case <-active.Done():
			return errors.New("phase=post-recovery: blackholed relay exited; live-process loss not proven")
		default:
		}
		fmt.Printf("RELAY_LOSS_NETNS n=8 decoded_upper_bounds p50=%s p95=%s max=%s samples=%v auth=0 duplicates=0 relay_alive=true\n", gaps[3], gaps[7], gaps[7], gaps)
	} else if shipped {
		fmt.Printf("RELAY_LOSS_PROCESSES freeze=%t n=8 decoded_upper_bounds p50=%s p95=%s max=%s samples=%v auth=0 duplicates=0\n", freeze, gaps[3], gaps[7], gaps[7], gaps)
	} else {
		fmt.Printf("RELAY_LOSS freeze=%t n=8 decoded_upper_bounds p50=%s p95=%s max=%s samples=%v old_accepted=0 old_sent=0 auth=0 duplicates=0\n", freeze, gaps[3], gaps[7], gaps[7], gaps)
	}
	return nil
}

// lossStableStatus waits for the resumed relay to drain pre-cut-off output
// before choosing the replay baseline. Only WorkerPackets must settle.
func lossStableStatus(ctx context.Context, status func(context.Context) (relay.RelayStatus, error)) (relay.RelayStatus, error) {
	const quiet = 200 * time.Millisecond
	const limit = 3 * time.Second
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	var last relay.RelayStatus
	var stableSince time.Time
	for {
		next, err := status(ctx)
		if err != nil {
			return last, fmt.Errorf("resumed old relay worker packets did not settle (limit %s, last=%+v): %w", limit, last.Stats, err)
		}
		if stableSince.IsZero() || next.Stats.WorkerPackets != last.Stats.WorkerPackets {
			stableSince = time.Now()
		}
		last = next
		if time.Since(stableSince) >= quiet {
			return last, nil
		}
		select {
		case <-ctx.Done():
			return last, fmt.Errorf("resumed old relay worker packets did not settle (limit %s, last=%+v): %w", limit, last.Stats, ctx.Err())
		case <-tick.C:
		}
	}
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

func relayLossCallError(call, phase string, gap time.Duration, err error) error {
	return fmt.Errorf("call=%s phase=%s gap=%s: %w", call, phase, gap, err)
}

// Keep the network trial's full observation honest: a short recovery followed
// by another freeze must fail, including a freeze with no final packet.
func relayLossMediaProgress(call string, report *callharness.Report, now time.Time) error {
	if len(report.Tracks) != 2 {
		return relayLossCallError(call, "post-recovery", 0, errors.New("audio/video evidence missing"))
	}
	for _, track := range report.Tracks {
		gap := max(track.MediaGap, now.Sub(report.StartedAt)-track.LastArrival)
		if track.Packets == 0 || gap > 5*time.Second {
			return relayLossCallError(call, "post-recovery-"+track.Kind, gap, errors.New("media stopped or gap exceeded 5s"))
		}
	}
	return nil
}

// On a timeout there is no completed recovery gap. Report the caller's
// measured packet gap, including trailing silence, rather than fault age.
func relayLossObservedGap(report *callharness.Report, now time.Time) time.Duration {
	var gap time.Duration
	for _, track := range report.Tracks {
		gap = max(gap, track.MediaGap, now.Sub(report.StartedAt)-track.LastArrival)
	}
	return gap
}

func lossResumedWorkerFence(before, after relay.RelayStatus) error {
	if after.Stats.WorkerPackets != before.Stats.WorkerPackets {
		return fmt.Errorf("phase=restored-relay-fence: resumed old slot worker packets changed: before=%d after=%d", before.Stats.WorkerPackets, after.Stats.WorkerPackets)
	}
	return nil
}
