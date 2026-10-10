package main

import (
	"context"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/relais/internal/clusterprocess"
	"github.com/relais/internal/privateapi"
	"github.com/relais/pkg/callharness"
	"github.com/relais/pkg/controlplane"
	"github.com/stretchr/testify/require"
)

// Opt-in real process smoke; ordinary tests do not require built binaries or
// Redis. It drives the launcher's production endpoints with the external Pion
// caller. Browser presentation gaps and the 60 s browser hold remain NOT-RUN.
func TestDemoExternalSmoke(t *testing.T) {
	if os.Getenv("RELAIS_DEMO_SMOKE") != "1" {
		t.Skip("opt-in: make build, then RELAIS_DEMO_SMOKE=1 go test -run TestDemoExternalSmoke -v ./cmd/relais-demo")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	manager := &clusterprocess.Manager{}
	defer manager.Stop(false)
	d, err := launch(ctx, manager, config{Bin: filepath.Join("..", "..", "bin"), RedisBinary: "redis-server"})
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
	original := status.Calls[0].Owner
	var move controlplane.MoveResult
	require.NoError(t, privateapi.Do(ctx, server.Client(), server.URL, http.MethodPost, "/calls/"+call.SessionID()+"/move", nil, &move, nil))
	require.Empty(t, move.Error)
	read()
	require.NotEqual(t, original, status.Calls[0].Owner)
	require.Equal(t, move.To, status.Calls[0].Owner)
	t.Logf("move: %s -> %s, epoch=%d moves=%d", original, status.Calls[0].Owner, status.Calls[0].Epoch, status.Calls[0].MoveCount)
	wait()
	var drain actionResult
	require.NoError(t, privateapi.Do(ctx, server.Client(), server.URL, http.MethodPost, "/demo/drain", map[string]string{"id": call.SessionID()}, &drain, nil))
	require.Empty(t, drain.Error)
	require.NotEqual(t, drain.From, drain.To)
	read()
	require.Equal(t, drain.To, status.Calls[0].Owner)
	require.Contains(t, status.WorkerPIDs, drain.Replacement)
	t.Logf("drain: %s -> %s; replacement %s pid=%d", drain.From, drain.To, drain.Replacement, drain.ReplacementPID)
	wait()
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
	d.mu.Lock()
	for _, w := range d.workers {
		children = append(children, w.child)
	}
	d.mu.Unlock()
}
