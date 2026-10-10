// crash-run is the real-process acceptance run used by the full CI matrix.
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
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/relais/internal/clusterprocess"
	"github.com/relais/internal/processrun"
	"github.com/relais/pkg/callharness"
)

type result struct {
	Gap                                              time.Duration
	Decrypt, AfterResume, Reconnects, Renegotiations int
	Decoded, Live, Detection                         time.Duration
	Pass                                             bool
	Path, LivePath                                   string
	ReplayPackets                                    int
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
func trial(ctx context.Context, manager *clusterprocess.Manager, bin, dir string, env []string, after, warmup time.Duration, verbose, terminate, cacheOff bool) (result, error) {
	var processes []*clusterprocess.Child
	defer func() {
		for i := len(processes) - 1; i >= 0; i-- {
			processes[i].Stop()
		}
	}()
	start := func(name string, args ...string) (*clusterprocess.Child, clusterprocess.Ready, error) {
		childEnv := env
		if name == "relay" {
			childEnv = withoutKey(env)
		}
		c, err := manager.Start(dir, name, childEnv, args...)
		if err != nil {
			return nil, clusterprocess.Ready{}, err
		}
		processes = append(processes, c)
		startup, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		r, err := c.Ready(startup)
		return c, r, err
	}
	_, relayReady, err := start("relay", filepath.Join(bin, "relais-relay"), "-media", "127.0.0.1:0", "-leg", "127.0.0.1:0", "-http", "127.0.0.1:0")
	if err != nil {
		return result{}, err
	}
	controlAddr, err := clusterprocess.FreeTCP()
	if err != nil {
		return result{}, err
	}
	controlURL := "http://" + controlAddr
	workerPIDs := map[string]*clusterprocess.Child{}
	controlArgs := []string{filepath.Join(bin, "relais-control"), "-http", controlAddr, "-relay", relayReady.HTTP}
	for _, name := range []string{"0", "1"} {
		args := []string{filepath.Join(bin, "relais-worker"), "-media", "127.0.0.1:0", "-http", "127.0.0.1:0", "-name", name, "-control", controlURL, "-relay-leg", relayReady.Leg, "-relay-media", relayReady.Media}
		if cacheOff {
			args = append(args, "-frame-cache-off")
		}
		c, r, err := start("worker-"+name, args...)
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
	case <-time.After(warmup):
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
	if err := worker.SignalGroup(signal); err != nil {
		return result{}, err
	}
	if !terminate {
		h.RecordProcessKill(owner, killed)
	}
	fmt.Printf("%s worker=%s pid=%d session=%s relay=%s\n", label, owner, worker.PID(), call.SessionID(), relayReady.Media)
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
		case <-worker.Done():
			if worker.Err() != nil {
				return result{}, fmt.Errorf("SIGTERM worker exit: %w", worker.Err())
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
	r.Path, r.LivePath, r.ReplayPackets = move.Recovery.Path, move.Recovery.LivePath, move.Recovery.ReplayPackets
	flowing := len(move.Tracks) == 2
	for _, track := range move.Tracks {
		r.Gap = max(r.Gap, track.Gap)
		flowing = flowing && track.PacketsAfter > 0
	}
	video := report.Track("video")
	decoded := video != nil && video.Video != nil && video.Video.FullDecode.Ran() && video.Video.FullDecode.Errors == "" && video.Video.FullDecode.FramesDecoded == video.Video.FullDecode.FramesIn && video.Video.KeyframeDecodeErrors == 0
	attributed := (r.Path == "Cache" && r.ReplayPackets > 0 || r.Path == "Keyframe" || r.Path == "Live") && (r.LivePath == "Keyframe" || r.LivePath == "Live")
	r.Pass = attributed && flowing && decoded && r.Gap > 0 && r.Gap < 2*time.Second && r.AfterResume == 0 && r.Reconnects == 0 && r.Renegotiations == 0 && report.OfferAnswerExchanges == 1 && report.ConnectedThroughout() && report.RemoteAddr == relayAddr && r.Decoded > 0 && r.Live > 0 && move.Error == "" && report.Consent.ResponsesAfter > 0
	if after >= 60*time.Second {
		r.Pass = r.Pass && report.Consent.ObservedFor >= 60*time.Second
	}
	return r
}
func printTable(results []result) {
	fmt.Println("run  gap_ms  decrypt(total/after)  reconnects  renegotiations  decoded_ms  live_ms  detection_ms  first_path  live_path  replay_packets  result")
	for i, r := range results {
		state := "FAIL"
		if r.Pass {
			state = "PASS"
		}
		fmt.Printf("%3d  %6.1f  %7d/%-5d       %3d        %3d          %7.1f    %7.1f  %7.1f       %-8s    %-8s    %5d           %s\n", i+1, float64(r.Gap)/float64(time.Millisecond), r.Decrypt, r.AfterResume, r.Reconnects, r.Renegotiations, float64(r.Decoded)/float64(time.Millisecond), float64(r.Live)/float64(time.Millisecond), float64(r.Detection)/float64(time.Millisecond), r.Path, r.LivePath, r.ReplayPackets, state)
	}
}
func run() error {
	runs := flag.Int("runs", 10, "trials per mode (cache and PLI are interleaved by default)")
	after := flag.Duration("after", 60*time.Second, "media/consent observation after takeover (60s for acceptance)")
	binArg := flag.String("bin", "bin", "built binaries and throwaway run directory")
	redisAddr := flag.String("redis", os.Getenv("RELAIS_REDIS_ADDR"), "explicit dedicated Redis address, or start a throwaway instance")
	redisBinary := flag.String("redis-server", "redis-server", "Redis executable for the throwaway instance")
	verbose := flag.Bool("verbose", false, "print full caller reports")
	compare := flag.Bool("compare-cache", true, "compare Redis cache+PLI with cache-off PLI (runs per mode)")
	cacheOff := flag.Bool("frame-cache-off", false, "run PLI only; disable cache writes and replay (overrides comparison)")
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
	manager := &clusterprocess.Manager{}
	ctx, cancel := manager.Context()
	defer cancel()
	addr := *redisAddr
	if addr == "" {
		addr, err = clusterprocess.FreeTCP()
		if err != nil {
			return err
		}
		_, port, _ := net.SplitHostPort(addr)
		redis, err := manager.Start(dir, "redis", os.Environ(), *redisBinary, "--bind", "127.0.0.1", "--port", port, "--save", "", "--appendonly", "no", "--dir", dir)
		if err != nil {
			return err
		}
		defer redis.Stop()
		startup, stop := context.WithTimeout(ctx, 5*time.Second)
		err = clusterprocess.WaitTCP(startup, addr, redis)
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
	modes := []bool{*cacheOff}
	if *compare && !*terminate && !*cacheOff {
		modes = []bool{false, true}
	}
	all := make([][]result, len(modes))
	labels := make([]string, len(modes))
	for m, off := range modes {
		labels[m] = "redis-cache+pli"
		if off {
			labels[m] = "pli-cache-off"
		}
	}
	for i := 0; i < *runs; i++ {
		for m, off := range modes {
			label := labels[m]
			fmt.Printf("TRIAL %d MODE %s\n", i+1, label)
			trialDir := filepath.Join(dir, fmt.Sprintf("%s-%02d", label, i+1))
			if err := os.Mkdir(trialDir, 0700); err != nil {
				return err
			}
			env := processEnv(hex.EncodeToString(key[:]), addr, fmt.Sprintf("relais:crash:%s:%s:%d:", filepath.Base(dir), label, i))
			trialCtx, stop := context.WithTimeout(ctx, *after+30*time.Second)
			// Identical phases for both modes, sampling #9's unchanged budget.
			warmup := 1550*time.Millisecond + time.Duration(i%10)*7*time.Millisecond
			r, err := trial(trialCtx, manager, bin, trialDir, env, *after, warmup, *verbose, *terminate, off)
			stop()
			all[m] = append(all[m], r)
			printTable(all[m])
			if err != nil {
				return err
			}
			if !r.Pass {
				return errors.New("caller-observed process handover threshold failed")
			}
			if !*terminate && off && (r.Path != "Keyframe" || r.ReplayPackets != 0) {
				return errors.New("cache-off run did not prove PLI attribution")
			}
		}
	}
	signal := "SIGKILL"
	if *terminate {
		signal = "SIGTERM drain"
	}
	for m, results := range all {
		fmt.Printf("PASS: %d/%d real-process %s trials mode=%s cache_attributed=%d/%d\n", len(results), *runs, signal, labels[m], cacheAttributions(results), len(results))
		printPathMedians(labels[m], results)
	}
	if len(all) == 2 {
		printComparison(all[0], all[1])
		if cacheAttributions(all[0]) == 0 {
			return errors.New("cross-process cached decode NOT-PROVEN: cache_attributed=0; every run fell back to PLI")
		}
	}
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

// All durations share SIGKILL as their origin. Cache frames restore old pixels;
// only live_ms measures a frame sent by the caller after that kill.
func printComparison(cache, pli []result) {
	fmt.Println("CACHE_VS_PLI common_start=SIGKILL (cached picture predates crash)")
	fmt.Println("run  cache_decoded_ms  cache_live_ms  cache_path  cache_live_path  pli_decoded_ms  pli_live_ms  pli_path")
	for i, c := range cache {
		p := pli[i]
		fmt.Printf("%3d  %16.1f  %13.1f  %-10s  %-15s  %14.1f  %11.1f  %s\n", i+1, float64(c.Decoded)/float64(time.Millisecond), float64(c.Live)/float64(time.Millisecond), c.Path, c.LivePath, float64(p.Decoded)/float64(time.Millisecond), float64(p.Live)/float64(time.Millisecond), p.Path)
	}
}

func cacheAttributions(results []result) int {
	n := 0
	for _, r := range results {
		if r.Path == "Cache" && r.ReplayPackets > 0 {
			n++
		}
	}
	return n
}
func median(values []time.Duration) time.Duration {
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	n := len(values)
	if n%2 == 1 {
		return values[n/2]
	}
	return values[n/2-1] + (values[n/2]-values[n/2-1])/2
}
func printPathMedians(mode string, results []result) {
	for _, path := range []string{"Cache", "Keyframe", "Live"} {
		var decoded, live []time.Duration
		for _, r := range results {
			if r.Path == path {
				decoded = append(decoded, r.Decoded)
			}
		}
		for _, r := range results {
			if r.LivePath == path {
				live = append(live, r.Live)
			}
		}
		if len(decoded) > 0 {
			fmt.Printf("PATH_MEDIAN mode=%s path=%s decoded_runs=%d decoded_ms=%.1f\n", mode, path, len(decoded), float64(median(decoded))/float64(time.Millisecond))
		}
		if len(live) > 0 {
			fmt.Printf("PATH_MEDIAN mode=%s path=%s live_runs=%d live_ms=%.1f\n", mode, path, len(live), float64(median(live))/float64(time.Millisecond))
		}
	}
}
