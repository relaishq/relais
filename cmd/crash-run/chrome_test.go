//go:build !windows

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/relaishq/relais/internal/clusterprocess"
	"github.com/relaishq/relais/internal/relaytrace"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/relaishq/relais/pkg/storage"
	"github.com/stretchr/testify/require"
)

// TestChromeRelayChange is opt-in; it owns an isolated Chrome and uses the
// same supervised relay processes as the crash-run relay-loss trial.
func TestChromeRelayChange(t *testing.T) {
	binary := os.Getenv("RELAIS_CHROME")
	if binary == "" {
		t.Skip("NOT-RUN: set RELAIS_CHROME to an explicit Chrome binary")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	dir := t.TempDir()
	manager := &clusterprocess.Manager{}
	defer manager.Stop(false)
	redisBinary, err := findChromeRedis()
	require.NoError(t, err)
	addr, err := chromeRedisAddress()
	require.NoError(t, err)
	_, port, _ := net.SplitHostPort(addr)
	child, err := manager.Start(dir, "redis", os.Environ(), redisBinary, "--bind", "127.0.0.1", "--port", port, "--save", "", "--appendonly", "no", "--dir", dir)
	require.NoError(t, err)
	require.NoError(t, child.WaitLog(ctx, "Ready to accept connections"))
	require.NoError(t, lossWait(ctx, func() bool {
		select {
		case <-child.Done():
			return false
		default:
		}
		return pingRedis(ctx, addr, "") == nil
	}))
	keyHex := "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	store, err := sessionstore.NewRedis(ctx, storage.RedisConfig{Addr: addr, Prefix: "relais:chrome:"}, key)
	require.NoError(t, err)
	defer store.Close()
	env := withoutKey(processEnv(keyHex, addr, "relais:chrome:"))
	bin, err := filepath.Abs("../../bin")
	require.NoError(t, err)
	var bindings []controlplane.RelayBinding
	var first clusterprocess.Ready
	var active *clusterprocess.Child
	for _, id := range []string{"r1", "r2"} {
		c, e := manager.Start(dir, id, env, filepath.Join(bin, "relais-relay"), "-buffer-off", "-relay-lease", id, "-relay-slot", id, "-media", "127.0.0.1:0", "-leg", "127.0.0.1:0", "-http", "127.0.0.1:0")
		require.NoError(t, e)
		ready, e := c.Ready(ctx)
		require.NoError(t, e)
		if id == "r1" {
			active, first = c, ready
		}
		bindings = append(bindings, controlplane.RelayBinding{Slot: mediaworker.RelaySlot{ID: id, Leg: netip.MustParseAddrPort(ready.Leg), Public: netip.MustParseAddrPort(ready.Media)}, Relay: &controlplane.RemoteRelay{URL: ready.HTTP}})
	}
	standby, err := manager.Start(dir, "standby", env, filepath.Join(bin, "relais-relay"), "-standby", "-buffer-off", "-relay-lease", "r1", "-relay-slot", "r1", "-media", first.Media, "-leg", first.Leg, "-http", first.HTTP[len("http://"):])
	require.NoError(t, err)
	worker, err := mediaworker.New(mediaworker.Config{Relay: &mediaworker.RelayConfig{Owners: store, Addr: bindings[0].Slot.Leg, PublicAddr: bindings[0].Slot.Public}})
	require.NoError(t, err)
	defer worker.Close()
	// Keep only the first event of each kind/path per trial. Worker-check is
	// authenticated STUN arrival; media events confirm actual worker traffic.
	var traceMu sync.Mutex
	var traceStart time.Time
	var serverTimeline []chromeServerEvent
	seenTrace := make(map[string]bool)
	stopTrace := relaytrace.Observe(func(at time.Time, name, path string, _ []byte) {
		switch name {
		case "worker-answer", "worker-check", "use-candidate", "worker-switch", "worker-input", "worker-output", "worker-reject":
		default:
			return
		}
		traceMu.Lock()
		defer traceMu.Unlock()
		key := name + ":" + path
		if traceStart.IsZero() || seenTrace[key] {
			return
		}
		seenTrace[key] = true
		serverTimeline = append(serverTimeline, chromeServerEvent{Ms: float64(at.Sub(traceStart)) / float64(time.Millisecond), Event: name, Path: path})
	})
	defer stopTrace()
	plane := controlplane.New(bindings[0].Relay, store)
	require.NoError(t, plane.Register("0", worker.LocalAddr(), worker))
	for _, binding := range bindings {
		require.NoError(t, binding.Relay.(*controlplane.RemoteRelay).AddWorker(ctx, worker.LocalAddr()))
	}
	require.NoError(t, plane.ConfigureRelaySet(ctx, bindings))
	life, stop := context.WithCancel(ctx)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- plane.Run(life) }()
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
				_ = plane.Heartbeat(worker.LocalAddr())
			}
		}
	}()
	defer func() { stop(); <-heartbeatDone }()
	script, err := os.ReadFile("chrome_page.js")
	require.NoError(t, err)
	recovery, err := os.ReadFile("../relais-demo/web/relay-change.js")
	require.NoError(t, err)
	mux := http.NewServeMux()
	mux.Handle("/calls", plane.Handler())
	mux.Handle("/calls/", plane.Handler())
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = fmt.Fprintf(w, "<!doctype html><body><script>%s\n//# sourceURL=relais-relay-change.js\n</script><script>%s</script>", recovery, script)
	})
	server := chromeHTTPServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/calls" {
			t.Logf("CHROME_HTTP call-create proto=%s", r.Proto)
		}
		mux.ServeHTTP(w, r)
	}))
	defer server.Close()
	// The isolated httptest TLS origin enables HTTP/2 so eight SSE streams do
	// not exhaust Chrome's HTTP/1.1 connection pool. Trust its test certificate
	// only inside this fresh, disposable browser profile.
	options := append(chromedp.DefaultExecAllocatorOptions[:], chromedp.ExecPath(binary), chromedp.UserDataDir(filepath.Join(dir, "profile")), chromedp.Flag("use-fake-device-for-media-stream", true), chromedp.Flag("use-fake-ui-for-media-stream", true), chromedp.Flag("autoplay-policy", "no-user-gesture-required"), chromedp.Flag("ignore-certificate-errors", true), chromedp.Flag("remote-debugging-port", "0"))
	alloc, allocCancel := chromedp.NewExecAllocator(ctx, options...)
	defer allocCancel()
	browser, browserCancel := chromedp.NewContext(alloc)
	defer browserCancel()
	require.NoError(t, chromedp.Run(browser, chromedp.Navigate(server.URL)))
	defer func() {
		var evidence json.RawMessage
		if err := chromedp.Run(browser, chromedp.Evaluate(`({error:chromeGate.error,failure:chromeGate.failure,calls:chromeGate.calls.map(c=>({path:c.path,phase:c.phase,phases:c.phases,timeline:c.timeline,failure:c.failure,ended:c.ended,reconnects:c.reconnects,samples:c.samples}))})`, &evidence)); err == nil {
			t.Logf("CHROME_EVIDENCE %s", evidence)
		} else {
			t.Logf("CHROME_EVIDENCE_ERROR %v", err)
		}
	}()
	read := func(since float64) chromeCall {
		var call chromeCall
		started := time.Now()
		require.NoError(t, chromedp.Run(browser, chromedp.Evaluate(fmt.Sprintf(`chromeGate.snapshot(0,%f)`, since), &call)))
		t.Logf("CHROME_READ snapshot_ms=%.1f samples=%d transfer_ms=%.1f", call.At, len(call.Samples), float64(time.Since(started))/float64(time.Millisecond))
		return call
	}
	require.NoError(t, lossWait(ctx, func() bool {
		var ready bool
		require.NoError(t, chromedp.Run(browser, chromedp.Evaluate(`!!chromeGate.error || !!chromeGate.calls[0].ended || chromeGate.calls[0].samples.length>20`, &ready)))
		return ready
	}))
	initial := read(0)
	require.Empty(t, initial.Error)
	require.Empty(t, initial.Ended)
	id := strings.TrimPrefix(initial.Path, "/calls/")
	_, assignment, err := store.RelayOwner(ctx, id)
	require.NoError(t, err)
	// A same-run three-second baseline. Each event contributes its largest
	// observed pause per track, so pooling cannot hide a later stall in a window.
	t.Log("CHROME_PHASE baseline")
	baselineStart := chromeNow(t, browser)
	chromeWait(t, browser, 3*time.Second)
	baseline := chromeMeasure(read(baselineStart), baselineStart)
	t.Logf("CHROME_BASELINE %s", chromeJSON(baseline))
	var decryptNotice sync.Once
	checkDecrypt := func(m chromeMeasurement) {
		require.Zero(t, m.Decrypt)
		if !m.DecryptExposed {
			decryptNotice.Do(func() { t.Log("NOT-RUN row=7 Chrome inbound-rtp decrypt counter not exposed") })
		}
	}
	checkDecrypt(baseline)
	var planned []chromeMeasurement
	for range 10 {
		target := "r1"
		if assignment.Slot == target {
			target = "r2"
		}
		t.Logf("CHROME_PHASE planned-change index=%d target=%s", len(planned)+1, target)
		begin := chromeNow(t, browser)
		traceMu.Lock()
		traceStart = time.Now()
		serverTimeline = nil
		seenTrace = make(map[string]bool)
		traceMu.Unlock()
		require.NoError(t, plane.ChangeRelay(ctx, id, target))
		switched := chromeNow(t, browser)
		chromeWait(t, browser, 2300*time.Millisecond)
		call := read(begin)
		m := chromeMeasure(call, begin)
		checkDecrypt(m)
		planned = append(planned, m)
		t.Logf("CHROME_BROWSER_TIMELINE index=%d begin_ms=%.1f events=%s", len(planned), begin, chromeJSON(call.Timeline))
		traceMu.Lock()
		t.Logf("CHROME_SERVER_TIMELINE index=%d origin=before-ChangeRelay events=%s", len(planned), chromeJSON(serverTimeline))
		traceMu.Unlock()
		t.Logf("CHROME_SWITCH row=1 begin_ms=%.1f committed_ms=%.1f evidence=%s", begin, switched, chromeJSON(m))
		require.Empty(t, call.Ended)
		require.Zero(t, call.Reconnects)
		require.NotNil(t, m.After)
		require.Equal(t, bindings[map[string]int{"r1": 0, "r2": 1}[target]].Slot.Public.String(), m.After.Remote)
		require.True(t, chromePostProgress(call, switched, m.After.Remote), "require two seconds of advancing video framesDecoded, audio packetsReceived and receiver synchronization-source RTP timestamp on the selected target pair")
		_, assignment, err = store.RelayOwner(ctx, id)
		require.NoError(t, err)
	}
	chromeTable(t, "1-planned", planned)
	for track, base := range []float64{baseline.AudioGap, baseline.VideoGap} {
		values := make([]float64, len(planned))
		for i, m := range planned {
			values[i] = m.AudioGap
			if track == 1 {
				values[i] = m.VideoGap
			}
		}
		require.LessOrEqual(t, chromePercentile(values, 95), math.Min(150, base+50))
	}
	// Put eight live calls on the slot that will lose active and standby.
	setupCtx, setupCancel := context.WithTimeout(browser, 30*time.Second)
	defer setupCancel()
	for assignment.Slot != "r1" {
		require.NoError(t, plane.ChangeRelay(setupCtx, id, "r1"), "row 2 setup: place initial call on r1 within 30s")
		_, assignment, err = store.RelayOwner(setupCtx, id)
		require.NoError(t, err)
	}
	for index := 1; index < 8; index++ {
		t.Logf("CHROME_ADD_CALL start call=%d/8 setup_budget=30s", index+1)
		err := chromedp.Run(setupCtx, chromedp.Evaluate(`void chromeGate.addCall().catch(()=>{})`, nil))
		var state chromeSetupState
		if err == nil {
			var pollErr error
			loggedStart := false
			err = lossWait(setupCtx, func() bool {
				pollErr = chromedp.Run(setupCtx, chromedp.Evaluate(fmt.Sprintf(`chromeGate.setupState(%d)`, index), &state))
				if pollErr == nil && !loggedStart {
					t.Logf("CHROME_ADD_CALL started call=%d/8 state=%s", index+1, chromeJSON(state))
					loggedStart = true
				}
				return pollErr != nil || state.Ready || state.Ended != ""
			})
			if err == nil {
				err = pollErr
			}
		}
		// Use the still-live parent browser for final diagnostics if setup timed
		// out. Do not consume the whole test budget waiting on an unresolved JS promise.
		if err != nil {
			diagnosticCtx, diagnosticCancel := context.WithTimeout(browser, 2*time.Second)
			var states json.RawMessage
			diagnosticErr := chromedp.Run(diagnosticCtx, chromedp.Evaluate(`chromeGate.calls.map((_,i)=>chromeGate.setupState(i))`, &states))
			diagnosticCancel()
			t.Fatalf("row 2 setup failed adding call %d/8 within 30s: %v; last_state=%s all_states=%s diagnostic_error=%v", index+1, err, chromeJSON(state), states, diagnosticErr)
		}
		t.Logf("CHROME_ADD_CALL finish call=%d/8 state=%s", index+1, chromeJSON(state))
		require.Empty(t, state.Ended, "row 2 setup call %d/8 failed: %s", index+1, chromeJSON(state))
		require.True(t, state.Ready, "row 2 setup call %d/8 did not finish: %s", index+1, chromeJSON(state))
	}
	var paths []string
	require.NoError(t, chromedp.Run(setupCtx, chromedp.Evaluate(`chromeGate.calls.map(c=>c.path)`, &paths)), "row 2 setup: collect eight call paths within 30s")
	for _, path := range paths {
		callID := strings.TrimPrefix(path, "/calls/")
		_, a, e := store.RelayOwner(setupCtx, callID)
		require.NoError(t, e)
		if a.Slot != "r1" {
			require.NoError(t, plane.ChangeRelay(setupCtx, callID, "r1"), "row 2 setup: place %s on r1 within 30s", callID)
		}
	}
	setupCancel()
	chromeWait(t, browser, 2*time.Second)
	t.Log("CHROME_PHASE kill-active-and-standby")
	fault := chromeNow(t, browser)
	require.NoError(t, standby.Signal(syscall.SIGKILL))
	require.NoError(t, active.Signal(syscall.SIGKILL))
	select {
	case <-active.Done():
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case <-standby.Done():
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	chromeWait(t, browser, 7300*time.Millisecond) // includes two full seconds after a recovery at the 5 s limit
	var calls []chromeCall
	require.NoError(t, chromedp.Run(browser, chromedp.Evaluate(fmt.Sprintf(`chromeGate.calls.map((_,i)=>chromeGate.snapshot(i,%f))`, fault), &calls)))
	var lost []chromeMeasurement
	for _, c := range calls {
		m := chromeMeasure(c, fault)
		checkDecrypt(m)
		lost = append(lost, m)
		t.Logf("CHROME_SWITCH row=2 call=%s evidence=%s", c.Path, chromeJSON(m))
		require.Empty(t, c.Ended)
		require.True(t, m.Progress)
		require.True(t, chromePostProgress(c, fault, bindings[1].Slot.Public.String()), "recovery must keep progressing on the new pair for two seconds")
		require.LessOrEqual(t, m.AudioGap, 5000.0)
		require.LessOrEqual(t, m.VideoGap, 5000.0)
		failures, e := worker.SessionDecryptFailures(strings.TrimPrefix(c.Path, "/calls/"))
		require.NoError(t, e)
		require.Zero(t, failures)
	}
	chromeTable(t, "2-SIGKILL-local", lost)
	t.Log("NOT-RUN row=2 netns; row=4 public-address-only fault is not staged by this local driver")
}

func findChromeRedis() (string, error) { return exec.LookPath("redis-server") }
func chromeRedisAddress() (string, error) {
	for port := 16500; port <= 16599; port++ {
		addr := fmt.Sprintf("127.0.0.1:%d", port)
		l, e := net.Listen("tcp4", addr)
		if e == nil {
			_ = l.Close()
			return addr, nil
		}
	}
	return "", fmt.Errorf("no free throwaway Redis port in 16500-16599")
}

// RTP comes from the audio receiver's synchronization source matching the
// inbound SSRC, never from a wall-clock packet timestamp.
type chromeTrack struct {
	Packets float64  `json:"packetsReceived"`
	Frames  float64  `json:"framesDecoded"`
	Bytes   float64  `json:"bytesReceived"`
	RTP     *float64 `json:"rtpTimestamp"`
	Decrypt *float64 `json:"packetsFailedDecryption"`
}
type chromePair struct {
	ID     string   `json:"id"`
	Remote string   `json:"remote"`
	Sent   *float64 `json:"packetsSent"`
	Bytes  *float64 `json:"bytesReceived"`
}
type chromeSample struct {
	At       float64             `json:"at"`
	Audio    chromeTrack         `json:"audio"`
	Video    chromeTrack         `json:"video"`
	Pair     *chromePair         `json:"pair"`
	Sources  chromeSignalSources `json:"sources"`
	Sampling chromeSampling      `json:"sampling"`
}
type chromeSampling struct {
	StatsMs     float64 `json:"statsMs"`
	ProcessMs   float64 `json:"processMs"`
	IntervalMs  float64 `json:"intervalMs"`
	ReportCount int     `json:"reportCount"`
	PairCount   int     `json:"pairCount"`
}
type chromeServerEvent struct {
	Ms    float64 `json:"ms"`
	Event string  `json:"event"`
	Path  string  `json:"path"`
}
type chromeSignalSources struct {
	AudioPackets string  `json:"audioPackets"`
	AudioRTP     *string `json:"audioRTP"`
	VideoFrames  string  `json:"videoFrames"`
}
type chromeCall struct {
	At         float64           `json:"at"`
	Path       string            `json:"path"`
	Ended      string            `json:"ended"`
	Error      string            `json:"error"`
	Reconnects int               `json:"reconnects"`
	Samples    []chromeSample    `json:"samples"`
	Timeline   []json.RawMessage `json:"timeline"`
}
type chromeSetupState struct {
	Index   int               `json:"index"`
	Path    string            `json:"path"`
	Ready   bool              `json:"ready"`
	Ended   string            `json:"ended"`
	Phase   string            `json:"phase"`
	Phases  []json.RawMessage `json:"phases"`
	Failure json.RawMessage   `json:"failure"`
}
type chromeMeasurement struct {
	AudioGap, VideoGap                                float64
	Before, After                                     *chromePair
	AudioPackets, AudioBytes, VideoFrames, VideoBytes float64
	Progress                                          bool
	DecryptExposed                                    bool
	Decrypt                                           float64
	Sources                                           chromeSignalSources
	ObservedAt                                        float64
	SamplingMax                                       chromeSampling
}

func chromeMeasure(c chromeCall, begin float64) chromeMeasurement {
	var m chromeMeasurement
	// The snapshot timestamp and counters share a browser observation boundary.
	// Time spent serializing/decoding CDP evidence is not a media observation.
	end := c.At
	m.ObservedAt = end
	var before, last chromeSample
	for _, s := range c.Samples {
		if s.At <= begin {
			before = s
		}
		if s.At <= end {
			last = s
		}
	}
	m.Before, m.After = before.Pair, last.Pair
	m.Sources = last.Sources
	m.AudioPackets = last.Audio.Packets - before.Audio.Packets
	m.AudioBytes = last.Audio.Bytes - before.Audio.Bytes
	m.VideoFrames = last.Video.Frames - before.Video.Frames
	m.VideoBytes = last.Video.Bytes - before.Video.Bytes
	m.Progress = m.AudioPackets > 0 && m.VideoFrames > 0 && before.Audio.RTP != nil && last.Audio.RTP != nil && *before.Audio.RTP != *last.Audio.RTP
	prev := before
	audioAt, videoAt := before.At, before.At
	for _, s := range c.Samples {
		if s.At <= begin || s.At > end {
			continue
		}
		m.SamplingMax.StatsMs = math.Max(m.SamplingMax.StatsMs, s.Sampling.StatsMs)
		m.SamplingMax.ProcessMs = math.Max(m.SamplingMax.ProcessMs, s.Sampling.ProcessMs)
		m.SamplingMax.IntervalMs = math.Max(m.SamplingMax.IntervalMs, s.Sampling.IntervalMs)
		m.SamplingMax.ReportCount = max(m.SamplingMax.ReportCount, s.Sampling.ReportCount)
		m.SamplingMax.PairCount = max(m.SamplingMax.PairCount, s.Sampling.PairCount)
		if s.Audio.Packets > prev.Audio.Packets {
			m.AudioGap = math.Max(m.AudioGap, s.At-audioAt)
			audioAt = s.At
		}
		if s.Video.Frames > prev.Video.Frames {
			m.VideoGap = math.Max(m.VideoGap, s.At-videoAt)
			videoAt = s.At
		}
		for _, v := range []*float64{s.Audio.Decrypt, s.Video.Decrypt} {
			if v != nil {
				m.DecryptExposed = true
				m.Decrypt = math.Max(m.Decrypt, *v)
			}
		}
		prev = s
	}
	m.AudioGap = math.Max(m.AudioGap, end-audioAt)
	m.VideoGap = math.Max(m.VideoGap, end-videoAt)
	return m
}

// Reject a short burst followed by a second freeze. Every half-second of the
// final two-second observation must advance all three ADR success counters.
// Candidate selection is checked separately from those media counters.
func chromePostProgress(c chromeCall, switched float64, remote string) bool {
	if len(c.Samples) == 0 {
		return false
	}
	end := c.Samples[len(c.Samples)-1].At
	begin := end - 2000
	if begin < switched {
		return false
	}
	var previous chromeSample
	haveBefore := false
	for _, s := range c.Samples {
		if s.At <= begin {
			previous = s
			haveBefore = true
		}
	}
	if !haveBefore || previous.Pair == nil || previous.Pair.Remote != remote {
		return false
	}
	for part := 1; part <= 4; part++ {
		due := begin + float64(part)*500
		current := previous
		for _, s := range c.Samples {
			if s.At > previous.At && s.At <= due {
				if s.Pair == nil || s.Pair.Remote != remote {
					return false
				}
				current = s
			}
		}
		if current.Audio.Packets <= previous.Audio.Packets || current.Video.Frames <= previous.Video.Frames || previous.Audio.RTP == nil || current.Audio.RTP == nil || *current.Audio.RTP == *previous.Audio.RTP {
			return false
		}
		previous = current
	}
	return true
}

func chromeNow(t *testing.T, ctx context.Context) float64 {
	t.Helper()
	var now float64
	require.NoError(t, chromedp.Run(ctx, chromedp.Evaluate("performance.now()", &now)))
	return now
}
func chromeWait(t *testing.T, ctx context.Context, d time.Duration) {
	t.Helper()
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	case <-timer.C:
	}
}
func chromePercentile(values []float64, p int) float64 {
	v := append([]float64(nil), values...)
	sort.Float64s(v)
	return v[max(0, int(math.Ceil(float64(len(v)*p)/100))-1)]
}
func chromeTable(t *testing.T, row string, values []chromeMeasurement) {
	t.Helper()
	for _, track := range []string{"audio", "video"} {
		var v []float64
		for _, m := range values {
			gap := m.AudioGap
			if track == "video" {
				gap = m.VideoGap
			}
			v = append(v, gap)
		}
		t.Logf("CHROME_GATE row=%s track=%s n=%d p50_ms=%.1f p95_ms=%.1f max_ms=%.1f", row, track, len(v), chromePercentile(v, 50), chromePercentile(v, 95), chromePercentile(v, 100))
	}
}
func chromeJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestChromeMeasurementsRequireMediaProgress(t *testing.T) {
	number := func(v float64) *float64 { return &v }
	sample := func(at, packets, frames, rtp float64) chromeSample {
		return chromeSample{At: at, Audio: chromeTrack{Packets: packets, RTP: number(rtp)}, Video: chromeTrack{Frames: frames}, Pair: &chromePair{Remote: "127.0.0.1:49000"}}
	}
	good := chromeCall{At: 150, Samples: []chromeSample{sample(100, 1, 1, 100), sample(120, 2, 2, 200), sample(140, 3, 3, 300)}}
	m := chromeMeasure(good, 100)
	require.True(t, m.Progress)
	require.Equal(t, 20.0, m.AudioGap)
	require.Equal(t, 20.0, m.VideoGap)
	for _, kind := range []string{"pair-only", "no-video", "no-audio", "no-rtp", "rtp-wallclock-unavailable"} {
		t.Run(kind, func(t *testing.T) {
			bad := chromeCall{At: 150, Samples: []chromeSample{sample(100, 1, 1, 100), sample(140, 2, 2, 200)}}
			switch kind {
			case "pair-only":
				bad.Samples[1] = sample(140, 1, 1, 100)
				bad.Samples[1].Pair.Remote = "127.0.0.1:49001"
			case "no-video":
				bad.Samples[1].Video.Frames = 1
			case "no-audio":
				bad.Samples[1].Audio.Packets = 1
			case "no-rtp":
				bad.Samples[1].Audio.RTP = number(100)
			case "rtp-wallclock-unavailable":
				bad.Samples[1].Audio.RTP = nil
			}
			require.False(t, chromeMeasure(bad, 100).Progress)
		})
	}
	good.At = 1000
	trailing := chromeMeasure(good, 100)
	require.Equal(t, 860.0, trailing.VideoGap, "include trailing freeze")
	require.Equal(t, 3.0, chromePercentile([]float64{3, 1, 2}, 95))
}

func TestChromeRecoveryRequiresTwoSecondsOfProgress(t *testing.T) {
	var samples []chromeSample
	for i := 0; i <= 30; i++ {
		rtp := float64(i * 4800)
		samples = append(samples, chromeSample{At: float64(i * 100), Audio: chromeTrack{Packets: float64(i * 5), RTP: &rtp}, Video: chromeTrack{Frames: float64(i * 3)}, Pair: &chromePair{Remote: "target"}})
	}
	require.True(t, chromePostProgress(chromeCall{Samples: samples}, 500, "target"))
	require.False(t, chromePostProgress(chromeCall{Samples: samples}, 1500, "target"), "two seconds must be observed after the switch")
	require.False(t, chromePostProgress(chromeCall{Samples: samples}, 500, "old"), "media on the wrong pair is not recovery")
	frozen := append([]chromeSample(nil), samples...)
	for i := 15; i < len(frozen); i++ {
		frozen[i].Audio = frozen[14].Audio
		frozen[i].Video = frozen[14].Video
	}
	require.True(t, chromeMeasure(chromeCall{At: 3000, Samples: frozen}, 0).Progress, "a cumulative-only check would incorrectly accept this burst")
	require.False(t, chromePostProgress(chromeCall{Samples: frozen}, 500, "target"), "a brief recovery followed by a freeze must fail")
}
