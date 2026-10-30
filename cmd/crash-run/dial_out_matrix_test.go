//go:build !windows

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/clusterprocess"
	"github.com/relaishq/relais/internal/nettopology"
	"github.com/relaishq/relais/internal/redistestenv"
	"github.com/relaishq/relais/internal/workerlink"
	"github.com/relaishq/relais/pkg/callharness"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/relaishq/relais/pkg/storage"
	"github.com/stretchr/testify/require"
)

type tappedFrames struct {
	io.ReadCloser
	pending bytes.Buffer
	visit   func(workerlink.Message) error
}

func (b *tappedFrames) Read(dst []byte) (int, error) {
	for b.pending.Len() == 0 {
		m, err := workerlink.Read(b.ReadCloser)
		if err != nil {
			return 0, err
		}
		if err := b.visit(m); err != nil {
			if err == errDiscardFrame {
				continue
			}
			return 0, err
		}
		if err := workerlink.Write(&b.pending, m); err != nil {
			return 0, err
		}
	}
	return b.pending.Read(dst)
}

type processLinkGate struct {
	exportID  atomic.Uint64
	armed     atomic.Bool
	entered   chan struct{}
	release   chan struct{}
	canceled  chan struct{}
	once      sync.Once
	mu        sync.Mutex
	active    map[*http.Request]context.CancelFunc
	blocked   bool
	blackhole bool
	restored  chan struct{}
	discarded atomic.Uint64
}

func newProcessLinkGate() *processLinkGate {
	return &processLinkGate{entered: make(chan struct{}), release: make(chan struct{}), canceled: make(chan struct{}), active: make(map[*http.Request]context.CancelFunc)}
}

var errDiscardFrame = errors.New("test: discard link frame")

func (g *processLinkGate) discard() error {
	g.mu.Lock()
	blocked := g.blocked && g.blackhole
	g.mu.Unlock()
	if blocked {
		g.discarded.Add(1)
		return errDiscardFrame
	}
	return nil
}
func (g *processLinkGate) fault(blackhole bool) {
	g.mu.Lock()
	g.blocked, g.blackhole, g.restored = true, blackhole, make(chan struct{})
	var cancel []context.CancelFunc
	if !blackhole {
		for _, stop := range g.active {
			cancel = append(cancel, stop)
		}
	}
	g.mu.Unlock()
	for _, stop := range cancel {
		stop()
	}
}
func (g *processLinkGate) restore() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.blocked = false
	close(g.restored)
}

func (g *processLinkGate) admit(w http.ResponseWriter, r *http.Request, next http.Handler) {
	g.mu.Lock()
	blocked, blackhole, restored := g.blocked, g.blackhole, g.restored
	if blocked {
		g.mu.Unlock()
		if !blackhole {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-restored:
		}
		g.mu.Lock()
	}
	ctx, cancel := context.WithCancel(r.Context())
	g.active[r] = cancel
	g.mu.Unlock()
	defer func() { cancel(); g.mu.Lock(); delete(g.active, r); g.mu.Unlock() }()
	request := r.WithContext(ctx)
	request.Body = &tappedFrames{ReadCloser: r.Body, visit: func(m workerlink.Message) error { return g.workerFrame(ctx, m) }}
	next.ServeHTTP(w, request)
}
func (g *processLinkGate) unblock() { g.once.Do(func() { close(g.release) }) }
func (g *processLinkGate) workerFrame(ctx context.Context, m workerlink.Message) error {
	if err := g.discard(); err != nil {
		return err
	}
	if m.Kind == "reply" && m.ID == g.exportID.Load() && g.armed.CompareAndSwap(true, false) {
		close(g.entered)
		select {
		case <-g.release:
		case <-ctx.Done():
			close(g.canceled)
			return ctx.Err()
		}
	}
	return nil
}

type dialProcessRig struct {
	ctx      context.Context
	topology *pairTopology
	release  *pairRelease
	store    *sessionstore.Redis
	mapping  *nettopology.NATMapping
	gate     *processLinkGate
	harness  *callharness.Harness
	inbound  bool
}

func newDialProcessRig(t *testing.T, nat bool) *dialProcessRig {
	return newModeProcessRig(t, nat, false)
}
func newModeProcessRig(t *testing.T, nat, inbound bool, cacheOff ...bool) *dialProcessRig {
	t.Helper()
	if os.Getenv("RELAIS_TEST_DIAL_OUT_PROCESS") != "1" {
		t.Skip("set RELAIS_TEST_DIAL_OUT_PROCESS=1 after make build")
	}
	if testing.Short() {
		t.Skip("native process matrix needs full fault windows")
	}
	manager := &clusterprocess.Manager{}
	managed, stop := manager.Context()
	t.Cleanup(stop)
	ctx, cancel := context.WithTimeout(managed, 3*time.Minute)
	t.Cleanup(cancel)
	bin, err := filepath.Abs("../../bin")
	require.NoError(t, err)
	logRoot := os.Getenv("RELAIS_TEST_PROCESS_LOG_DIR")
	if logRoot == "" {
		logRoot = bin
	}
	dir, err := os.MkdirTemp(logRoot, "dial-out-matrix-")
	require.NoError(t, err)
	t.Logf("process logs: %s", dir)
	var key [32]byte
	_, err = rand.Read(key[:])
	require.NoError(t, err)
	addr, prefix := redistestenv.Address(t), "dial-out-matrix:"+rand.Text()+":"
	require.NotEmpty(t, addr)
	store, err := sessionstore.NewRedis(ctx, storage.RedisConfig{Addr: addr, Prefix: prefix}, key[:])
	require.NoError(t, err)
	env := processEnv(hex.EncodeToString(key[:]), addr, prefix)
	topology, err := newPairTopology(manager, dir, env, "", 0, 0)
	require.NoError(t, err)
	rig := &dialProcessRig{ctx: ctx, topology: topology, release: &pairRelease{Dir: bin, Version: "local"}, store: store, gate: newProcessLinkGate(), inbound: inbound}
	transport := workerlink.Transport()
	var proxy *httptest.Server
	t.Cleanup(func() {
		rig.gate.unblock()
		manager.Stop(true)
		if proxy != nil {
			proxy.CloseClientConnections()
			proxy.Close()
		}
		transport.CloseIdleConnections()
		if rig.mapping != nil {
			require.NoError(t, rig.mapping.Close())
		}
		require.NoError(t, store.Close())
		if !t.Failed() && os.Getenv("RELAIS_TEST_KEEP_PROCESS_LOGS") != "1" {
			_ = os.RemoveAll(dir)
		}
	})
	if len(cacheOff) == 0 {
		require.NoError(t, topology.startRelay(ctx, rig.release))
	} else {
		// The existing crash-run cache/PLI comparison disables relay buffering
		// in both modes; otherwise caller ciphertext replay masks PLI recovery.
		topology.relays++
		args := append(topology.relayArgs(rig.release, false), "-buffer-off")
		p, err := topology.launch("relay-1", "relay", rig.release, withoutKey(env), args...)
		require.NoError(t, err)
		require.NoError(t, topology.ready(ctx, p, 10*time.Second))
		require.Equal(t, topology.relayMedia, p.Ready.Media)
		require.Equal(t, topology.relayLeg, p.Ready.Leg)
		topology.relay = p
	}
	rig.startControl(t)
	target, err := url.Parse("http://" + topology.controlAddr)
	require.NoError(t, err)
	reverse := httputil.NewSingleHostReverseProxy(target)
	reverse.Transport, reverse.FlushInterval = transport, -1
	reverse.ModifyResponse = func(resp *http.Response) error {
		if resp.StatusCode == http.StatusOK && resp.Request.URL.Path == "/private/workers/0/link" {
			resp.Body = &tappedFrames{ReadCloser: resp.Body, visit: func(m workerlink.Message) error {
				if err := rig.gate.discard(); err != nil {
					return err
				}
				if m.Kind == "request" && strings.HasSuffix(m.Path, "/export") {
					rig.gate.exportID.Store(m.ID)
				}
				return nil
			}}
		}
		return nil
	}
	proxy = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/private/workers/0/link" {
			rig.gate.admit(w, r, reverse)
			return
		}
		reverse.ServeHTTP(w, r)
	}))
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	proxy.Config.Protocols = protocols
	proxy.Start()
	if nat {
		rig.mapping, err = nettopology.NewNATMapping(netip.MustParseAddrPort(topology.relayLeg))
		require.NoError(t, err)
	}
	for i := range topology.workers {
		controlURL, leg := "http://"+topology.controlAddr, topology.relayLeg
		if i == 0 {
			controlURL = proxy.URL
			if nat {
				leg = rig.mapping.WorkerAddr().String()
			}
		}
		args := []string{rig.release.binary("relais-worker"), "-name", strconv.Itoa(i), "-media", topology.workerMedia[i], "-relay-leg", leg, "-relay-media", topology.relayMedia, "-control", controlURL}
		if len(cacheOff) != 0 && cacheOff[0] {
			args = append(args, "-frame-cache-off")
		}
		if inbound {
			args = append(args, "-http", topology.workerHTTP[i])
		} else {
			args = append(args, "-connect=dial-out")
		}
		p, err := topology.launch(pairWorkerName(i), "worker", rig.release, env, args...)
		require.NoError(t, err)
		topology.workers[i] = p
		require.NoError(t, topology.ready(ctx, p, 8*time.Second))
	}
	rig.ready(t)
	return rig
}
func (r *dialProcessRig) startControl(t *testing.T) {
	t.Helper()
	p := r.topology
	args := []string{r.release.binary("relais-control"), "-http", p.controlAddr, "-relay", "http://" + p.relayHTTP}
	for i := range 2 {
		endpoint := "dial-out"
		if r.inbound {
			endpoint = "http://" + p.workerHTTP[i]
		}
		args = append(args, "-worker", strconv.Itoa(i)+"="+endpoint)
	}
	control, err := p.launch("control", "control", r.release, p.env, args...)
	require.NoError(t, err)
	require.NoError(t, p.ready(r.ctx, control, 5*time.Second))
	p.control = control
}
func (r *dialProcessRig) ready(t *testing.T) controlplane.Status {
	t.Helper()
	s, err := r.topology.waitControl(r.ctx, 10*time.Second, "dial-out eligibility", func(s controlplane.Status) bool {
		live := s.Ready && pairWorkerLive(s, "0", time.Now().Add(-time.Second)) && pairWorkerLive(s, "1", time.Now().Add(-time.Second)) && !pairWorker(s, "0").Draining
		return live && (r.inbound || pairWorker(s, "0").Link != nil && pairWorker(s, "0").Link.Connected)
	})
	if err != nil {
		t.Logf("eligibility: rebuild=%+v calls=%+v lost=%d", s.Rebuild, s.Calls, s.LostCount)
		for _, w := range s.Workers {
			t.Logf("worker=%+v link=%+v", w, w.Link)
		}
	}
	require.NoError(t, err)
	return s
}
func (r *dialProcessRig) caller(t *testing.T, duration time.Duration) *callharness.Call {
	t.Helper()
	h, err := callharness.Start(callharness.Options{External: &callharness.ExternalTopology{SignalingURL: "http://" + r.topology.controlAddr + "/calls", RelayAddr: netip.MustParseAddrPort(r.topology.relayMedia)}})
	require.NoError(t, err)
	r.harness = h
	t.Cleanup(func() { _ = h.Close() })
	c, err := h.Dial(r.ctx, callharness.CallOptions{Video: true, Worker: 0})
	require.NoError(t, err)
	ctx, stop := context.WithCancel(r.ctx)
	sent := make(chan error, 1)
	go func() { sent <- c.SendMedia(ctx, duration) }()
	t.Cleanup(func() {
		stop()
		select {
		case <-sent:
		case <-time.After(5 * time.Second):
			t.Error("caller media sender did not stop")
		}
	})
	time.Sleep(600 * time.Millisecond)
	return c
}
func (r *dialProcessRig) cleanCall(t *testing.T, c *callharness.Call) *callharness.Report {
	t.Helper()
	report, err := c.Hangup(r.ctx)
	require.NoError(t, err)
	require.True(t, report.ConnectedThroughout())
	require.Zero(t, report.DecryptionFailures.Total())
	require.Zero(t, report.ICERestarts)
	require.Zero(t, report.Renegotiations)
	for _, track := range report.Tracks {
		require.Less(t, report.HungUpAt-track.LastArrival, 200*time.Millisecond, "media must still be flowing")
	}
	video := report.Track("video")
	require.NotNil(t, video)
	require.NotNil(t, video.Video)
	require.True(t, video.Video.FullDecode.Ran())
	require.Empty(t, video.Video.FullDecode.Errors)
	require.Equal(t, video.Video.FullDecode.FramesIn, video.Video.FullDecode.FramesDecoded)
	r.workerCounters(t)
	return report
}
func (r *dialProcessRig) workerCounters(t *testing.T) {
	t.Helper()
	for i := range 2 {
		select {
		case <-r.topology.workers[i].Child.Done():
			continue
		default:
		}
		endpoint := "http://" + r.topology.controlAddr + "/private/workers/" + strconv.Itoa(i) + "/metrics"
		if r.inbound {
			endpoint = "http://" + r.topology.workerHTTP[i] + "/metrics"
		}
		values, available, err := scrapeMetrics(r.ctx, &http.Client{Timeout: 2 * time.Second}, endpoint)
		require.NoError(t, err)
		require.True(t, available["relais_worker_decryption_failures_total"])
		require.Zero(t, values["relais_worker_decryption_failures_total"])
	}
}

func TestDialOutProcessShutdownDuringDrain(t *testing.T) {
	for _, graceful := range []bool{true, false} {
		t.Run(strconv.FormatBool(graceful), func(t *testing.T) {
			r := newDialProcessRig(t, true)
			c := r.caller(t, 60*time.Second)
			r.gate.armed.Store(true)
			require.NoError(t, r.mapping.Rebind(netip.AddrPort{}))
			select {
			case <-r.gate.entered:
			case <-time.After(6 * time.Second):
				t.Fatal("drain did not reach the applied export reply")
			}
			intents, err := r.store.ControlIntents(r.ctx)
			require.NoError(t, err)
			require.Len(t, intents, 1)
			require.Equal(t, c.SessionID(), intents[0].ID)
			began := time.Now()
			signal := syscall.SIGKILL
			if graceful {
				signal = syscall.SIGTERM
			}
			require.NoError(t, r.topology.control.Child.SignalGroup(signal))
			if graceful {
				select {
				case <-r.gate.canceled:
					t.Fatal("shutdown cut off the applied export instead of waiting")
				case <-time.After(200 * time.Millisecond):
				}
			}
			r.gate.unblock()
			select {
			case <-r.topology.control.Child.Done():
			case <-time.After(8 * time.Second):
				t.Fatal("control shutdown did not finish")
			}
			r.startControl(t)
			s := r.ready(t)
			t.Logf("successor rebuild: %+v", s.Rebuild)
			if s.LostCount != 0 || len(s.Calls) != 1 {
				t.Logf("successor takeovers: %+v workers: %+v", s.Takeovers, s.Workers)
			}
			require.Len(t, s.Calls, 1)
			require.Zero(t, s.LostCount)
			if graceful {
				require.Empty(t, s.Takeovers)
			} else {
				// A lost applied export is the ADR's explicit uncertainty case,
				// not an idle-link false takeover. It must recover exactly once.
				require.Len(t, s.Takeovers, 1)
				require.False(t, s.Takeovers[0].Lost)
			}
			intents, err = r.store.ControlIntents(r.ctx)
			require.NoError(t, err)
			require.Empty(t, intents, "successor must reconcile the interrupted intent")
			// Space the later moves to avoid the open immediate-move decoder
			// gap tracked in https://github.com/relaishq/relais/issues/306.
			time.Sleep(2 * time.Second)
			require.NoError(t, c.MoveExternal(r.ctx, 0))
			time.Sleep(200 * time.Millisecond)
			require.NoError(t, c.MoveExternal(r.ctx, 1))
			time.Sleep(700 * time.Millisecond)
			r.cleanCall(t, c)
			t.Logf("DIAL_OUT_DRAIN_SHUTDOWN graceful=%t recovery=%s uncertain_recoveries=%d false_takeovers=0 caller_decryption_failures=0 worker_decryption_failures=0 logs=%s", graceful, time.Since(began), len(s.Takeovers), r.topology.dir)
		})
	}
}

func TestDialOutProcessLinkOutages(t *testing.T) {
	for _, blackhole := range []bool{false, true} {
		for _, idle := range []bool{true, false} {
			for _, duration := range []time.Duration{200 * time.Millisecond, time.Second, 5 * time.Second, 30 * time.Second} {
				t.Run(fmt.Sprintf("blackhole=%t/idle=%t/%s", blackhole, idle, duration), func(t *testing.T) {
					r := newDialProcessRig(t, false)
					var c *callharness.Call
					if !idle {
						c = r.caller(t, duration+20*time.Second)
					}
					before := r.ready(t)
					r.gate.fault(blackhole)
					time.Sleep(duration)
					during, err := r.topology.controlStatus(r.ctx)
					require.NoError(t, err)
					require.Empty(t, during.Takeovers)
					require.Zero(t, during.LostCount)
					require.False(t, pairWorker(during, "0").Dead)
					require.False(t, pairWorker(during, "0").Recovering)
					if !idle {
						require.Len(t, during.Calls, 1)
						require.Equal(t, before.Calls[0].Epoch, during.Calls[0].Epoch)
					}
					r.gate.restore()
					after := r.ready(t)
					if !blackhole || duration >= workerlink.LostAfter {
						_, err := r.topology.waitControl(r.ctx, 8*time.Second, "fresh link admission", func(s controlplane.Status) bool {
							w := pairWorker(s, "0")
							return w != nil && w.Link != nil && w.Link.Connected && w.Link.Reconnects > pairWorker(before, "0").Link.Reconnects
						})
						require.NoError(t, err)
					}
					require.Empty(t, after.Takeovers)
					require.Zero(t, after.LostCount)
					if blackhole && duration >= 5*time.Second {
						require.Positive(t, r.gate.discarded.Load(), "black hole must actually consume framed traffic")
					}
					if idle {
						c = r.caller(t, 15*time.Second)
					}
					for range 20 {
						for _, target := range []int{1, 0} {
							require.NoError(t, c.MoveExternal(r.ctx, target))
							time.Sleep(200 * time.Millisecond)
						}
					}
					report := r.cleanCall(t, c)
					var pooled []time.Duration
					for _, move := range report.Moves {
						var gap time.Duration
						for _, track := range move.Tracks {
							require.Positive(t, track.PacketsAfter)
							gap = max(gap, track.Gap)
						}
						pooled = append(pooled, gap)
					}
					for _, track := range report.Tracks {
						require.Less(t, track.MediaGap, 2*time.Second)
					}
					p95 := slices.Sorted(slices.Values(pooled))
					require.Len(t, p95, 40)
					require.LessOrEqual(t, p95[37], 150*time.Millisecond)
					t.Logf("DIAL_OUT_OUTAGE blackhole=%t idle=%t outage=%s moves=40 pooled_p95=%s discarded=%d false_takeovers=0 lost_calls=0 caller_decryption_failures=0 worker_decryption_failures=0 logs=%s", blackhole, idle, duration, p95[37], r.gate.discarded.Load(), r.topology.dir)
				})
			}
		}
	}
}
