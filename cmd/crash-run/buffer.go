package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/relaishq/relais/internal/clusterprocess"
)

type bufferEvidence struct {
	AudioSent, AudioReturned, AudioLost   int
	VideoSent, VideoReturned, VideoLost   int
	FirstContent                          string
	FirstContentAfterKill                 time.Duration
	RelayReplayPackets                    int
	RelayReplayDuration                   time.Duration
	Complete, GateReleased                bool
	RecoveryRequests                      int
	UniqueDecodedFrames, KeyframeRequests int
}

type bufferProcessConfig struct {
	Dir, SignalingURL, RelayHTTP, RelayMedia, KillURL string
	After, Warmup                                     time.Duration
	BufferOff                                         bool
	ReplayLoss                                        bool
}

// The temporary caller executable uses the existing exact packet and frame
// observations in new harness tests. This avoids changing #32's recorder or
// exposing test observation APIs in the production harness. Build it once for
// all trials. Buffer comparison requires this source checkout and the Go tool.
func buildBufferCaller(ctx context.Context, dir string) (string, error) {
	path := filepath.Join(dir, "buffer-caller.test")
	cmd := exec.CommandContext(ctx, "go", "test", "-c", "-o", path, "./pkg/callharness")
	log, err := os.Create(filepath.Join(dir, "caller-build.log"))
	if err != nil {
		return "", err
	}
	defer func() { _ = log.Close() }()
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("build buffered caller (run from the source checkout): %w; see %s", err, log.Name())
	}
	return path, nil
}

func bufferTrial(ctx context.Context, manager *clusterprocess.Manager, caller, bin, dir string, env []string, after, warmup time.Duration, off, cacheOff, replayLoss bool) (out result, trialErr error) {
	// Keep all service children in the driver's manager, including when the
	// caller test crashes or times out before its test cleanups can run.
	var services []*clusterprocess.Child
	defer func() {
		for i := len(services) - 1; i >= 0; i-- {
			services[i].Stop()
		}
	}()
	systemMetrics := newMetricsRun()
	var recoveryRequests atomic.Int32
	defer func() {
		out.Metrics = systemMetrics.finish(dir)
		if out.BufferEvidence != nil {
			out.BufferEvidence.RecoveryRequests = int(recoveryRequests.Load())
		}
		if trialErr == nil {
			trialErr = validateBufferMetrics(out, off, replayLoss)
		}
	}()
	start := func(name string, args ...string) (*clusterprocess.Child, clusterprocess.Ready, error) {
		childEnv := env
		if name == "relay" {
			childEnv = withoutKey(env)
		}
		child, err := manager.Start(dir, name, childEnv, args...)
		if err != nil {
			return nil, clusterprocess.Ready{}, err
		}
		services = append(services, child)
		readyCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		ready, err := child.Ready(readyCtx)
		if err == nil {
			systemMetrics.add(name, ready.HTTP, child)
		}
		return child, ready, err
	}
	args := []string{filepath.Join(bin, "relais-relay"), "-media", "127.0.0.1:0", "-leg", "127.0.0.1:0", "-http", "127.0.0.1:0"}
	if replayLoss {
		args = append(args, "-hold-timeout", "150ms")
	}
	if off {
		args = append(args, "-buffer-off")
	}
	_, relayReady, err := start("relay", args...)
	if err != nil {
		return result{}, err
	}
	controlAddr, err := clusterprocess.FreeTCP()
	if err != nil {
		return result{}, err
	}
	controlURL := "http://" + controlAddr
	workers := map[string]*clusterprocess.Child{}
	controlArgs := []string{filepath.Join(bin, "relais-control"), "-http", controlAddr, "-relay", relayReady.HTTP}
	for _, name := range []string{"0", "1"} {
		args := []string{filepath.Join(bin, "relais-worker"), "-media", "127.0.0.1:0", "-http", "127.0.0.1:0", "-name", name, "-control", controlURL, "-relay-leg", relayReady.Leg, "-relay-media", relayReady.Media}
		if cacheOff {
			args = append(args, "-frame-cache-off")
		}
		child, ready, err := start("worker-"+name, args...)
		if err != nil {
			return result{}, err
		}
		workers[name] = child
		workerURL := ready.HTTP
		if replayLoss {
			target, err := url.Parse(workerURL)
			if err != nil {
				return result{}, err
			}
			proxy := httputil.NewSingleHostReverseProxy(target)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				// Delay real adoption past the gate deadline. Resume was already told
				// that the plan was complete, so recovery must use the HTTP request.
				if strings.HasSuffix(req.URL.Path, "/resume") {
					time.Sleep(250 * time.Millisecond)
				}
				if strings.HasSuffix(req.URL.Path, "/keyframe") {
					recoveryRequests.Add(1)
				}
				proxy.ServeHTTP(w, req)
			}))
			defer server.Close()
			workerURL = server.URL
		}
		controlArgs = append(controlArgs, "-worker", name+"="+workerURL)
	}
	if _, _, err := start("control", controlArgs...); err != nil {
		return result{}, err
	}
	// Only this runner can signal its owned children. The caller requests the
	// current owner by registration name, never by an unguarded raw PID.
	mux := http.NewServeMux()
	mux.HandleFunc("POST /kill/{owner}", func(w http.ResponseWriter, req *http.Request) {
		worker := workers[req.PathValue("owner")]
		if worker == nil {
			http.Error(w, "unknown owned worker", http.StatusNotFound)
			return
		}
		systemMetrics.sampleBeforeFault(req.Context())
		at := time.Now()
		if err := worker.SignalGroup(syscall.SIGKILL); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			At  time.Time
			PID int
		}{at, worker.PID()})
	})
	killServer := httptest.NewServer(mux)
	defer killServer.Close()
	cfg := bufferProcessConfig{Dir: dir, SignalingURL: controlURL + "/calls", RelayHTTP: relayReady.HTTP, RelayMedia: relayReady.Media, KillURL: killServer.URL, After: after, Warmup: warmup, BufferOff: off, ReplayLoss: replayLoss}
	data, err := json.Marshal(cfg)
	if err != nil {
		return result{}, err
	}
	configPath := filepath.Join(dir, "caller-config.json")
	if err := os.WriteFile(configPath, data, 0600); err != nil {
		return result{}, err
	}
	env = append(env, "RELAIS_BUFFER_PROCESS_CONFIG="+configPath)
	child, err := manager.Start(dir, "caller", env, caller, "-test.run=^TestAcceptanceRelayBufferProcessCaller$", "-test.v", "-test.timeout="+(after+25*time.Second).String())
	if err != nil {
		return result{}, err
	}
	defer child.Stop()
	select {
	case <-ctx.Done():
		return result{}, ctx.Err()
	case <-child.Done():
		if err := child.Err(); err != nil {
			return result{}, fmt.Errorf("buffered caller: %w; see %s", err, filepath.Join(dir, "caller.log"))
		}
	}
	data, err = os.ReadFile(filepath.Join(dir, "result.json"))
	if err != nil {
		return result{}, err
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return result{}, err
	}
	if out.BufferEvidence == nil {
		return result{}, fmt.Errorf("buffered caller supplied no buffer evidence")
	}
	return out, nil
}

func printBufferComparison(on, off []result) {
	fmt.Println("BUFFER_VS_BASELINE profile=lan common_start=SIGKILL phase1_median_ms=416 measurement=caller_loss_content_and_freshness")
	fmt.Println("run mode gap_ms pause_vs_phase1_ms decrypt_total/after audio_returned/sent/lost video_returned/sent/lost first_content first_content_ms keyframe_requests relay_replay_packets gate_released lost_audio_ms lost_video_frames first_new_content audio_fresh_ms video_fresh_ms recovery_http result")
	printRows := func(mode string, results []result) {
		for i, r := range results {
			e := r.BufferEvidence
			if e == nil {
				continue
			}
			state := "FAIL"
			if r.Pass {
				state = "PASS"
			}
			rowMode := mode
			if e.RecoveryRequests > 0 && !e.Complete {
				rowMode = "buffer-lossy"
			}
			fmt.Printf("%d %s %.1f %+.1f %d/%d %d/%d/%d %d/%d/%d %s %.1f %d %d %t %.1f %d %q %.1f %.1f %d %s\n", i+1, rowMode, float64(r.Gap)/float64(time.Millisecond), float64(r.Gap-416*time.Millisecond)/float64(time.Millisecond), r.Decrypt, r.AfterResume, e.AudioReturned, e.AudioSent, e.AudioLost, e.VideoReturned, e.VideoSent, e.VideoLost, e.FirstContent, float64(e.FirstContentAfterKill)/float64(time.Millisecond), e.KeyframeRequests, e.RelayReplayPackets, e.GateReleased, float64(r.Measurement.LostAudio)/float64(time.Millisecond), r.Measurement.LostVideoFrames, r.Measurement.FirstNewContent, float64(r.Measurement.Audio.BackToBaseline)/float64(time.Millisecond), float64(r.Measurement.Video.BackToBaseline)/float64(time.Millisecond), e.RecoveryRequests, state)
		}
	}
	printRows("buffer-on", on)
	printRows("buffer-off", off)
	if len(on) > 0 {
		gaps := make([]time.Duration, 0, len(on))
		for _, r := range on {
			gaps = append(gaps, r.Gap)
		}
		fmt.Printf("BUFFER_PAUSE_MEDIAN gap_ms=%.1f phase1_median_ms=416 delta_ms=%+.1f runs=%d\n", float64(median(gaps))/float64(time.Millisecond), float64(median(gaps)-416*time.Millisecond)/float64(time.Millisecond), len(on))
	}
}

// These are the process topology's actual private collector samples. Requiring
// both ring peaks and lifetime replay/loss counters avoids a presence-only check.
func validateBufferMetrics(out result, off, replayLoss bool) error {
	surviving := 0
	for _, process := range out.Metrics {
		if !strings.HasPrefix(process.Process, "worker-") || process.Exited {
			continue
		}
		surviving++
		value, present := process.Values["relais_worker_decryption_failures_total"]
		if process.Samples == 0 || process.Error != "" || !present {
			return errors.New("surviving worker decryption metrics unavailable")
		}
		if value != 0 {
			return fmt.Errorf("surviving worker %s recorded decryption failures: %g", process.Process, value)
		}
		// Replay-window rejections were decryption failures before ADR 0009.
		if replays := process.Values["relais_worker_replay_rejections_total"]; replays != 0 {
			return fmt.Errorf("surviving worker %s recorded replay rejections: %g", process.Process, replays)
		}
	}
	if surviving != 1 {
		return fmt.Errorf("expected one surviving worker, got %d", surviving)
	}

	for _, process := range out.Metrics {
		if process.Process != "relay" {
			continue
		}
		if process.Samples == 0 || process.Error != "" {
			return errors.New("relay buffer metrics scrape failed")
		}
		for _, name := range []string{"buffer_sessions", "buffer_packets", "buffer_bytes", "buffer_drops_total", "buffer_expired_packets_total", "buffer_untracked_total", "replays_total", "replay_packets_total", "replay_filtered_packets_total", "replay_drops_total", "replay_send_failures_total", "replay_expiries_total"} {
			if _, ok := process.Values["relais_relay_"+name]; !ok {
				return fmt.Errorf("relay buffer metric missing: %s", name)
			}
		}
		if off {
			return nil
		}
		if process.GaugePeaks["relais_relay_buffer_packets"] <= 0 || process.GaugePeaks["relais_relay_buffer_bytes"] <= 0 {
			return errors.New("relay metrics did not observe occupied ring")
		}
		if replayLoss {
			if process.Values["relais_relay_replay_drops_total"] <= 0 || process.Values["relais_relay_replay_expiries_total"] <= 0 || out.BufferEvidence.RecoveryRequests == 0 {
				return errors.New("lossy process replay did not prove counted expiry and HTTP keyframe recovery")
			}
		} else if process.Values["relais_relay_replays_total"] <= 0 || process.Values["relais_relay_replay_packets_total"] <= 0 {
			return errors.New("relay metrics did not observe replay")
		}
		return nil
	}
	return errors.New("relay buffer process metrics absent")
}
