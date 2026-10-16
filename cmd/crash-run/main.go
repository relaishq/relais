// crash-run is the real-process acceptance run used by the full CI matrix.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
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

	"github.com/relaishq/relais/internal/clusterprocess"
	"github.com/relaishq/relais/internal/nettopology"
	"github.com/relaishq/relais/internal/redisendpoint"
	"github.com/relaishq/relais/pkg/callharness"
)

type result struct {
	Unsupported                                      string              `json:"unsupported,omitempty"`
	StoreStall                                       *storeStallEvidence `json:"store_stall,omitempty"`
	ProfileRow                                       *profileRow         `json:"profile_row,omitempty"`
	BufferEvidence                                   *bufferEvidence
	Metrics                                          []ProcessMetrics `json:"metrics,omitempty"`
	Gap                                              time.Duration
	Decrypt, AfterResume, Reconnects, Renegotiations int
	Decoded, Live, PLI, Detection                    time.Duration
	Pass                                             bool
	Path, LivePath                                   string
	ReplayPackets                                    int
	Measurement                                      callharness.EventMeasurement
	RoutesRestored                                   uint64
	Duplicates                                       int
	OldWorkerPackets, NewWorkerPackets               uint64
	ClaimAfter, FenceTime, ActivateTime              time.Duration
	BindRestoreTime, ReadyAfter, RegisteredAfter     time.Duration
	BindWaitTime                                     time.Duration
}

// Each netns run retains route evidence, even without a profile scenario.
func recordPrivateRouteSettings(dir string, tuned bool, kernel string, routes []nettopology.PrivateRoute) error {
	report := struct {
		PrivateRouteTuning bool                       `json:"private_route_tuning"`
		KernelRelease      string                     `json:"kernel_release"`
		PrivateRoutes      []nettopology.PrivateRoute `json:"private_routes"`
	}{tuned, kernel, routes}
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "private-route-settings.json"), encoded, 0600); err != nil {
		return err
	}
	fmt.Printf("PRIVATE_ROUTE_SETTINGS %s\n", encoded)
	return nil
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
		if name == "RELAIS_MOVE_DIAGNOSTICS" || name == "RELAIS_SESSIONSTORE_KEY" || strings.HasPrefix(name, "RELAIS_REDIS_") || name == "RELAIS_PRIVATE_NETS" {
			continue
		}
		env = append(env, entry)
	}
	return append(env, "RELAIS_SESSIONSTORE_KEY="+key, "RELAIS_REDIS_ADDR="+addr, "RELAIS_REDIS_PREFIX="+prefix)
}
func trial(ctx context.Context, manager *clusterprocess.Manager, bin, dir string, env []string, after, warmup time.Duration, verbose, terminate, pauseWorker, cacheOff bool, topology *nettopology.Topology, profile *profileTrialOptions, stalls ...*storeStallOptions) (out result, trialErr error) {
	var stall *storeStallOptions
	if len(stalls) > 0 {
		stall = stalls[0]
	}
	var processes []*clusterprocess.Child
	defer func() {
		for i := len(processes) - 1; i >= 0; i-- {
			processes[i].Stop()
		}
	}()
	systemMetrics := newMetricsRun()
	var uncertainBefore float64
	var uncertainBeforeAvailable bool
	defer func() {
		out.Metrics = systemMetrics.finish(dir)
		if out.ProfileRow != nil {
			for _, m := range out.Metrics {
				if m.Process == "relay" {
					out.ProfileRow.BarrierResends = uint64(m.Values["relais_relay_barrier_resends_total"])
				}
				if m.Process == "control" {
					v, ok := m.Values["relais_control_uncertain_move_resumes_total"]
					out.ProfileRow.ObservedUncertainMoveResumes, out.ProfileRow.ObservedUncertainMoveResumesAvailable = observedUncertainResumeDelta(uncertainBefore, uncertainBeforeAvailable, v, ok)
				}
			}
		}
	}()
	start := func(name string, args ...string) (*clusterprocess.Child, clusterprocess.Ready, error) {
		childEnv := env
		if name == "relay" || name == "relay-standby" {
			childEnv = withoutKey(env)
		}
		c, err := startRole(manager, topology, dir, name, childEnv, args...)
		if err != nil {
			return nil, clusterprocess.Ready{}, fmt.Errorf("%s launch: %w", name, err)
		}
		processes = append(processes, c)
		startup, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		r, err := c.Ready(startup)
		if err != nil {
			err = fmt.Errorf("%s readiness: %w", name, err)
		} else {
			systemMetrics.add(name, r.HTTP, c)
		}
		return c, r, err
	}
	relayArgs := []string{filepath.Join(bin, "relais-relay"), "-media", roleBind(topology, "relay", true), "-leg", roleBind(topology, "relay", false), "-http", roleBind(topology, "relay", false)}
	if profile == nil {
		relayArgs = append(relayArgs, "-buffer-off")
	}
	_, relayReady, err := start("relay", relayArgs...)
	if err != nil {
		return result{}, err
	}
	if stall != nil && stall.Variant == "live" {
		standby, err := manager.Start(dir, "relay-standby", withoutKey(env), filepath.Join(bin, "relais-relay"), "-standby", "-buffer-off", "-media", relayReady.Media, "-leg", relayReady.Leg, "-http", strings.TrimPrefix(relayReady.HTTP, "http://"))
		if err != nil {
			return result{}, err
		}
		processes = append(processes, standby)
		stall.Standby = standby
		wait, stop := context.WithTimeout(ctx, 3*time.Second)
		err = standby.WaitLog(wait, "STANDBY_WAIT")
		stop()
		if err != nil {
			return result{}, err
		}
	}
	controlAddr, err := controlAddress(topology)
	if err != nil {
		return result{}, err
	}
	controlURL := "http://" + controlAddr
	workerPIDs := map[string]*clusterprocess.Child{}
	workerURLs := map[string]string{}
	var faults []*uncertainProxy
	defer func() {
		for _, f := range faults {
			f.Close()
		}
	}()
	privateAPIs := []string{strings.TrimPrefix(relayReady.HTTP, "http://"), controlAddr}
	controlArgs := []string{filepath.Join(bin, "relais-control"), "-http", controlAddr, "-relay", relayReady.HTTP}
	for _, name := range []string{"0", "1"} {
		args := []string{filepath.Join(bin, "relais-worker"), "-media", roleBind(topology, "worker-"+name, false), "-http", roleBind(topology, "worker-"+name, false), "-name", name, "-control", controlURL, "-relay-leg", relayReady.Leg, "-relay-media", relayReady.Media}
		if os.Getenv("RELAIS_SRTP_DIAGNOSTICS") == "1" {
			args = append(args, "-srtp-diagnostics")
		}
		if cacheOff {
			args = append(args, "-frame-cache-off")
		}
		c, r, err := start("worker-"+name, args...)
		if err != nil {
			return result{}, err
		}
		workerPIDs[name] = c
		workerURLs[name] = r.HTTP
		privateAPIs = append(privateAPIs, strings.TrimPrefix(r.HTTP, "http://"))
		endpoint := r.HTTP
		if profile != nil && profile.Name == "lossy-dc" {
			proxy, err := newUncertainProxy(topology.Plan.Host.String(), endpoint)
			if err != nil {
				return result{}, err
			}
			faults = append(faults, proxy)
			endpoint = proxy.URL
		}
		controlArgs = append(controlArgs, "-worker", name+"="+endpoint)
	}
	_, _, err = start("control", controlArgs...)
	if err != nil {
		return result{}, err
	}
	relayAddr, err := netip.ParseAddrPort(relayReady.Media)
	if err != nil {
		return result{}, err
	}
	external := &callharness.ExternalTopology{SignalingURL: controlURL + "/calls", RelayAddr: relayAddr}
	if topology != nil {
		if err := topology.AllowMedia(ctx, relayAddr); err != nil {
			return result{}, err
		}
		// Verify the store too, but diagnostics use only the HTTP endpoints.
		privateEndpoints := append(append([]string(nil), privateAPIs...), envValue(env, "RELAIS_REDIS_ADDR"))
		if err := topology.VerifyPrivateAPIs(ctx, privateEndpoints); err != nil {
			return result{}, err
		}
		fmt.Println("ISOLATION PASS: caller cannot reach relay-private, workers, control or store; only relay UDP media is allowed")
		external.CallerSocket = topology.CallerSocket
	}
	h, err := callharness.Start(callharness.Options{External: external})
	if err != nil {
		return result{}, err
	}
	defer func() { _ = h.Close() }()
	options := callharness.CallOptions{Video: true, Worker: -1, SRTPDiagnostics: os.Getenv("RELAIS_SRTP_DIAGNOSTICS") == "1"}
	if profile != nil {
		p, _ := nettopology.ParseProfile(profile.Name)
		options.Freshness.AbsoluteLatencyCeiling = p.FreshnessCeiling
	}
	call, err := h.Dial(ctx, options)
	if err != nil {
		return result{}, err
	}
	// These calls are continuous across SIGKILL, takeover and the consent window.
	mediaCtx, stopMedia := context.WithCancel(ctx)
	defer stopMedia()
	sent := make(chan error, 1)
	mediaDuration := after + 15*time.Second
	if stall != nil {
		mediaDuration += stall.Duration
	}
	if profile != nil {
		mediaDuration = after + 40*time.Second
	}
	go func() { sent <- call.SendMedia(mediaCtx, mediaDuration) }()
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
		return result{}, definiteProfileFailure("lost call: expected one owning call in /status")
	}
	if profile != nil && len(crashTakeovers(status.Takeovers)) > 0 {
		return result{}, definiteProfileFailure("unexpected takeover before acceptance")
	}
	owner := status.Calls[0].Owner
	worker := workerPIDs[owner]
	if worker == nil {
		return result{}, fmt.Errorf("unknown owning worker %q", owner)
	}
	if profile != nil {
		systemMetrics.sampleBeforeFault(ctx)
		uncertainBefore, uncertainBeforeAvailable = systemMetrics.value("control", "relais_control_uncertain_move_resumes_total")
	}
	if profile != nil && profile.Name == "lossy-dc" {
		// The uncertain-reply probe is separate from the acceptance event. A
		// committed resume with no usable reply must settle or roll back safely;
		// it need not claim a successful operator move. Keep its caller record.
		target := 1
		if owner == "1" {
			target = 0
		}
		faults[target].Arm()
		probeErr := profileMove(ctx, call, owner)
		fmt.Printf("UNCERTAIN_PROBE operator_error=%v\n", probeErr)
		select {
		case <-ctx.Done():
			return result{}, ctx.Err()
		case <-time.After(6 * time.Second):
		}
		status, err = h.Status(ctx)
		if err != nil {
			return result{}, fmt.Errorf("uncertainty probe status: %w", err)
		}
		if len(status.Calls) != 1 {
			return result{}, definiteProfileFailure("uncertainty probe lost call")
		}
		if len(crashTakeovers(status.Takeovers)) > 0 {
			return result{}, definiteProfileFailure("unexpected takeover before acceptance")
		}
		owner = status.Calls[0].Owner
		worker = workerPIDs[owner]
	}
	if profile != nil && profile.Name == "lossy-dc" {
		// Exercise barrier loss separately from profile timing samples. Forcing
		// a 75ms resend into every measured move would bias the named-profile p95.
		// Keep this probe and let its measurement settle before the baseline.
		if err := profileBarrierMove(ctx, topology, call, owner); err != nil {
			return result{}, err
		}
		select {
		case <-ctx.Done():
			return result{}, ctx.Err()
		case <-time.After(6 * time.Second):
		}
		status, err = h.Status(ctx)
		if err != nil {
			return result{}, fmt.Errorf("post-probe status: %w", err)
		}
		if len(status.Calls) != 1 {
			return result{}, definiteProfileFailure("barrier probe lost call")
		}
		if len(crashTakeovers(status.Takeovers)) > 0 {
			return result{}, definiteProfileFailure("unexpected takeover before acceptance")
		}
		owner = status.Calls[0].Owner
		worker = workerPIDs[owner]
	}
	systemMetrics.sampleBeforeFault(ctx)
	var diagnostics *moveDiagnostics
	var tcpBefore []nettopology.TCPSnapshot
	if profile != nil && profile.Scenario == "move" {
		tcpBefore = topology.SnapshotTCP(ctx)
	}
	var stalledAt time.Time
	if stall != nil {
		stall.SessionID, stall.Owner, stall.WorkerURL = call.SessionID(), owner, workerURLs[owner]
		if err := stall.prepare(ctx, env, call.SessionID(), controlURL); err != nil {
			return result{}, err
		}
		defer stall.close()
		if err := stall.Redis.Signal(syscall.SIGSTOP); err != nil {
			return result{}, err
		}
		defer func() { _ = stall.Redis.Signal(syscall.SIGCONT) }()
		stalledAt = time.Now()
		fmt.Printf("SIGSTOP store pid=%d session=%s stall=%s\n", stall.Redis.PID(), call.SessionID(), stall.Duration)
		select {
		case <-ctx.Done():
			return result{}, ctx.Err()
		case err := <-sent:
			senderJoined = true
			return result{}, fmt.Errorf("media ended during store stall: %v", err)
		case <-time.After(stall.Duration):
		}
	}
	killed := time.Now()
	var profileActionErr error
	if stall != nil && (stall.Variant == "live" || stall.Variant == "expired") {
		if err := stall.resume(stalledAt); err != nil {
			return result{}, err
		}
	} else if profile != nil && profile.Scenario == "move" {
		err = profileMove(ctx, call, owner)
		// Keep the caller record even on a definitive/uncertain move error. The
		// report judges it; stopping here would discard the failure's media evidence.
		if err != nil {
			profileActionErr = err
			fmt.Printf("PROFILE_MOVE_ERROR %v\n", err)
		}
		diagnostics = collectMoveDiagnostics(ctx, privateAPIs, tcpBefore, topology.SnapshotTCP(ctx))
	} else {
		signal := syscall.SIGKILL
		label := "SIGKILL"
		if pauseWorker {
			signal, label = syscall.SIGSTOP, "SIGSTOP"
		}
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
		if stall != nil {
			select {
			case <-ctx.Done():
				return result{}, ctx.Err()
			case <-worker.Done():
			}
			if err := stall.resume(stalledAt); err != nil {
				return result{}, err
			}
		}
		priorTakeovers := len(crashTakeovers(status.Takeovers))
		// Wait for a terminal takeover, then observe at least 60 s of consent in
		// the default run. The short developer mode cannot claim that acceptance.
		deadline := time.NewTimer(3 * time.Second)
		defer deadline.Stop()
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		// SIGTERM exits this wait on the first owner change, which may precede
		// move completion. The later worker-exit and media-freshness checks catch
		// a failed drain or rollback; this poll alone does not prove success.
	waitTakeover:
		for len(crashTakeovers(status.Takeovers)) == priorTakeovers && (!terminate || len(status.Calls) != 1 || status.Calls[0].Owner == owner) {
			select {
			case <-ctx.Done():
				return result{}, ctx.Err()
			case <-deadline.C:
				if stall != nil {
					stall.TimedOut = true
					break waitTakeover
				}
				return result{}, definiteProfileFailure("takeover did not complete within 3 s")
			case <-tick.C:
				status, err = h.Status(ctx)
				if err != nil {
					return result{}, err
				}
			}
		}
		if terminate {
			if len(crashTakeovers(status.Takeovers)) != 0 {
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
		} else if stall == nil || !stall.TimedOut {
			takeovers := crashTakeovers(status.Takeovers)
			takeover := takeovers[len(takeovers)-1]
			if stall == nil && (takeover.Lost || takeover.Error != "") {
				return result{}, definiteProfileFailure(fmt.Sprintf("takeover lost=%t: %s", takeover.Lost, takeover.Error))
			}
		}
	}
	if stall != nil {
		status, err = h.Status(ctx)
		if err != nil {
			stall.ObservationErrors = append(stall.ObservationErrors, "post-resume status: "+err.Error())
		}
		stall.capture(ctx, status, relayReady.HTTP)
		if stall.Variant == "expired" {
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				o := stall.Observations[len(stall.Observations)-1]
				if o.RelayRoutes == 0 && o.WorkerSessions == 0 && !o.LeaseExists && !o.SnapshotExists {
					break
				}
				select {
				case <-ctx.Done():
					return result{}, ctx.Err()
				case <-time.After(50 * time.Millisecond):
				}
				status, err = h.Status(ctx)
				if err != nil {
					return result{}, err
				}
				stall.capture(ctx, status, relayReady.HTTP)
			}
		}
	}
	observation := time.NewTimer(max(after, (callharness.FreshnessPolicy{}).EvaluationWindow()))
	defer observation.Stop()
observe:
	for {
		select {
		case <-ctx.Done():
			return result{}, ctx.Err()
		case err := <-sent:
			senderJoined = true
			if stall == nil {
				return result{}, fmt.Errorf("media ended before observation finished: %v", err)
			}
			stall.MediaError = fmt.Sprint(err)
			sent = nil // Preserve the full observation horizon after a sender failure.
		case <-observation.C:
			break observe
		}
	}
	observedUntil := time.Now()
	stopMedia()
	var mediaErr error
	if !senderJoined {
		mediaErr = <-sent
		senderJoined = true
	}
	if mediaErr != nil && !errors.Is(mediaErr, context.Canceled) {
		if stall == nil {
			return result{}, mediaErr
		}
		stall.MediaError = mediaErr.Error()
	}
	if stall != nil {
		status, err = h.Status(ctx)
		if err != nil {
			stall.ObservationErrors = append(stall.ObservationErrors, "final status: "+err.Error())
		}
		stall.capture(ctx, status, relayReady.HTTP)
	}
	report, err := call.Hangup(ctx)
	if stall != nil {
		return stall.judge(ctx, report, err, status, relayReady, stalledAt, killed, observedUntil, dir)
	}
	if err != nil {
		return result{}, err
	}
	if verbose {
		fmt.Println(report.Summary())
	}
	if profile != nil {
		row := measureProfile(report, relayReady.Media, *profile, after)
		if diagnostics != nil {
			diagnostics.selectMove(row.EventResult.MoveDiagnosticID, profileActionErr == nil && !row.EventResult.RolledBack)
			row.MoveDiagnostics = diagnostics
		}
		if profileActionErr != nil {
			row.Integrity.Failed = true
			row.Integrity.Reasons = append(row.Integrity.Reasons, "profile action error: "+profileActionErr.Error())
		}
		for _, f := range faults {
			row.InjectedDiscardedReplies += f.Count()
		}
		encoded, err := json.Marshal(report)
		if err != nil {
			return result{}, err
		}
		if err := os.WriteFile(filepath.Join(dir, "caller-report.json"), encoded, 0600); err != nil {
			return result{}, err
		}
		return result{ProfileRow: &row, Measurement: row.Measurement}, nil
	}
	if terminate {
		return measureDrain(report, relayReady.Media, observedUntil), nil
	}
	return measure(report, relayReady.Media, after), nil
}
func measure(report *callharness.Report, relayAddr string, after time.Duration) result {
	if report.HasDistributedEvidence() {
		return result{Unsupported: "local crash policy refuses distributed point estimates"}
	}
	r := result{Decrypt: report.DecryptionFailures.Total(), Reconnects: report.ICERestarts, Renegotiations: report.Renegotiations}
	if len(report.Moves) != 1 {
		return r
	}
	move := report.Moves[0]
	r.Measurement = move.Measurement
	r.AfterResume, r.Decoded, r.Live, r.Detection = move.DecryptionFailuresAfterResume, move.Recovery.FirstDecodedAfterKill, move.Recovery.FirstDecodedLiveAfterKill, move.DetectionTime
	r.Path, r.LivePath, r.ReplayPackets = move.Recovery.Path, move.Recovery.LivePath, move.Recovery.ReplayPackets
	r.PLI = move.Recovery.FirstDecodedPLIAfterKill
	flowing := len(move.Tracks) == 2
	for _, track := range move.Tracks {
		r.Gap = max(r.Gap, track.Gap)
		flowing = flowing && track.PacketsAfter > 0
	}
	video := report.Track("video")
	decoded := video != nil && video.Video != nil && video.Video.FullDecode.Ran() && video.Video.FullDecode.Errors == "" && video.Video.FullDecode.FramesDecoded == video.Video.FullDecode.FramesIn && video.Video.KeyframeDecodeErrors == 0
	attributed := (r.Path == "Cache" && r.ReplayPackets > 0 || r.Path == "Keyframe" || r.Path == "Live") && (r.LivePath == "Keyframe" || r.LivePath == "Live")
	r.Pass = r.Measurement.SettleWindow > 0 && !r.Measurement.ContentRecoveryFailed() && attributed && flowing && decoded && r.Gap > 0 && r.Gap < 2*time.Second && r.AfterResume == 0 && r.Reconnects == 0 && r.Renegotiations == 0 && report.OfferAnswerExchanges == 1 && report.ConnectedThroughout() && report.RemoteAddr == relayAddr && r.Decoded > 0 && r.Live > 0 && move.Error == "" && report.Consent.ResponsesAfter > 0
	if after >= 60*time.Second {
		r.Pass = r.Pass && report.Consent.ObservedFor >= 60*time.Second
	}
	return r
}
func printTable(results []result) {
	fmt.Println("run  gap_ms  decrypt(total/after)  reconnects  renegotiations  decoded_ms  live_ms  detection_ms  first_path  live_path  replay_packets  pli_keyframe_ms  result")
	for i, r := range results {
		fmt.Printf("CALLER_EVENT run=%d %s\n", i+1, r.Measurement.Summary())
		state := "FAIL"
		if r.Pass {
			state = "PASS"
		}
		fmt.Printf("%3d  %6.1f  %7d/%-5d       %3d        %3d          %7.1f    %7.1f  %7.1f       %-8s    %-8s    %5d           %7.1f          %s\n", i+1, float64(r.Gap)/float64(time.Millisecond), r.Decrypt, r.AfterResume, r.Reconnects, r.Renegotiations, float64(r.Decoded)/float64(time.Millisecond), float64(r.Live)/float64(time.Millisecond), float64(r.Detection)/float64(time.Millisecond), r.Path, r.LivePath, r.ReplayPackets, float64(r.PLI)/float64(time.Millisecond), state)
	}
}
func run() (err error) {
	runs := flag.Int("runs", 10, "trials per mode (cache and PLI are interleaved by default)")
	after := flag.Duration("after", 60*time.Second, "media/consent observation after takeover (60s for acceptance)")
	binArg := flag.String("bin", "bin", "built binaries and throwaway run directory")
	redisAddr := flag.String("redis", os.Getenv("RELAIS_REDIS_ADDR"), "explicit dedicated Redis address, or start a throwaway instance")
	redisBinary := flag.String("redis-server", "redis-server", "Redis executable for the throwaway instance")
	verbose := flag.Bool("verbose", false, "print full caller reports")
	bufferOn := flag.Bool("buffer", false, "run with caller SRTP buffering, using the source-checkout caller probe")
	replayLoss := flag.Bool("buffer-lossy", false, "expire the real replay gate before adoption; prove HTTP keyframe recovery")
	compareBuffer := flag.Bool("compare-buffer", false, "interleave buffer on/off with the same frame-cache mode; requires source checkout and Go")
	compare := flag.Bool("compare-cache", true, "compare Redis cache+PLI with cache-off PLI (runs per mode)")
	cacheOff := flag.Bool("frame-cache-off", false, "run PLI only; disable cache writes and replay (overrides comparison)")
	pauseWorker := flag.Bool("worker-stop", false, "SIGSTOP the owning worker; prove heartbeat takeover without waiting for lease expiry")
	terminate := flag.Bool("sigterm", false, "verify graceful owning-worker drain instead of crash takeover")
	relayStandby := flag.Bool("relay-standby", false, "fenced standby takeover after SIGKILL and SIGSTOP, ten runs each")
	topologyOption := flag.String("topology", "loopback", "loopback or netns (Linux/root only)")
	profile := flag.String("profile", "lan", "lan, region, far-caller or lossy-dc (shaping requires netns)")
	privateRouteTuning := flag.Bool("private-route-tuning", true, "rto_min 5ms and quickack on owned namespace DC routes; false measures the untuned baseline")
	scenario := flag.String("scenario", "", "profile acceptance: move or takeover; requires netns, enables buffer")
	relayRestart := flag.Bool("relay-restart", false, "SIGKILL and restart the relay mid-call; workers and caller stay alive")
	restoreOff := flag.Bool("route-restore-off", false, "disable route restore in relay-restart baseline trials")
	compareRestore := flag.Bool("compare-restore", true, "compare restore on/off in relay-restart mode")
	flag.DurationVar(&metricsInterval, "metrics-interval", time.Second, "private metrics scrape interval; 0 disables periodic scraping (pre-fault and final samples remain)")
	storeStall := flag.String("store-stall", "", "store-stall variant: moderate (1.5s), long (5s), live (5s, survival), or expired (35s, no worker kill); owns throwaway Redis, loopback only")
	stallDuration := flag.Duration("store-stall-duration", 0, "override selected store-stall duration")
	flag.Parse()
	if *pauseWorker && (*terminate || *relayRestart || *relayStandby || *storeStall != "" || *scenario != "" || *bufferOn || *compareBuffer) {
		return errors.New("worker-stop requires the plain worker takeover scenario")
	}
	if err := validateStoreStall(*storeStall, *stallDuration, *redisAddr, *topologyOption, *scenario != "" || *bufferOn || *compareBuffer || *replayLoss || *relayRestart || *relayStandby || *terminate || *cacheOff || *restoreOff); err != nil {
		return err
	}
	if *scenario != "" {
		if err := validateProfileScenario(*topologyOption, *scenario, *bufferOn || *compareBuffer || *replayLoss || *relayRestart || *relayStandby || *terminate || *cacheOff || *restoreOff); err != nil {
			return err
		}
	} else if *profile != "lan" {
		return errors.New("shaped profile acceptance requires -scenario=move or takeover")
	}
	if *replayLoss {
		*bufferOn = true
	}
	if *replayLoss && *compareBuffer {
		return errors.New("buffer-lossy cannot combine with compare-buffer")
	}
	if (*bufferOn || *compareBuffer) && (*relayRestart || *relayStandby || *terminate || *topologyOption != "loopback") {
		return errors.New("buffer comparison requires loopback and cannot combine with relay-restart, relay-standby or sigterm")
	}
	if err := validateTopologyOptions(*topologyOption, *profile, *redisAddr); err != nil {
		return err
	}
	if *relayRestart && *topologyOption != "loopback" {
		return errors.New("relay-restart supports only the loopback topology")
	}
	if *relayStandby && *topologyOption != "loopback" {
		return errors.New("relay-standby supports only the loopback topology")
	}
	if metricsInterval < 0 {
		return errors.New("metrics-interval must not be negative")
	}
	if *relayStandby && (*relayRestart || *terminate || *cacheOff || *restoreOff) {
		return errors.New("relay-standby cannot combine with relay-restart, sigterm, frame-cache-off or route-restore-off")
	}
	if *relayRestart && (*terminate || *cacheOff) {
		return errors.New("relay-restart cannot combine with sigterm or frame-cache-off")
	}
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
	var topology *nettopology.Topology
	if *topologyOption == "netns" {
		topology, err = nettopology.OpenManaged(ctx, func(format string, args ...any) { fmt.Printf(format+"\n", args...) }, manager.AddCleanup)
		if err != nil {
			return err
		}
	}
	defer func() {
		cancel()
		if topology != nil {
			err = errors.Join(err, topology.Close())
		}
	}()
	var privateRoutes []nettopology.PrivateRoute
	var kernelRelease string
	if topology != nil {
		kernelRelease, err = topology.KernelRelease(ctx)
		if err != nil {
			return err
		}
		if *privateRouteTuning {
			privateRoutes, err = topology.TunePrivateRoutes(ctx)
		} else {
			privateRoutes, err = topology.ObservePrivateRoutes(ctx)
		}
		if err != nil {
			return err
		}
		if err := recordPrivateRouteSettings(dir, *privateRouteTuning, kernelRelease, privateRoutes); err != nil {
			return err
		}
	}
	var settings []nettopology.LinkSetting
	if topology != nil && *scenario != "" {
		settings, err = topology.ApplyProfile(ctx, *profile)
		if err != nil {
			return err
		}
		encoded, err := json.MarshalIndent(settings, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, "network-settings.json"), encoded, 0600); err != nil {
			return err
		}
		fmt.Printf("PROFILE_SETTINGS profile=%s %s\n", *profile, encoded)
	}
	var bufferCaller string
	if *bufferOn || *compareBuffer {
		bufferCaller, err = buildBufferCaller(ctx, dir)
		if err != nil {
			return err
		}
	}
	addr := *redisAddr
	password := ""
	if topology != nil {
		var secret [32]byte
		if _, err := rand.Read(secret[:]); err != nil {
			return err
		}
		password = hex.EncodeToString(secret[:])
	}
	var ownedRedis *clusterprocess.Child
	if addr == "" {
		addr, err = redisAddress(topology)
		if err != nil {
			return err
		}
		_, port, _ := net.SplitHostPort(addr)
		args, removeConfig, err := redisCommand(topology, *redisBinary, port, dir, password)
		if err != nil {
			return err
		}
		defer removeConfig()
		if err := manager.AddCleanup(removeConfig); err != nil {
			return err
		}
		redis, err := startRole(manager, topology, dir, "redis", os.Environ(), args...)
		if err != nil {
			return fmt.Errorf("store launch: %w", err)
		}
		ownedRedis = redis
		defer redis.Stop()
		startup, stop := context.WithTimeout(ctx, 5*time.Second)
		err = clusterprocess.WaitTCP(startup, addr, redis)
		if err == nil && topology != nil {
			err = pingRedis(startup, addr, password)
		}
		stop()
		removeConfig()
		if err != nil {
			return fmt.Errorf("store readiness: %w (log %s)", err, filepath.Join(dir, "redis.log"))
		}
	}
	if err := redisendpoint.ValidateDev(addr); err != nil {
		return err
	}
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		return err
	}
	if *after < 60*time.Second {
		fmt.Println("DEVELOPMENT RUN: 60 s post-takeover consent acceptance NOT-RUN")
	}
	if *storeStall != "" {
		return runStoreStallTrials(ctx, manager, ownedRedis, bin, dir, hex.EncodeToString(key[:]), addr, *storeStall, *stallDuration, *runs, *after, *verbose)
	}
	if *scenario != "" {
		return runProfileTrials(ctx, manager, topology, bin, dir, hex.EncodeToString(key[:]), addr, password, profileTrialOptions{Name: *profile, Scenario: *scenario, PrivateRouteTuning: *privateRouteTuning, PrivateRoutes: privateRoutes, KernelRelease: kernelRelease}, settings, *runs, *after, *verbose)
	}
	modes := []bool{*cacheOff}
	if *compare && !*terminate && !*cacheOff {
		modes = []bool{false, true}
	}
	if *relayRestart {
		modes = []bool{*restoreOff}
		if *compareRestore && !*restoreOff {
			modes = []bool{false, true}
		}
	}
	if *relayStandby {
		modes = []bool{false, true}
	}
	if *bufferOn || *compareBuffer {
		modes = []bool{false}
		if *compareBuffer {
			modes = []bool{false, true}
		}
	}
	all := make([][]result, len(modes))
	labels := make([]string, len(modes))
	for m, off := range modes {
		if *bufferOn || *compareBuffer {
			labels[m] = "buffer-on"
			if *replayLoss {
				labels[m] = "buffer-lossy"
			}
			if off {
				labels[m] = "buffer-off"
			}
			continue
		}
		labels[m] = "redis-cache+pli"
		if off {
			labels[m] = "pli-cache-off"
		}
		if *relayRestart {
			labels[m] = "restore-on"
			if off {
				labels[m] = "restore-off"
			}
		}
		if *relayStandby {
			labels[m] = "SIGKILL"
			if off {
				labels[m] = "SIGSTOP"
			}
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
			if topology != nil {
				env = append(env, "RELAIS_PRIVATE_NETS="+topology.Plan.PrivateNets(), "RELAIS_REDIS_PASSWORD="+password)
			}
			trialCtx, stop := context.WithTimeout(ctx, *after+30*time.Second)
			// Identical phases for both modes, sampling #9's unchanged budget.
			warmup := 1550*time.Millisecond + time.Duration(i%10)*7*time.Millisecond
			var r result
			var err error
			if *bufferOn || *compareBuffer {
				r, err = bufferTrial(trialCtx, manager, bufferCaller, bin, trialDir, env, *after, warmup, off, *cacheOff, *replayLoss)
			} else if *relayStandby {
				warmup = 2250*time.Millisecond + time.Duration(i%10)*7*time.Millisecond
				r, err = relayStandbyTrial(trialCtx, manager, bin, trialDir, env, *after, warmup, *verbose, off)
			} else if *relayRestart {
				warmup = 2250*time.Millisecond + time.Duration(i%10)*7*time.Millisecond
				r, err = relayRestartTrial(trialCtx, manager, bin, trialDir, env, *after, warmup, *verbose, off)
			} else {
				r, err = trial(trialCtx, manager, bin, trialDir, env, *after, warmup, *verbose, *terminate, *pauseWorker, off, topology, nil)
			}
			stop()
			if err != nil {
				return trialFailure(i+1, label, trialDir, err)
			}
			all[m] = append(all[m], r)
			if *bufferOn || *compareBuffer {
				if off {
					printBufferComparison(nil, all[m])
				} else {
					printBufferComparison(all[m], nil)
				}
			} else if *relayStandby {
				printRelayStandbyTable(label, all[m])
			} else if *relayRestart {
				printRelayRestartTable(label, all[m])
			} else {
				printTable(all[m])
			}
			if !r.Pass {
				return trialFailure(i+1, label, trialDir, errors.New("caller-observed process handover threshold failed"))
			}
			if !*bufferOn && !*compareBuffer && !*relayStandby && !*relayRestart && !*terminate && off && !provesPLIRecovery(r) {
				return trialFailure(i+1, label, trialDir, errors.New("cache-off run did not prove PLI attribution"))
			}
		}
	}
	if *bufferOn || *compareBuffer {
		if len(all) == 2 {
			printBufferComparison(all[0], all[1])
		} else {
			printBufferComparison(all[0], nil)
		}
		for m, results := range all {
			fmt.Printf("PASS: %d/%d real-process SIGKILL trials mode=%s\n", len(results), *runs, labels[m])
		}
		return nil
	}
	if *relayStandby {
		for m, results := range all {
			fmt.Printf("PASS: %d/%d real-process relay-standby trials mode=%s\n", len(results), *runs, labels[m])
			printRelayStandbyTable(labels[m], results)
		}
		return nil
	}
	if *relayRestart {
		for m, results := range all {
			fmt.Printf("PASS: %d/%d real-process relay-restart trials mode=%s\n", len(results), *runs, labels[m])
			printRelayRestartTable(labels[m], results)
		}
		return nil
	}
	signal := "SIGKILL"
	if *pauseWorker {
		signal = "SIGSTOP"
	}
	if *terminate {
		signal = "SIGTERM drain"
	}
	for m, results := range all {
		conclusive := conclusiveEvents(results)
		if !callharness.EnoughConclusive(conclusive, len(results)) {
			return fmt.Errorf("mode=%s caller measurement coverage %d/%d below %d%%", labels[m], conclusive, len(results), callharness.MinimumConclusivePercent)
		}
		fmt.Printf("CALLER_COVERAGE mode=%s conclusive=%d/%d minimum=%d%%\n", labels[m], conclusive, len(results), callharness.MinimumConclusivePercent)
		fmt.Printf("PASS: %d/%d real-process %s trials mode=%s cache_attributed=%d/%d\n", len(results), *runs, signal, labels[m], cacheAttributions(results), len(results))
		printPathMedians(labels[m], results, signal)
	}
	if len(all) == 2 {
		printComparison(all[0], all[1], signal)
		if cacheAttributions(all[0]) == 0 {
			return errors.New("cross-process cached decode NOT-PROVEN: cache_attributed=0; every run fell back to PLI")
		}
	}
	return nil
}

func provesPLIRecovery(r result) bool {
	return r.PLI > 0 && r.ReplayPackets == 0
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
	if report.HasDistributedEvidence() {
		return result{Unsupported: "local crash policy refuses distributed point estimates"}
	}
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
func printComparison(cache, pli []result, starts ...string) {
	origin := "SIGKILL"
	if len(starts) > 0 {
		origin = starts[0]
	}
	fmt.Printf("CACHE_VS_PLI common_start=%s (cached picture predates fault)\n", origin)
	fmt.Println("run  cache_decoded_ms  cache_live_ms  cache_path  cache_live_path  pli_decoded_ms  pli_live_ms  pli_path  pli_live_path  pli_keyframe_ms")
	for i, c := range cache {
		p := pli[i]
		fmt.Printf("%3d  %16.1f  %13.1f  %-10s  %-15s  %14.1f  %11.1f  %-8s  %-13s  %15.1f\n", i+1, float64(c.Decoded)/float64(time.Millisecond), float64(c.Live)/float64(time.Millisecond), c.Path, c.LivePath, float64(p.Decoded)/float64(time.Millisecond), float64(p.Live)/float64(time.Millisecond), p.Path, p.LivePath, float64(p.PLI)/float64(time.Millisecond))
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
func printPathMedians(mode string, results []result, starts ...string) {
	origin := "SIGKILL"
	if len(starts) > 0 {
		origin = starts[0]
	}
	var pli []time.Duration
	for _, r := range results {
		if r.PLI > 0 {
			pli = append(pli, r.PLI)
		}
	}
	if len(pli) > 0 {
		fmt.Printf("PLI_MEDIAN mode=%s common_start=%s decoded_runs=%d pli_keyframe_ms=%.1f\n", mode, origin, len(pli), float64(median(pli))/float64(time.Millisecond))
	}
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

func conclusiveEvents(results []result) int {
	n := 0
	for _, result := range results {
		if result.Measurement.Conclusive() {
			n++
		}
	}
	return n
}
