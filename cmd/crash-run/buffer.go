package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/relais/internal/clusterprocess"
)

type bufferEvidence struct {
	AudioSent, AudioReturned, AudioLost   int
	VideoSent, VideoReturned, VideoLost   int
	FirstContent                          string
	FirstContentAfterKill                 time.Duration
	RelayReplayPackets                    int
	RelayReplayDuration                   time.Duration
	Complete, GateReleased                bool
	UniqueDecodedFrames, KeyframeRequests int
}

type bufferProcessConfig struct {
	Dir, SignalingURL, RelayHTTP, RelayMedia, KillURL string
	After, Warmup                                     time.Duration
	BufferOff                                         bool
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

func bufferTrial(ctx context.Context, manager *clusterprocess.Manager, caller, bin, dir string, env []string, after, warmup time.Duration, off, cacheOff bool) (result, error) {
	// Keep all service children in the driver's manager, including when the
	// caller test crashes or times out before its test cleanups can run.
	var services []*clusterprocess.Child
	defer func() {
		for i := len(services) - 1; i >= 0; i-- {
			services[i].Stop()
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
		return child, ready, err
	}
	args := []string{filepath.Join(bin, "relais-relay"), "-media", "127.0.0.1:0", "-leg", "127.0.0.1:0", "-http", "127.0.0.1:0"}
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
		controlArgs = append(controlArgs, "-worker", name+"="+ready.HTTP)
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
	cfg := bufferProcessConfig{Dir: dir, SignalingURL: controlURL + "/calls", RelayHTTP: relayReady.HTTP, RelayMedia: relayReady.Media, KillURL: killServer.URL, After: after, Warmup: warmup, BufferOff: off}
	data, err := json.Marshal(cfg)
	if err != nil {
		return result{}, err
	}
	configPath := filepath.Join(dir, "caller-config.json")
	if err := os.WriteFile(configPath, data, 0600); err != nil {
		return result{}, err
	}
	env = append(env, "RELAIS_BUFFER_PROCESS_CONFIG="+configPath)
	child, err := manager.Start(dir, "caller", env, caller, "-test.run=^TestRelayBufferProcessCaller$", "-test.v", "-test.timeout="+(after+25*time.Second).String())
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
	var r result
	if err := json.Unmarshal(data, &r); err != nil {
		return result{}, err
	}
	if r.BufferEvidence == nil {
		return result{}, fmt.Errorf("buffered caller supplied no buffer evidence")
	}
	return r, nil
}

func printBufferComparison(on, off []result) {
	fmt.Println("BUFFER_VS_BASELINE profile=lan common_start=SIGKILL phase1_median_ms=416 measurement=exact_RTP_packet_return (#32 lost-content/freshness yardstick pending)")
	fmt.Println("run mode gap_ms pause_vs_phase1_ms decrypt_total/after audio_returned/sent/lost video_returned/sent/lost first_content first_content_ms keyframe_requests relay_replay_packets gate_released result")
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
			fmt.Printf("%d %s %.1f %+.1f %d/%d %d/%d/%d %d/%d/%d %s %.1f %d %d %t %s\n", i+1, mode, float64(r.Gap)/float64(time.Millisecond), float64(r.Gap-416*time.Millisecond)/float64(time.Millisecond), r.Decrypt, r.AfterResume, e.AudioReturned, e.AudioSent, e.AudioLost, e.VideoReturned, e.VideoSent, e.VideoLost, e.FirstContent, float64(e.FirstContentAfterKill)/float64(time.Millisecond), e.KeyframeRequests, e.RelayReplayPackets, e.GateReleased, state)
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
