// crash-run is the opt-in real-process acceptance run, never a CI requirement.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/relais/internal/processrun"
	"github.com/relais/pkg/callharness"
)

type result struct {
	Gap                                              time.Duration
	Decrypt, AfterResume, Reconnects, Renegotiations int
	Decoded, Live, Detection                         time.Duration
	Pass                                             bool
}

func binaries(bin string) error {
	for _, name := range []string{"relais-relay", "relais-worker", "relais-control"} {
		stat, err := os.Stat(filepath.Join(bin, name))
		if err != nil || stat.IsDir() || stat.Mode()&0111 == 0 {
			return fmt.Errorf("build %s first (make build)", name)
		}
	}
	_, err := exec.LookPath("ffmpeg")
	if err != nil {
		return errors.New("ffmpeg is required for full video decode")
	}
	return nil
}
func processEnv(key, addr, prefix string) []string {
	env := []string{}
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if name == "RELAIS_SESSIONSTORE_KEY" || name == "RELAIS_REDIS_ADDR" || name == "RELAIS_REDIS_PREFIX" {
			continue
		}
		env = append(env, entry)
	}
	return append(env, "RELAIS_SESSIONSTORE_KEY="+key, "RELAIS_REDIS_ADDR="+addr, "RELAIS_REDIS_PREFIX="+prefix)
}
func trial(ctx context.Context, manager *processManager, bin, dir string, env []string, after time.Duration, verbose, terminate bool) (result, error) {
	var processes []*child
	defer func() {
		for i := len(processes) - 1; i >= 0; i-- {
			processes[i].stop()
		}
	}()
	start := func(name string, args ...string) (*child, ready, error) {
		childEnv := env
		if name == "relay" {
			childEnv = withoutKey(env)
		}
		c, err := manager.start(dir, name, childEnv, args...)
		if err != nil {
			return nil, ready{}, err
		}
		processes = append(processes, c)
		startup, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		r, err := c.ready(startup)
		return c, r, err
	}
	_, relayReady, err := start("relay", filepath.Join(bin, "relais-relay"), "-media", "127.0.0.1:0", "-leg", "127.0.0.1:0", "-http", "127.0.0.1:0")
	if err != nil {
		return result{}, err
	}
	controlAddr, err := freeTCP()
	if err != nil {
		return result{}, err
	}
	controlURL := "http://" + controlAddr
	workerPIDs := map[string]*child{}
	controlArgs := []string{filepath.Join(bin, "relais-control"), "-http", controlAddr, "-relay", relayReady.HTTP}
	for _, name := range []string{"0", "1"} {
		c, r, err := start("worker-"+name, filepath.Join(bin, "relais-worker"), "-media", "127.0.0.1:0", "-http", "127.0.0.1:0", "-name", name, "-control", controlURL, "-relay-leg", relayReady.Leg, "-relay-media", relayReady.Media)
		if err != nil {
			return result{}, err
		}
		workerPIDs[name] = c
		controlArgs = append(controlArgs, "-worker", name+"="+r.HTTP)
	}
	_, _, err = start("control", controlArgs...)
	if err != nil {
		return result{}, err
	}
	relayAddr, err := netip.ParseAddrPort(relayReady.Media)
	if err != nil {
		return result{}, err
	}
	h, err := callharness.Start(callharness.Options{External: &callharness.ExternalTopology{SignalingURL: controlURL + "/calls", RelayAddr: relayAddr}})
	if err != nil {
		return result{}, err
	}
	defer func() { _ = h.Close() }()
	call, err := h.Dial(ctx, callharness.CallOptions{Video: true, Worker: -1})
	if err != nil {
		return result{}, err
	}
	// These calls are continuous across SIGKILL, takeover and the consent window.
	mediaCtx, stopMedia := context.WithCancel(ctx)
	defer stopMedia()
	sent := make(chan error, 1)
	go func() { sent <- call.SendMedia(mediaCtx, after+15*time.Second) }()
	senderJoined := false
	defer func() {
		stopMedia()
		if !senderJoined {
			<-sent
		}
	}()
	select {
	case <-ctx.Done():
		return result{}, ctx.Err()
	case err := <-sent:
		senderJoined = true
		return result{}, fmt.Errorf("media ended before SIGKILL: %v", err)
	case <-time.After(2 * time.Second):
	}
	status, err := h.Status(ctx)
	if err != nil {
		return result{}, err
	}
	if len(status.Calls) != 1 {
		return result{}, errors.New("expected one owning call in /status")
	}
	owner := status.Calls[0].Owner
	worker := workerPIDs[owner]
	if worker == nil {
		return result{}, fmt.Errorf("unknown owning worker %q", owner)
	}
	killed := time.Now()
	signal := syscall.SIGKILL
	label := "SIGKILL"
	if terminate {
		signal, label = syscall.SIGTERM, "SIGTERM"
	}
	if err := worker.signalGroup(signal); err != nil {
		return result{}, err
	}
	if !terminate {
		h.RecordProcessKill(owner, killed)
	}
	fmt.Printf("%s worker=%s pid=%d session=%s relay=%s\n", label, owner, worker.pid, call.SessionID(), relayReady.Media)
	// Wait for a terminal takeover, then observe at least 60 s of consent in
	// the default run. The short developer mode cannot claim that acceptance.
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for len(status.Takeovers) == 0 && (!terminate || len(status.Calls) != 1 || status.Calls[0].Owner == owner) {
		select {
		case <-ctx.Done():
			return result{}, ctx.Err()
		case <-deadline.C:
			return result{}, errors.New("takeover did not complete within 3 s")
		case <-tick.C:
			status, err = h.Status(ctx)
			if err != nil {
				return result{}, err
			}
		}
	}
	if terminate {
		if len(status.Takeovers) != 0 {
			return result{}, errors.New("SIGTERM must drain through a planned move, not crash recovery")
		}
		select {
		case <-ctx.Done():
			return result{}, ctx.Err()
		case <-worker.done:
			if worker.err != nil {
				return result{}, fmt.Errorf("SIGTERM worker exit: %w", worker.err)
			}
		case <-time.After(6 * time.Second):
			return result{}, errors.New("SIGTERM worker did not finish drain and exit")
		}
	} else {
		takeover := status.Takeovers[0]
		if takeover.Lost || takeover.Error != "" {
			return result{}, fmt.Errorf("takeover lost=%t: %s", takeover.Lost, takeover.Error)
		}
	}
	select {
	case <-ctx.Done():
		return result{}, ctx.Err()
	case err := <-sent:
		senderJoined = true
		return result{}, fmt.Errorf("media ended before observation finished: %v", err)
	case <-time.After(after):
	}
	observedUntil := time.Now()
	stopMedia()
	mediaErr := <-sent
	senderJoined = true
	if mediaErr != nil && !errors.Is(mediaErr, context.Canceled) {
		return result{}, mediaErr
	}
	report, err := call.Hangup(ctx)
	if err != nil {
		return result{}, err
	}
	if verbose {
		fmt.Println(report.Summary())
	}
	if terminate {
		return measureDrain(report, relayReady.Media, observedUntil), nil
	}
	return measure(report, relayReady.Media, after), nil
}
func measure(report *callharness.Report, relayAddr string, after time.Duration) result {
	r := result{Decrypt: report.DecryptionFailures.Total(), Reconnects: report.ICERestarts, Renegotiations: report.Renegotiations}
	if len(report.Moves) != 1 {
		return r
	}
	move := report.Moves[0]
	r.AfterResume, r.Decoded, r.Live, r.Detection = move.DecryptionFailuresAfterResume, move.Recovery.FirstDecodedAfterKill, move.Recovery.FirstDecodedLiveAfterKill, move.DetectionTime
	flowing := len(move.Tracks) == 2
	for _, track := range move.Tracks {
		r.Gap = max(r.Gap, track.Gap)
		flowing = flowing && track.PacketsAfter > 0
	}
	video := report.Track("video")
	decoded := video != nil && video.Video != nil && video.Video.FullDecode.Ran() && video.Video.FullDecode.Errors == "" && video.Video.FullDecode.FramesDecoded == video.Video.FullDecode.FramesIn && video.Video.KeyframeDecodeErrors == 0
	r.Pass = flowing && decoded && r.Gap > 0 && r.Gap < 2*time.Second && r.AfterResume == 0 && r.Reconnects == 0 && r.Renegotiations == 0 && report.OfferAnswerExchanges == 1 && report.ConnectedThroughout() && report.RemoteAddr == relayAddr && r.Decoded > 0 && r.Live > 0 && move.Error == "" && report.Consent.ResponsesAfter > 0
	if after >= 60*time.Second {
		r.Pass = r.Pass && report.Consent.ObservedFor >= 60*time.Second
	}
	return r
}
func printTable(results []result) {
	fmt.Println("run  gap_ms  decrypt(total/after)  reconnects  renegotiations  decoded_ms  live_ms  detection_ms  result")
	for i, r := range results {
		state := "FAIL"
		if r.Pass {
			state = "PASS"
		}
		fmt.Printf("%3d  %6.1f  %7d/%-5d       %3d        %3d          %7.1f    %7.1f  %7.1f       %s\n", i+1, float64(r.Gap)/float64(time.Millisecond), r.Decrypt, r.AfterResume, r.Reconnects, r.Renegotiations, float64(r.Decoded)/float64(time.Millisecond), float64(r.Live)/float64(time.Millisecond), float64(r.Detection)/float64(time.Millisecond), state)
	}
}
func run() error {
	runs := flag.Int("runs", 10, "consecutive real SIGKILL trials")
	after := flag.Duration("after", 60*time.Second, "media/consent observation after takeover (60s for acceptance)")
	binArg := flag.String("bin", "bin", "built binaries and throwaway run directory")
	redisAddr := flag.String("redis", os.Getenv("RELAIS_REDIS_ADDR"), "explicit dedicated Redis address, or start a throwaway instance")
	redisBinary := flag.String("redis-server", "redis-server", "Redis executable for the throwaway instance")
	verbose := flag.Bool("verbose", false, "print full caller reports")
	terminate := flag.Bool("sigterm", false, "verify graceful owning-worker drain instead of crash takeover")
	flag.Parse()
	if *runs < 1 || *after < time.Second {
		return errors.New("runs must be positive and after must be at least 1s")
	}
	bin, err := filepath.Abs(*binArg)
	if err != nil {
		return err
	}
	if err := binaries(bin); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(bin, "crash-run-")
	if err != nil {
		return err
	}
	fmt.Printf("process logs: %s\n", dir)
	manager := &processManager{}
	ctx, cancel := manager.context()
	defer cancel()
	addr := *redisAddr
	if addr == "" {
		addr, err = freeTCP()
		if err != nil {
			return err
		}
		_, port, _ := net.SplitHostPort(addr)
		redis, err := manager.start(dir, "redis", os.Environ(), *redisBinary, "--bind", "127.0.0.1", "--port", port, "--save", "", "--appendonly", "no", "--dir", dir)
		if err != nil {
			return err
		}
		defer redis.stop()
		startup, stop := context.WithTimeout(ctx, 5*time.Second)
		err = waitTCP(startup, addr, redis)
		stop()
		if err != nil {
			return err
		}
	}
	if err := processrun.ValidateRedis(addr); err != nil {
		return err
	}
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		return err
	}
	if *after < 60*time.Second {
		fmt.Println("DEVELOPMENT RUN: 60 s post-takeover consent acceptance NOT-RUN")
	}
	results := []result{}
	for i := 0; i < *runs; i++ {
		trialDir := filepath.Join(dir, fmt.Sprintf("run-%02d", i+1))
		if err := os.Mkdir(trialDir, 0700); err != nil {
			return err
		}
		env := processEnv(hex.EncodeToString(key[:]), addr, fmt.Sprintf("relais:crash:%s:%d:", filepath.Base(dir), i))
		trialCtx, stop := context.WithTimeout(ctx, *after+30*time.Second)
		r, err := trial(trialCtx, manager, bin, trialDir, env, *after, *verbose, *terminate)
		stop()
		results = append(results, r)
		printTable(results)
		if err != nil {
			return err
		}
		if !r.Pass {
			return errors.New("caller-observed process handover threshold failed")
		}
	}
	label := "SIGKILL"
	if *terminate {
		label = "SIGTERM drain"
	}
	fmt.Printf("PASS: %d consecutive real-process %s trials\n", len(results), label)
	return nil
}
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func withoutKey(env []string) []string {
	result := make([]string, 0, len(env))
	for _, entry := range env {
		if !strings.HasPrefix(entry, "RELAIS_SESSIONSTORE_KEY=") {
			result = append(result, entry)
		}
	}
	return result
}

// Graceful shutdown is a planned move: no takeover event or crash margins.
// Measure the actual caller's longest media gap and require packets continuing
// through hangup, clean full decoding, and the original connection identity.
func measureDrain(report *callharness.Report, relayAddr string, observedUntil time.Time) result {
	r := result{Decrypt: report.DecryptionFailures.Total(), Reconnects: report.ICERestarts, Renegotiations: report.Renegotiations}
	flowing := len(report.Tracks) == 2
	for _, track := range report.Tracks {
		r.Gap = max(r.Gap, track.MediaGap)
		flowing = flowing && track.Packets > 0 && observedUntil.Sub(report.StartedAt.Add(track.LastArrival)) < 150*time.Millisecond
	}
	video := report.Track("video")
	decoded := video != nil && video.Video != nil && video.Video.FullDecode.Ran() && video.Video.FullDecode.Errors == "" && video.Video.FullDecode.FramesDecoded == video.Video.FullDecode.FramesIn && video.Video.KeyframeDecodeErrors == 0
	r.Pass = flowing && decoded && r.Gap > 0 && r.Gap < 100*time.Millisecond && r.Decrypt == 0 && r.Reconnects == 0 && r.Renegotiations == 0 && report.OfferAnswerExchanges == 1 && report.ConnectedThroughout() && report.RemoteAddr == relayAddr && len(report.Moves) == 0
	return r
}
