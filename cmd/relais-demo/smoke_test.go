package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/relais/internal/clusterprocess"
	"github.com/relais/internal/privateapi"
	"github.com/relais/internal/processrun"
	agentdemo "github.com/relais/pkg/agent/demo"
	"github.com/relais/pkg/callharness"
	"github.com/relais/pkg/controlplane"
	"github.com/stretchr/testify/require"
)

// Use a second IPv4 loopback address when the host supports it. Otherwise
// probe private HTTP sockets on ::1; that probe does not require IPv6 media.
func TestDemoLocalNetworkSmoke(t *testing.T) {
	if os.Getenv("RELAIS_DEMO_SMOKE") != "1" {
		t.Skip("opt-in: make build, then RELAIS_DEMO_SMOKE=1 go test -run TestDemoLocalNetworkSmoke -v ./cmd/relais-demo")
	}
	publicHost, isolationHost := "127.0.0.2", "127.0.0.2"
	probe, err := net.Listen("tcp4", net.JoinHostPort(publicHost, "0"))
	if err != nil {
		publicHost, isolationHost = "127.0.0.1", "::1"
		t.Log("second IPv4 loopback address unavailable; private-API isolation probe uses ::1 HTTP only")
	} else {
		require.NoError(t, probe.Close())
	}
	cfg := config{Bin: filepath.Join("..", "..", "bin"), RedisBinary: "redis-server", HTTP: "127.0.0.1:0", LocalNetwork: publicHost}
	listener, fingerprint, err := listenPage(cfg)
	require.NoError(t, err)
	defer func() { _ = listener.Close() }()
	manager := &clusterprocess.Manager{}
	ctx, stop := manager.Context()
	defer stop()
	d, err := launch(ctx, manager, cfg)
	require.NoError(t, err)
	mediaHost, _, err := net.SplitHostPort(d.relay)
	require.NoError(t, err)
	require.Equal(t, publicHost, mediaHost)
	files, err := fs.Sub(web, "web")
	require.NoError(t, err)
	served := make(chan error, 1)
	go func() { served <- processrun.Serve(ctx, listener, d.handler(http.FileServer(http.FS(files)))) }()
	defer func() { stop(); require.NoError(t, <-served) }()
	transport := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec // owned ephemeral certificate
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	response, err := client.Get("https://" + listener.Addr().String() + "/")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.NotNil(t, response.TLS)
	require.NoError(t, response.TLS.PeerCertificates[0].VerifyHostname(publicHost))
	require.NotEmpty(t, fingerprint)
	_ = response.Body.Close()
	require.NotEmpty(t, d.launchToken)
	for _, token := range []string{"", "wrong", d.launchToken} {
		req, err := http.NewRequest(http.MethodPost, "https://"+listener.Addr().String()+"/", nil)
		require.NoError(t, err)
		req.Header.Set("X-Relais-Demo-Token", token)
		out, err := client.Do(req)
		require.NoError(t, err)
		want := http.StatusForbidden
		if token == d.launchToken {
			want = http.StatusOK // static page reached
		}
		require.Equal(t, want, out.StatusCode)
		_ = out.Body.Close()
	}
	// Check readiness from the real production-launched binaries, not just
	// flags. Their private APIs must bind only 127.0.0.1 and refuse isolationHost.
	logs, err := filepath.Glob(filepath.Join(d.dir, "*.log"))
	require.NoError(t, err)
	privateCount := 0
	for _, path := range logs {
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		for _, line := range strings.Split(string(data), "\n") {
			var ready clusterprocess.Ready
			if json.Unmarshal([]byte(line), &ready) != nil || ready.HTTP == "" {
				continue
			}
			privateCount++
			host, port, err := net.SplitHostPort(strings.TrimPrefix(ready.HTTP, "http://"))
			require.NoError(t, err)
			require.Equal(t, "127.0.0.1", host)
			conn, dialErr := net.DialTimeout("tcp", net.JoinHostPort(isolationHost, port), 100*time.Millisecond)
			if conn != nil {
				_ = conn.Close()
			}
			require.Error(t, dialErr, "private API exposed on isolation address %s", isolationHost)
			if ready.Leg != "" {
				legHost, _, err := net.SplitHostPort(ready.Leg)
				require.NoError(t, err)
				require.Equal(t, "127.0.0.1", legHost)
			}
		}
	}
	require.Equal(t, 5, privateCount, "relay, control, and three workers")
	t.Logf("TLS page %s, public media %s; all %d private APIs refuse %s", listener.Addr(), d.relay, privateCount, isolationHost)
}

// Opt-in real process smoke; ordinary tests do not require built binaries or
// Redis. It drives the launcher's production endpoints with the external Pion
// caller. Browser presentation gaps and the 60 s browser hold remain NOT-RUN.
func TestDemoExternalSmoke(t *testing.T)      { runExternalSmoke(t, "echo") }
func TestDemoAgentExternalSmoke(t *testing.T) { runExternalSmoke(t, "demo") }
func runExternalSmoke(t *testing.T, agentName string) {
	if os.Getenv("RELAIS_DEMO_SMOKE") != "1" {
		t.Skip("opt-in: make build, then RELAIS_DEMO_SMOKE=1 go test -run TestDemoExternalSmoke -v ./cmd/relais-demo")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	manager := &clusterprocess.Manager{}
	defer manager.Stop(false)
	d, err := launch(ctx, manager, config{Bin: filepath.Join("..", "..", "bin"), RedisBinary: "redis-server", Agent: agentName})
	require.NoError(t, err)
	// Retain all started process identities and assert they're reaped by cleanup.
	var children []*clusterprocess.Child
	d.mu.Lock()
	for _, w := range d.workers {
		children = append(children, w.child)
	}
	d.mu.Unlock()
	defer func() {
		manager.Stop(false)
		for _, child := range children {
			select {
			case <-child.Done():
			default:
				t.Errorf("child %d not reaped", child.PID())
			}
		}
		conn, err := net.DialTimeout("tcp", d.redis, 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			t.Errorf("throwaway Redis %s survived cleanup", d.redis)
		}
	}()
	files, err := fs.Sub(web, "web")
	require.NoError(t, err)
	server := httptest.NewServer(d.handler(http.FileServer(http.FS(files))))
	defer server.Close()
	response, err := server.Client().Get(server.URL + "/")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	_ = response.Body.Close()
	relay, err := netip.ParseAddrPort(d.relay)
	require.NoError(t, err)
	h, err := callharness.Start(callharness.Options{External: &callharness.ExternalTopology{SignalingURL: server.URL + "/calls", RelayAddr: relay}})
	require.NoError(t, err)
	defer func() { _ = h.Close() }()
	call, err := h.Dial(ctx, callharness.CallOptions{Video: true, Worker: -1})
	require.NoError(t, err)
	media, stopMedia := context.WithCancel(ctx)
	defer stopMedia()
	sent := make(chan error, 1)
	go func() { sent <- call.SendMedia(media, 35*time.Second) }()
	defer func() { stopMedia(); <-sent }()
	wait := func() {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Second):
		}
	}
	wait()
	wait()
	var status demoStatus
	read := func() {
		status = demoStatus{}
		require.NoError(t, privateapi.Do(ctx, server.Client(), server.URL, http.MethodGet, "/demo/status", nil, &status, nil))
		require.Len(t, status.Calls, 1)
		require.Len(t, status.WorkerPIDs, 3)
	}
	read()
	require.Equal(t, agentName, status.SelectedAgent)
	evidenceRecords := []json.RawMessage{}
	checkAgent := func(kind string, evidence *agentContinuity) {
		if agentName != "demo" {
			return
		}
		require.NotNil(t, evidence)
		require.Empty(t, evidence.Errors)
		require.NotNil(t, evidence.Before)
		require.NotNil(t, evidence.After)
		require.NotNil(t, evidence.After.LastResume)
		var observed agentdemo.Status
		require.NoError(t, privateapi.Do(ctx, server.Client(), server.URL, http.MethodGet, "/demo/agent/"+call.SessionID(), nil, &observed, nil))
		payload, err := json.Marshal(struct {
			*agentContinuity
			Observed agentdemo.Status `json:"observed_after"`
		}{evidence, observed})
		require.NoError(t, err)
		command := exec.Command("node", "-e", `const A=require('./web/continuity.js');let s='';process.stdin.on('data',x=>s+=x);process.stdin.on('end',()=>{const verdict=A.verdict(process.argv[1],JSON.parse(s));console.log(JSON.stringify(verdict));if(verdict.status!=='pass')process.exitCode=1;});`, kind)
		command.Stdin = bytes.NewReader(payload)
		output, err := command.CombinedOutput()
		require.NoError(t, err, "page continuity verdict: %s", output)
		t.Logf("DEMO_AGENT_%s %s", kind, output)
		evidenceRecords = append(evidenceRecords, json.RawMessage(payload))
	}
	original := status.Calls[0].Owner
	var move struct {
		controlplane.MoveResult
		Agent *agentContinuity `json:"agent_continuity"`
	}
	require.NoError(t, privateapi.Do(ctx, server.Client(), server.URL, http.MethodPost, "/calls/"+call.SessionID()+"/move", nil, &move, nil))
	require.Empty(t, move.Error)
	read()
	require.NotEqual(t, original, status.Calls[0].Owner)
	require.Equal(t, move.To, status.Calls[0].Owner)
	t.Logf("move: %s -> %s, epoch=%d moves=%d", original, status.Calls[0].Owner, status.Calls[0].Epoch, status.Calls[0].MoveCount)
	wait()
	checkAgent("move", move.Agent)
	var drain actionResult
	require.NoError(t, privateapi.Do(ctx, server.Client(), server.URL, http.MethodPost, "/demo/drain", map[string]string{"id": call.SessionID()}, &drain, nil))
	require.Empty(t, drain.Error)
	require.NotEqual(t, drain.From, drain.To)
	read()
	require.Equal(t, drain.To, status.Calls[0].Owner)
	require.Contains(t, status.WorkerPIDs, drain.Replacement)
	t.Logf("drain: %s -> %s; replacement %s pid=%d", drain.From, drain.To, drain.Replacement, drain.ReplacementPID)
	wait()
	checkAgent("drain", drain.Agent)
	replacements := map[string]bool{drain.Replacement: true}
	killedReplacement := false
	for i := 0; i < 3; i++ {
		var kill actionResult
		require.NoError(t, privateapi.Do(ctx, server.Client(), server.URL, http.MethodPost, "/demo/kill", map[string]string{"id": call.SessionID()}, &kill, nil))
		h.RecordProcessKill(kill.From, kill.At)
		killedReplacement = killedReplacement || replacements[kill.From]
		replacements[kill.Replacement] = true
		require.Empty(t, kill.Error)
		read()
		require.Equal(t, kill.To, status.Calls[0].Owner)
		require.Contains(t, status.WorkerPIDs, kill.Replacement)
		require.Equal(t, uint64(i+1), status.Calls[0].TakeoverCount)
		require.NotContains(t, status.WorkerPIDs, kill.From)
		wait()
		checkAgent("kill", kill.Agent)
		read()
		for _, w := range status.Workers {
			if _, live := status.WorkerPIDs[w.Name]; live {
				require.False(t, w.Dead, "live replacement %s marked dead", w.Name)
			}
		}
		t.Logf("kill %d: %s -> %s; replacement %s", i+1, kill.From, kill.To, kill.Replacement)
	}
	require.True(t, killedReplacement, "replacement must own the call and then lose it")
	wait()
	wait()
	observed := time.Now()
	report, err := call.Hangup(ctx)
	require.NoError(t, err)
	require.True(t, report.ConnectedThroughout())
	require.Equal(t, 0, report.ICERestarts)
	require.Equal(t, 0, report.Renegotiations)
	require.Equal(t, 1, report.OfferAnswerExchanges)
	require.Equal(t, d.relay, report.RemoteAddr)
	require.Len(t, report.Tracks, 2)
	for _, track := range report.Tracks {
		require.Greater(t, track.Packets, 0)
		require.Less(t, observed.Sub(report.StartedAt.Add(track.LastArrival)), 200*time.Millisecond, "%s echo did not continue after kill", track.Kind)
	}
	video := report.Track("video")
	require.NotNil(t, video)
	require.NotNil(t, video.Video)
	require.True(t, video.Video.FullDecode.Ran())
	require.Empty(t, video.Video.FullDecode.Errors)
	require.Equal(t, video.Video.FullDecode.FramesIn, video.Video.FullDecode.FramesDecoded)
	t.Log(report.Summary())
	if agentName == "demo" {
		audio := report.Track("audio")
		require.NotNil(t, audio)
		require.Equal(t, audio.Packets, audio.UnmatchedPayloads)
		evidence, err := json.MarshalIndent(evidenceRecords, "", "  ")
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join("..", "..", "bin", "w47-agent-process-evidence.json"), evidence, 0600))
	}
	d.mu.Lock()
	for _, w := range d.workers {
		children = append(children, w.child)
	}
	d.mu.Unlock()
}
