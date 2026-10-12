package controlplane_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/relais/internal/privateapi"
	"github.com/relais/internal/workerprobe"
	"github.com/relais/pkg/agent"
	"github.com/relais/pkg/callharness"
	"github.com/relais/pkg/controlplane"
	"github.com/relais/pkg/framecache"
	"github.com/relais/pkg/mediaworker"
	"github.com/relais/pkg/relay"
	"github.com/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

// Drop the reply only after the real handler committed. This is the transport
// failure seen by a remote client, not a mock of the mutation itself.
type lostReply struct {
	path        string
	failure     error
	before      bool
	afterCommit func()
	armed       atomic.Bool
	dropped     atomic.Bool
	mu          sync.Mutex
	resumes     []mediaworker.ResumeRequest
}

func (f *lostReply) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Path == "/resume" {
		data, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		req.Body = io.NopCloser(strings.NewReader(string(data)))
		var resume mediaworker.ResumeRequest
		if err := json.Unmarshal(data, &resume); err != nil {
			return nil, err
		}
		f.mu.Lock()
		f.resumes = append(f.resumes, resume)
		f.mu.Unlock()
	}
	drop := strings.HasSuffix(req.URL.Path, f.path) && f.armed.CompareAndSwap(true, false)
	if drop && f.before {
		f.dropped.Store(true)
		return nil, f.failure
	}
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil || !drop {
		return resp, err
	}
	_, err = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return nil, err
	}
	if f.afterCommit != nil {
		f.afterCommit()
	}
	f.dropped.Store(true)
	return nil, f.failure
}
func (f *lostReply) requests() []mediaworker.ResumeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]mediaworker.ResumeRequest(nil), f.resumes...)
}

type httpSystem struct {
	plane       *controlplane.Plane
	relay       *relay.Relay
	workers     []*mediaworker.Worker
	remote      []*controlplane.RemoteWorker
	harness     *callharness.Harness
	diagnostics bool
}

func newHTTPSystem(t *testing.T, source, target, routing *lostReply, counts ...int) *httpSystem {
	return newHTTPSystemWithSettings(t, source, target, routing, httpSystemSettings{}, counts...)
}

type httpSystemSettings struct {
	workerStores        []sessionstore.Store
	sourceHeartbeatGate *atomic.Bool
	deadAfter           time.Duration
	holdTimeout         time.Duration
	agent               mediaworker.AgentConfig
	store               sessionstore.Store
	buffer              *relay.BufferConfig
	targetTransport     http.RoundTripper
	cache               framecache.Store
	callerSocket        func() (net.PacketConn, error)
	diagnostics         bool
	packetTrace         func(mediaworker.PacketTrace)
}

func newHTTPSystemWithSettings(t *testing.T, source, target, routing *lostReply, settings httpSystemSettings, counts ...int) *httpSystem {
	t.Helper()
	cleanupProbe := workerprobe.Enable()
	t.Cleanup(cleanupProbe)
	store := settings.store
	if store == nil {
		store = sessionstore.NewMemory()
	}
	r, err := relay.New(relay.Config{PublicAddr: "127.0.0.1:0", WorkerAddr: "127.0.0.1:0", Owners: store, HoldTimeout: settings.holdTimeout, Buffer: settings.buffer})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, r.Close()) })
	rs := httptest.NewServer(r.PrivateHandler())
	t.Cleanup(rs.Close)
	rr := &controlplane.RemoteRelay{URL: rs.URL}
	if routing != nil {
		rr.Client = &http.Client{Transport: routing, Timeout: 2 * time.Second}
	}
	plane := controlplane.New(rr, store)
	system := &httpSystem{plane: plane, relay: r, diagnostics: settings.diagnostics}
	count := 2
	if len(counts) > 0 {
		count = counts[0]
	}
	for i := range count {
		name := strconv.Itoa(i)
		workerStore := store
		if i < len(settings.workerStores) && settings.workerStores[i] != nil {
			workerStore = settings.workerStores[i]
		}
		w, err := mediaworker.New(mediaworker.Config{ListenAddr: "127.0.0.1:0", Agent: settings.agent, FrameCache: settings.cache, PacketTrace: settings.packetTrace, Relay: &mediaworker.RelayConfig{Addr: r.WorkerAddr(), PublicAddr: r.PublicAddr(), Owners: workerStore}})
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, w.Close()) })
		ws := httptest.NewServer(w.PrivateHandler())
		t.Cleanup(ws.Close)
		rw := &controlplane.RemoteWorker{URL: ws.URL}
		var fault *lostReply
		if i == 0 {
			fault = source
		}
		if i == 1 {
			fault = target
		}
		if fault != nil {
			rw.Client = &http.Client{Transport: fault, Timeout: 2 * time.Second}
		}
		if i == 1 && settings.targetTransport != nil {
			rw.Client = &http.Client{Transport: settings.targetTransport, Timeout: 2 * time.Second}
		}
		require.NoError(t, rr.AddWorker(context.Background(), w.LocalAddr()))
		require.NoError(t, plane.Register(name, w.LocalAddr(), rw))
		system.workers = append(system.workers, w)
		system.remote = append(system.remote, rw)
	}
	ps := httptest.NewServer(plane.ProcessHandler())
	t.Cleanup(ps.Close)
	for i := range count {
		name := strconv.Itoa(i)
		var heartbeats mediaworker.Heartbeats = &controlplane.RemoteHeartbeats{URL: ps.URL, Name: name}
		if i == 0 && settings.sourceHeartbeatGate != nil {
			heartbeats = &partitionHeartbeats{next: heartbeats, blocked: settings.sourceHeartbeatGate}
		}
		system.workers[i].StartHeartbeats(heartbeats)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = plane.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	h, err := callharness.Start(callharness.Options{External: &callharness.ExternalTopology{SignalingURL: ps.URL + "/calls", RelayAddr: r.PublicAddr(), CallerSocket: settings.callerSocket}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, h.Close()) })
	system.harness = h
	return system
}
func flowingCall(t *testing.T, s *httpSystem, action func(context.Context, *callharness.Call)) *callharness.Report {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	call, err := s.harness.Dial(ctx, callharness.CallOptions{Video: true, Worker: 0, SRTPDiagnostics: s.diagnostics})
	require.NoError(t, err)
	media, stop := context.WithCancel(ctx)
	sent := make(chan error, 1)
	go func() { sent <- call.SendMedia(media, 10*time.Second) }()
	defer func() { stop(); <-sent }()
	time.Sleep(600 * time.Millisecond)
	action(ctx, call)
	time.Sleep(700 * time.Millisecond)
	observedUntil := time.Now()
	report, err := call.Hangup(ctx)
	require.NoError(t, err, "caller DELETE must still find its live recovered call")
	require.True(t, report.ConnectedThroughout())
	require.Zero(t, report.DecryptionFailures.Total())
	require.Zero(t, report.ICERestarts)
	require.Zero(t, report.Renegotiations)
	require.Equal(t, 1, report.OfferAnswerExchanges)
	for _, kind := range []string{"audio", "video"} {
		track := report.Track(kind)
		require.NotNil(t, track)
		require.Positive(t, track.Packets)
		require.Less(t, observedUntil.Sub(report.StartedAt.Add(track.LastArrival)), 150*time.Millisecond, "media must flow after recovery, not just before the fault")
	}
	return report
}
func TestHTTPTakeoverLostResumeReply(t *testing.T) {
	for _, failure := range []error{context.DeadlineExceeded, errors.New("connection reset after commit")} {
		t.Run(failure.Error(), func(t *testing.T) {
			target := &lostReply{path: "/resume", failure: failure}
			target.armed.Store(true)
			s := newHTTPSystem(t, nil, target, nil)
			flowingCall(t, s, func(ctx context.Context, call *callharness.Call) {
				require.NoError(t, workerprobe.Kill(s.workers[0].LocalAddr()))
				require.Eventually(t, func() bool { status, err := s.plane.Status(ctx); return err == nil && len(status.Takeovers) == 1 }, 3*time.Second, 10*time.Millisecond)
				status, err := s.plane.Status(ctx)
				require.NoError(t, err)
				require.Zero(t, status.LostCount)
				require.False(t, status.Takeovers[0].Lost)
				require.Len(t, status.Calls, 1)
				require.Equal(t, "1", status.Calls[0].Owner)
			})
			require.True(t, target.dropped.Load())
			requests := target.requests()
			require.GreaterOrEqual(t, len(requests), 2)
			require.Equal(t, requests[0].Lease.Epoch, requests[1].Lease.Epoch)
			require.Equal(t, requests[0].State, requests[1].State, "retry confirms the same adoption without advancing counters again")
			require.EqualValues(t, 8192, requests[0].SequenceMargin)
			require.EqualValues(t, 128, requests[0].SRTCPIndexMargin)
		})
	}
}
func TestHTTPMoveLostReplies(t *testing.T) {
	for _, step := range []string{"export", "export-not-run", "forward-route", "resume", "hold"} {
		t.Run(step, func(t *testing.T) {
			fault := &lostReply{failure: errors.New("reply lost after commit")}
			var source, target, routing *lostReply
			switch step {
			case "export", "export-not-run":
				fault.path = "/export"
				fault.before = step == "export-not-run"
				source = fault
			case "resume":
				fault.path = "/resume"
				target = fault
				source = &lostReply{path: "/never"}
			case "forward-route":
				fault.path = "/move"
				routing = fault
			case "hold":
				fault.path = "/hold"
				routing = fault
			}
			s := newHTTPSystem(t, source, target, routing)
			report := flowingCall(t, s, func(ctx context.Context, call *callharness.Call) {
				fault.armed.Store(true)
				move, err := s.plane.Move(ctx, call.SessionID(), "1")
				if strings.HasPrefix(step, "export") {
					require.NoError(t, err)
					require.Equal(t, "takeover", move.Kind)
				} else {
					require.Error(t, err)
					if step != "hold" {
						require.True(t, move.Result.RolledBack)
					}
				}
				require.Zero(t, s.relay.Stats().Holds)
				status, err := s.plane.Status(ctx)
				require.NoError(t, err)
				require.Zero(t, status.LostCount)
				require.Len(t, status.Calls, 1)
				owner := "0"
				if strings.HasPrefix(step, "export") {
					owner = "1"
				}
				require.Equal(t, owner, status.Calls[0].Owner)
			})
			require.True(t, fault.dropped.Load())
			if step == "resume" {
				resumes := source.requests()
				require.Len(t, resumes, 1)
				require.EqualValues(t, 8192, resumes[0].SequenceMargin)
				require.EqualValues(t, 128, resumes[0].SRTCPIndexMargin)
			}
			if strings.HasPrefix(step, "export") {
				require.Len(t, report.Moves, 1)
				require.Equal(t, "takeover", report.Moves[0].Kind)
			}
		})
	}
}

// Wrapping Echo exercises the custom-agent output path, whose sequence is
// allocated independently of the caller's sequence.
type lostReplyAudioAgent struct{ agent.Echo }

func TestHTTPMoveUncertainAgentCauseAfterTargetAudio(t *testing.T) {
	source := &lostReply{path: "/never"}
	target := &lostReply{path: "/resume", failure: agent.ErrRestore}
	s := newHTTPSystemWithSettings(t, source, target, nil, httpSystemSettings{
		holdTimeout: 200 * time.Millisecond,
		agent: mediaworker.AgentConfig{Factory: func(string) agent.Agent {
			return &lostReplyAudioAgent{}
		}},
	})
	var targetAudio atomic.Uint64
	require.NoError(t, workerprobe.SetAfterEcho(s.workers[1].LocalAddr(), func(_ context.Context, _ string, packet []byte) {
		var header rtp.Header
		if _, err := header.Unmarshal(packet); err == nil && header.PayloadType == 111 {
			targetAudio.Add(1)
		}
	}))
	target.afterCommit = func() {
		// Keep the lost reply in flight until the hold expires and the
		// adopted target has encrypted and sent audio with advanced indexes.
		require.Eventually(t, func() bool {
			return s.relay.Stats().HoldTimeouts == 1 && targetAudio.Load() >= 10
		}, time.Second, time.Millisecond)
	}
	report := flowingCall(t, s, func(ctx context.Context, call *callharness.Call) {
		target.armed.Store(true)
		move, err := s.plane.Move(ctx, call.SessionID(), "1")
		require.ErrorIs(t, err, privateapi.ErrUncertain)
		require.ErrorIs(t, err, agent.ErrRestore)
		require.True(t, move.Result.RolledBack)
		require.False(t, move.Lost)
		require.GreaterOrEqual(t, targetAudio.Load(), uint64(10))
		resumes := source.requests()
		require.Len(t, resumes, 1)
		require.Equal(t, agent.Takeover, resumes[0].Kind)
		require.GreaterOrEqual(t, resumes[0].SequenceMargin, uint16(8192))
		require.GreaterOrEqual(t, resumes[0].SRTCPIndexMargin, uint32(128))
		status, err := s.plane.Status(ctx)
		require.NoError(t, err)
		require.Zero(t, status.LostCount)
		require.Len(t, status.Calls, 1)
		require.Equal(t, "0", status.Calls[0].Owner)
	})
	require.True(t, target.dropped.Load())
	require.Zero(t, report.DecryptionFailures.Total())
}

func TestHTTPTakeoverLostRouteReplyAndRejoinFence(t *testing.T) {
	route := &lostReply{path: "/move", failure: errors.New("route reply lost")}
	route.armed.Store(true)
	s := newHTTPSystem(t, nil, nil, route)
	flowingCall(t, s, func(ctx context.Context, call *callharness.Call) {
		require.NoError(t, workerprobe.Pause(s.workers[0].LocalAddr(), true))
		require.Eventually(t, func() bool {
			status, err := s.plane.Status(ctx)
			return err == nil && len(status.Takeovers) == 1 && !status.Takeovers[0].Lost
		}, 3*time.Second, 10*time.Millisecond)
		require.NoError(t, workerprobe.Pause(s.workers[0].LocalAddr(), false))
		require.Eventually(t, func() bool {
			old, err := s.remote[0].Status(ctx)
			status, e := s.plane.Status(ctx)
			return err == nil && e == nil && old.Sessions == 0 && !status.Workers[0].Dead
		}, 2*time.Second, 10*time.Millisecond, "remote rejoin must fence and drop the real stale session")
	})
	require.True(t, route.dropped.Load())
}

// Drain marks the adopted target ineligible while the lost reply is in flight.
// The short deadline stops Drain waiting for this very resume's reservation.
func TestHTTPThreeWorkerRetryChangesTarget(t *testing.T) {
	for _, step := range []string{"resume", "route"} {
		t.Run(step, func(t *testing.T) {
			fault := &lostReply{path: "/resume", failure: context.DeadlineExceeded}
			var target, routing *lostReply
			if step == "resume" {
				target = fault
			} else {
				fault.path = "/move"
				routing = fault
			}
			s := newHTTPSystem(t, nil, target, routing, 3)
			fault.afterCommit = func() {
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
				defer cancel()
				_, _ = s.plane.Drain(ctx, "1")
			}
			fault.armed.Store(true)
			flowingCall(t, s, func(ctx context.Context, _ *callharness.Call) {
				require.NoError(t, workerprobe.Kill(s.workers[0].LocalAddr()))
				require.Eventually(t, func() bool { status, err := s.plane.Status(ctx); return err == nil && len(status.Takeovers) == 1 }, 3*time.Second, 10*time.Millisecond)
				status, err := s.plane.Status(ctx)
				require.NoError(t, err)
				require.Zero(t, status.LostCount)
				require.Len(t, status.Calls, 1)
				require.Equal(t, "2", status.Calls[0].Owner)
				require.False(t, status.Takeovers[0].Lost)
			})
			require.True(t, fault.dropped.Load())
		})
	}
}
