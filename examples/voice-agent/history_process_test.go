//go:build !windows

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/relaishq/relais/examples/voice-agent/voice"
	legacy "github.com/relaishq/relais/pkg/agent"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/callharness"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/relay"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/relaishq/relais/pkg/storage"
	"github.com/relaishq/relais/pkg/workerapp"
	"github.com/stretchr/testify/require"
)

type historyWitness struct {
	Kind   string              `json:"kind"`
	At     time.Time           `json:"at,omitzero"`
	State  agent.State         `json:"state"`
	Resume *agent.ResumeNotice `json:"resume,omitempty"`
	Audio  int                 `json:"audio"`
}

var historyOutputMu sync.Mutex

func writeHistoryWitness(w historyWitness) {
	historyOutputMu.Lock()
	defer historyOutputMu.Unlock()
	_ = json.NewEncoder(os.Stdout).Encode(w)
}

// The witness delegates every callback to Demo A and changes no state/effects.
type observedVoice struct {
	agent.Agent
	agent.MoveOnlyState
}

func (a *observedVoice) Save(ctx context.Context) (agent.State, error) {
	s, err := a.Agent.Save(ctx)
	if err == nil && bytes.Contains(s.History, []byte(`"user":"turn-5"`)) && bytes.Contains(s.Bytes, []byte(`"phase":"synthesizing"`)) {
		writeHistoryWitness(historyWitness{Kind: "candidate", At: time.Now(), State: s})
	}
	return s, err
}
func (a *observedVoice) Restore(ctx context.Context, s agent.State) error {
	if err := a.Agent.Restore(ctx, s); err != nil {
		return err
	}
	writeHistoryWitness(historyWitness{Kind: "restore", State: s})
	return nil
}
func (a *observedVoice) Handle(ctx context.Context, e agent.Event) (agent.Effects, error) {
	effects, err := a.Agent.Handle(ctx, e)
	if err == nil && e.Kind == agent.Resumed {
		s, saveErr := a.Agent.Save(ctx)
		if saveErr != nil {
			return effects, saveErr
		}
		writeHistoryWitness(historyWitness{Kind: "resumed", State: s, Resume: e.Resume, Audio: len(effects.Audio)})
	}
	return effects, err
}

// Only the parent-owned loopback fixture is reachable through this provider.
// No OpenAI client, environment key, or provider warmup is used.
type localHistoryProvider struct{ url string }

func (*localHistoryProvider) Available() bool { return true }
func (p *localHistoryProvider) request(ctx context.Context, path string, v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url+path, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != 200 {
		return nil, fmt.Errorf("fixture status %d", response.StatusCode)
	}
	return io.ReadAll(response.Body)
}
func (p *localHistoryProvider) Transcribe(ctx context.Context, _ string, b []byte) (string, error) {
	v, e := p.request(ctx, "/transcribe", b)
	return string(v), e
}
func (p *localHistoryProvider) Reply(ctx context.Context, _ string, text string, h []voice.Exchange) (string, error) {
	v, e := p.request(ctx, "/reply", historyReplyRequest{Transcript: text, History: h})
	return string(v), e
}
func (p *localHistoryProvider) Speech(ctx context.Context, _ string, text string) (io.ReadCloser, error) {
	v, e := p.request(ctx, "/speech", text)
	return io.NopCloser(bytes.NewReader(v)), e
}

func TestVoiceHistoryWorker(t *testing.T) {
	if os.Getenv("RELAIS_HISTORY_HELPER") != "1" {
		t.Skip("worker subprocess fixture")
	}
	args := strings.Split(os.Getenv("RELAIS_HISTORY_ARGS"), "\n")
	reg := voice.Registration(&localHistoryProvider{url: os.Getenv("RELAIS_HISTORY_PROVIDER")}, splitCheckpoints(args))
	factory := reg.Factory
	reg.Factory = func(args agent.FactoryArgs) agent.Agent {
		a := factory(args)
		writeHistoryWitness(historyWitness{Kind: "factory", State: agent.State{Bytes: []byte(fmt.Sprintf("%d/%d", args.MaxStateBytes, args.MaxHistoryBytes))}})
		return &observedVoice{Agent: a, MoveOnlyState: a.(agent.MoveOnlyState)}
	}
	err := workerapp.Run(context.Background(), args, workerapp.Options{DefaultAgent: voice.Name, Agents: []workerapp.Registration{reg}})
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
	}
	require.NoError(t, err)
}

type historyReplyRequest struct {
	Transcript string           `json:"transcript"`
	History    []voice.Exchange `json:"history"`
}
type historyFixture struct {
	mu        sync.Mutex
	turns     int
	replies   []historyReplyRequest
	exchanges []voice.Exchange
	release   chan struct{}
	waiting   chan struct{}
	cut       string
}

func historyReply(r historyReplyRequest) string {
	names := make([]string, len(r.History))
	for i, e := range r.History {
		names[i] = e.User
	}
	return "Remember " + strings.Join(names, ", ") + ". Reply to " + r.Transcript + "."
}
func (f *historyFixture) serve(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/transcribe":
		var audio []byte
		if json.NewDecoder(r.Body).Decode(&audio) != nil || !bytes.HasPrefix(audio, []byte("OggS")) {
			http.Error(w, "invalid recording", 400)
			return
		}
		f.mu.Lock()
		f.turns++
		turn := f.turns
		f.mu.Unlock()
		_, _ = fmt.Fprintf(w, "turn-%d", turn)
	case "/reply":
		var request historyReplyRequest
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			http.Error(w, "invalid request", 400)
			return
		}
		f.mu.Lock()
		f.replies = append(f.replies, request)
		f.mu.Unlock()
		if request.Transcript == "turn-5" && (f.cut == "provider_wait" || f.cut == "uncommitted") {
			close(f.waiting)
			select {
			case <-f.release:
			case <-r.Context().Done():
				return
			}
		}
		reply := historyReply(request)
		f.mu.Lock()
		f.exchanges = append(f.exchanges, voice.Exchange{User: request.Transcript, Assistant: reply})
		f.mu.Unlock()
		_, _ = io.WriteString(w, reply)
	case "/speech":
		var text string
		if json.NewDecoder(r.Body).Decode(&text) != nil {
			http.Error(w, "invalid speech", 400)
			return
		}
		packets := 5
		if f.cut == "playback" && strings.HasSuffix(text, "Reply to turn-4.") {
			packets = 150
		}
		audio := []byte{1}
		for range packets {
			audio = binary.BigEndian.AppendUint16(audio, 3)
			audio = append(audio, 0xf8, 0xff, 0xfe)
		}
		ogg, err := voice.RecordingOgg(audio)
		if err != nil {
			http.Error(w, "invalid audio", 500)
			return
		}
		_, _ = w.Write(ogg)
	default:
		http.NotFound(w, r)
	}
}

type historyWorker struct {
	cmd    *exec.Cmd
	done   chan struct{}
	events chan historyWitness
	remote *controlplane.RemoteWorker
	addr   netip.AddrPort
}

func startHistoryWorker(t *testing.T, ctx context.Context, args []string, provider string) *historyWorker {
	t.Helper()
	p := &historyWorker{done: make(chan struct{}), events: make(chan historyWitness, 256)}
	p.cmd = exec.CommandContext(ctx, os.Args[0], "-test.run=^TestVoiceHistoryWorker$")
	// Do not inherit any provider credentials or Redis configuration.
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if key == "OPENAI_API_KEY" || strings.HasPrefix(key, "RELAIS_") {
			continue
		}
		p.cmd.Env = append(p.cmd.Env, entry)
	}
	p.cmd.Env = append(p.cmd.Env, "RELAIS_HISTORY_HELPER=1", "RELAIS_HISTORY_ARGS="+strings.Join(args, "\n"), "RELAIS_HISTORY_PROVIDER="+provider, "RELAIS_SESSIONSTORE_KEY="+hex.EncodeToString(make([]byte, 32)))
	stdout, err := p.cmd.StdoutPipe()
	require.NoError(t, err)
	var stderr bytes.Buffer
	p.cmd.Stderr = &stderr
	require.NoError(t, p.cmd.Start())
	ready := make(chan struct{ HTTP, Media string }, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			var value struct{ HTTP, Media string }
			if json.Unmarshal(scanner.Bytes(), &value) == nil && value.HTTP != "" {
				ready <- value
				continue
			}
			var event historyWitness
			if json.Unmarshal(scanner.Bytes(), &event) == nil && event.Kind != "" {
				p.events <- event
			}
		}
		close(p.events)
	}()
	go func() { _ = p.cmd.Wait(); close(p.done) }()
	t.Cleanup(func() {
		_ = p.cmd.Process.Kill()
		<-p.done
		if t.Failed() {
			t.Logf("worker stderr: %s", stderr.String())
		}
	})
	select {
	case value := <-ready:
		p.remote = &controlplane.RemoteWorker{URL: value.HTTP}
		p.addr = netip.MustParseAddrPort(value.Media)
	case <-p.done:
		t.Fatalf("worker exited: %s", stderr.String())
	case <-ctx.Done():
		t.Fatal("worker readiness timeout")
	}
	return p
}

func TestDemoAHistoryProcessSIGKILL(t *testing.T) {
	if testing.Short() {
		t.Skip("real subprocess SIGKILL matrix")
	}
	if os.Getenv("RELAIS_TEST_REDIS_REQUIRE") != "1" && os.Getenv("RELAIS_TEST_REDIS_CLUSTER") != "1" {
		t.Skip("owned Redis requires explicit opt-in")
	}
	for _, cut := range []string{"idle", "recording", "provider_wait", "playback", "uncommitted"} {
		t.Run(cut, func(t *testing.T) { testDemoAHistoryCut(t, cut) })
	}
}
func testDemoAHistoryCut(t *testing.T, cut string) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	addr := startHistoryRedis(t, ctx)
	prefix := "voice-history:" + strconv.FormatInt(time.Now().UnixNano(), 10) + ":"
	store, err := sessionstore.NewRedis(ctx, storage.RedisConfig{Addr: addr, Prefix: prefix}, make([]byte, 32))
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()
	router, err := relay.New(relay.Config{Owners: store})
	require.NoError(t, err)
	defer func() { require.NoError(t, router.Close()) }()
	plane := controlplane.New(router, store)
	control := httptest.NewServer(plane.ProcessHandler())
	defer control.Close()
	fixture := &historyFixture{cut: cut, release: make(chan struct{}), waiting: make(chan struct{})}
	provider := httptest.NewServer(http.HandlerFunc(fixture.serve))
	defer provider.Close()
	// The proxy can hold source writes before they reach Redis. Closing it after
	// SIGKILL discards held bytes; no queued command can publish the newer pair.
	proxy := newHistoryStoreGate(t, addr)
	args := func(name, redis string) []string {
		return []string{"-name=" + name, "-control=" + control.URL, "-relay-leg=" + router.WorkerAddr().String(), "-relay-media=" + router.PublicAddr().String(), "-redis=" + redis, "-redis-prefix=" + prefix, "-frame-cache-off", "-agent-split-checkpoints"}
	}
	source := startHistoryWorker(t, ctx, args("0", proxy.addr), provider.URL)
	router.AddWorker(source.addr)
	require.NoError(t, plane.Register("0", source.addr, source.remote))
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); _ = plane.Run(runCtx) }()
	defer func() { stop(); <-done }()
	h, err := callharness.Start(callharness.Options{External: &callharness.ExternalTopology{SignalingURL: control.URL + "/calls", RelayAddr: router.PublicAddr()}})
	require.NoError(t, err)
	defer h.Close()
	call, err := h.Dial(ctx, callharness.CallOptions{})
	require.NoError(t, err)
	var sequence uint64
	send := func(worker *historyWorker, name string) {
		sequence++
		message := agent.ControlMessage{ID: fmt.Sprintf("button-%d", sequence), Sequence: sequence, Name: name}
		require.Eventually(t, func() bool { _, err := worker.remote.AgentControl(ctx, call.SessionID(), message); return err == nil }, 4*time.Second, 10*time.Millisecond)
	}
	status := func(worker *historyWorker) (voice.State, *mediaworker.AgentSessionStatus, bool) {
		return historyStatus(ctx, worker, call.SessionID())
	}
	awaitPhase := func(worker *historyWorker, phase string) {
		deadline := time.Now().Add(10 * time.Second)
		for {
			state, diagnostic, ok := status(worker)
			if ok && state.Phase == phase {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("await phase %s: state=%+v host=%+v", phase, state, diagnostic)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	turn := func(worker *historyWorker) {
		send(worker, "ptt.start")
		require.NoError(t, call.SendMedia(ctx, 150*time.Millisecond))
		send(worker, "ptt.stop")
	}
	for i := 1; i <= 4; i++ {
		turn(source)
		if i == 4 && cut == "playback" {
			awaitPhase(source, "playing")
		} else {
			awaitPhase(source, "idle")
		}
	}
	fixture.mu.Lock()
	expectedHistory, err := json.Marshal(fixture.exchanges[:4])
	fixture.mu.Unlock()
	require.NoError(t, err)
	var before sessionstore.ComponentTuple
	checkpoint := func(phase string) {
		require.Eventually(t, func() bool {
			tuple, err := store.ReadComponents(ctx, call.SessionID())
			if err != nil {
				return false
			}
			if !bytes.Equal(historyComponent(tuple.Components.History, 4), expectedHistory) {
				return false
			}
			var state voice.State
			if json.Unmarshal(historyComponent(tuple.Components.Work, 3), &state) != nil || state.Phase != phase {
				return false
			}
			before = tuple
			return true
		}, 5*time.Second, 5*time.Millisecond)
	}
	phase := "idle"
	if cut == "playback" {
		phase = "playing"
	}
	checkpoint(phase)
	target := startHistoryWorker(t, ctx, args("1", addr), provider.URL)
	router.AddWorker(target.addr)
	require.NoError(t, plane.Register("1", target.addr, target.remote))
	switch cut {
	case "recording":
		send(source, "ptt.start")
		require.NoError(t, call.SendMedia(ctx, 150*time.Millisecond))
		checkpoint("recording")
	case "provider_wait", "uncommitted":
		turn(source)
		select {
		case <-fixture.waiting:
		case <-ctx.Done():
			t.Fatal("provider wait not reached")
		}
		checkpoint("replying")
		if cut == "uncommitted" {
			proxy.pause()
			close(fixture.release)
			// The private status is the host's accepted state, not a Save witness.
			require.Eventually(t, func() bool {
				s, _, ok := status(source)
				return ok && s.Phase == "synthesizing" && strings.Contains(s.Reply, "turn-5")
			}, 3*time.Second, 2*time.Millisecond)
			acceptedAt := time.Now()
			var local agent.State
			var candidateAt time.Time
			require.Eventually(t, func() bool {
				select {
				case event := <-source.events:
					if event.Kind == "candidate" {
						local = event.State
						candidateAt = event.At
						return true
					}
				default:
				}
				return false
			}, time.Second, 2*time.Millisecond)
			state, _, ok := status(source)
			require.True(t, ok)
			accepted, err := json.Marshal(state)
			require.NoError(t, err)
			require.JSONEq(t, string(local.Bytes), string(accepted), "candidate Save is now the host's locally accepted state")
			require.NotEqual(t, expectedHistory, local.History)
			var newer []voice.Exchange
			require.NoError(t, json.Unmarshal(local.History, &newer))
			require.Len(t, newer, 4)
			require.Equal(t, "turn-5", newer[3].User)
			require.False(t, candidateAt.IsZero(), "candidate Save must carry its actual observation time")
			heldCtx, stopHeld := context.WithTimeout(ctx, 2*time.Second)
			heldAt, err := proxy.heldAfter(heldCtx, candidateAt)
			stopHeld()
			require.NoError(t, err, "gate must report a source write that arrived after the candidate Save")
			// Wait ten default checkpoint intervals after observing local acceptance.
			// Otherwise an immediate SIGKILL could pass without the gate doing work.
			wait := time.NewTimer(time.Until(acceptedAt.Add(time.Second)))
			defer wait.Stop()
			select {
			case <-wait.C:
			case <-ctx.Done():
				t.Fatal("checkpoint observation window cancelled")
			}
			t.Logf("gate held a source write %s after candidate Save; store observed after %s of local acceptance", heldAt.Sub(candidateAt), time.Since(acceptedAt))
			tuple, err := store.ReadComponents(ctx, call.SessionID())
			require.NoError(t, err)
			require.Equal(t, before.Components.History, tuple.Components.History, "newer local acceptance has not committed")
		}
	}
	require.NoError(t, source.cmd.Process.Kill())
	<-source.done
	require.Equal(t, syscall.SIGKILL, source.cmd.ProcessState.Sys().(syscall.WaitStatus).Signal())
	proxy.close()
	var resumed historyWitness
	var factories, restores []historyWitness
	require.Eventually(t, func() bool {
		select {
		case event, ok := <-target.events:
			if !ok {
				return false
			}
			switch event.Kind {
			case "factory":
				factories = append(factories, event)
			case "restore":
				restores = append(restores, event)
			case "resumed":
				resumed = event
				return true
			}
		default:
		}
		return false
	}, 8*time.Second, 5*time.Millisecond)
	require.NotEmpty(t, factories, "replacement factory witnessed")
	for _, event := range factories {
		require.Equal(t, []byte("3072/8192"), event.State.Bytes)
	}
	require.NotEmpty(t, restores, "actual Demo A Restore callback witnessed")
	for _, event := range restores {
		require.Equal(t, expectedHistory, event.State.History)
	}
	require.Equal(t, expectedHistory, resumed.State.History)
	require.NotNil(t, resumed.Resume)
	require.Equal(t, legacy.Takeover, resumed.Resume.Kind)
	require.Equal(t, 0, resumed.Audio)
	var recovered voice.State
	require.NoError(t, json.Unmarshal(resumed.State.Bytes, &recovered))
	require.Equal(t, "idle", recovered.Phase)
	require.Contains(t, recovered.Notice, "worker stopped")
	if cut != "idle" {
		require.Equal(t, agent.OutcomeUnknown, recovered.Outcome)
		require.Equal(t, agent.OutcomeUnknown, resumed.Resume.MoveOnly)
	} else {
		require.Equal(t, agent.OK, recovered.Outcome)
	}
	awaitPhase(target, "idle")
	s, diagnostic, ok := status(target)
	require.True(t, ok)
	require.Equal(t, "idle", s.Phase)
	require.Zero(t, diagnostic.RecordingBytes)
	require.Zero(t, diagnostic.PlaybackBytes)
	require.Zero(t, diagnostic.PlaybackPackets)
	// No new push-to-talk turn: recovery alone must not fabricate output audio.
	require.NoError(t, call.SendMedia(ctx, 100*time.Millisecond))
	s, diagnostic, ok = status(target)
	require.True(t, ok)
	require.Equal(t, "idle", s.Phase)
	require.Zero(t, diagnostic.PlaybackPackets)
	fixture.mu.Lock()
	priorReplies := len(fixture.replies)
	fixture.mu.Unlock()
	turn(target)
	awaitPhase(target, "idle")
	fixture.mu.Lock()
	require.Greater(t, len(fixture.replies), priorReplies)
	request := fixture.replies[len(fixture.replies)-1]
	fixture.mu.Unlock()
	encoded, err := json.Marshal(request.History)
	require.NoError(t, err)
	require.Equal(t, expectedHistory, encoded, "next real reply provider request consumes restored exchanges")
	s, _, ok = status(target)
	require.True(t, ok)
	require.Equal(t, historyReply(request), s.Reply, "next response is derived from earlier exchanges")
	_, err = call.Hangup(ctx)
	require.NoError(t, err)
	t.Logf("cut=%s SIGKILL pid=%d; exact %d history bytes restored; next request and reply used four prior exchanges", cut, source.cmd.Process.Pid, len(expectedHistory))
}

func historyStatus(ctx context.Context, p *historyWorker, id string) (voice.State, *mediaworker.AgentSessionStatus, bool) {
	var state voice.State
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.remote.URL+"/status?agent-session="+id, nil)
	if err != nil {
		return state, nil, false
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		return state, nil, false
	}
	defer func() { _ = response.Body.Close() }()
	var status mediaworker.WorkerStatus
	if response.StatusCode != 200 || json.NewDecoder(response.Body).Decode(&status) != nil || status.AgentSession == nil {
		return state, nil, false
	}
	err = json.Unmarshal(status.AgentSession.App, &state)
	return state, status.AgentSession, err == nil
}

// Inspect only the frozen byte-component frame through the public store API.
func historyComponent(b []byte, kind byte) []byte {
	if len(b) < 6 || b[0] != 10 || b[1] != kind || int(binary.BigEndian.Uint32(b[2:6])) != len(b)-6 {
		return nil
	}
	return b[6:]
}

func startHistoryRedis(t *testing.T, ctx context.Context) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := listener.Addr().String()
	_, port, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	require.NoError(t, listener.Close())
	require.NotContains(t, []string{"6379", "7000", "7001", "7002", "7003", "7004", "7005", "9101", "9201"}, port)
	command := exec.CommandContext(ctx, "redis-server", "--bind", "127.0.0.1", "--port", port, "--save", "", "--appendonly", "no", "--dir", t.TempDir())
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	require.NoError(t, command.Start())
	done := make(chan struct{})
	go func() { _ = command.Wait(); close(done) }()
	t.Cleanup(func() {
		_ = command.Process.Kill()
		<-done
		if t.Failed() {
			t.Logf("redis: %s", output.String())
		}
	})
	require.Eventually(t, func() bool {
		c, err := net.DialTimeout("tcp", addr, 50*time.Millisecond)
		if err == nil {
			_ = c.Close()
		}
		return err == nil
	}, 3*time.Second, 10*time.Millisecond)
	return addr
}

// A source-only TCP gate holds outbound bytes before Redis receives them. It
// does not change checkpoint code or increase any state/store budget.
type historyStoreGate struct {
	addr        string
	listener    net.Listener
	mu          sync.Mutex
	paused      bool
	held        chan time.Time
	connections []net.Conn
	done        chan struct{}
	once        sync.Once
	wg          sync.WaitGroup
}

func newHistoryStoreGate(t *testing.T, upstream string) *historyStoreGate {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	g := &historyStoreGate{addr: l.Addr().String(), listener: l, done: make(chan struct{}), held: make(chan time.Time, 16)}
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		for {
			client, err := l.Accept()
			if err != nil {
				return
			}
			server, err := net.Dial("tcp", upstream)
			if err != nil {
				_ = client.Close()
				continue
			}
			g.mu.Lock()
			select {
			case <-g.done:
				g.mu.Unlock()
				_ = client.Close()
				_ = server.Close()
				return
			default:
			}
			g.connections = append(g.connections, client, server)
			g.mu.Unlock()
			g.wg.Add(2)
			go func() { defer g.wg.Done(); defer func() { _ = client.Close() }(); _, _ = io.Copy(client, server) }()
			go func() {
				defer g.wg.Done()
				defer func() { _ = server.Close() }()
				_, _ = io.Copy(&historyGateWriter{gate: g, dest: server}, client)
			}()
		}
	}()
	t.Cleanup(g.close)
	return g
}
func (g *historyStoreGate) pause() { g.mu.Lock(); g.paused = true; g.mu.Unlock() }

// heldAfter ignores writes already held before the candidate was saved. Each
// timestamp is captured only when Write receives nonempty bytes while paused.
func (g *historyStoreGate) heldAfter(ctx context.Context, after time.Time) (time.Time, error) {
	for {
		select {
		case at := <-g.held:
			if at.After(after) {
				return at, nil
			}
		case <-ctx.Done():
			return time.Time{}, fmt.Errorf("no post-candidate write held: %w", ctx.Err())
		case <-g.done:
			return time.Time{}, net.ErrClosed
		}
	}
}
func (g *historyStoreGate) close() {
	g.once.Do(func() {
		_ = g.listener.Close()
		close(g.done)
		g.mu.Lock()
		for _, c := range g.connections {
			_ = c.Close()
		}
		g.mu.Unlock()
		g.wg.Wait()
	})
}

type historyGateWriter struct {
	gate *historyStoreGate
	dest net.Conn
}

func (w *historyGateWriter) Write(b []byte) (int, error) {
	arrivedAt := time.Now()
	w.gate.mu.Lock()
	if w.gate.paused {
		if len(b) > 0 {
			select {
			case w.gate.held <- arrivedAt:
			default:
			}
		}
		w.gate.mu.Unlock()
		<-w.gate.done
		return 0, net.ErrClosed
	}
	defer w.gate.mu.Unlock()
	if err := w.dest.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		return 0, err
	}
	return w.dest.Write(b)
}

var _ voice.Provider = (*localHistoryProvider)(nil)
var _ agent.MoveOnlyState = (*observedVoice)(nil)
