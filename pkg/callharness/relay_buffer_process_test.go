package callharness

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/relais/pkg/controlplane"
	"github.com/stretchr/testify/require"
)

// crash-run compiles and launches only this opt-in caller test. The test keeps
// #27's exact packet/unique-frame observations inside the harness, without
// editing the recorder or the report files concurrently owned by #32. Relay,
// workers and control plane are ordinary standalone binaries and private APIs.
type bufferProcessConfig struct {
	Dir, SignalingURL, RelayHTTP, RelayMedia, KillURL string
	After, Warmup                                     time.Duration
	BufferOff                                         bool
}

type bufferProcessEvidence struct {
	AudioSent, AudioReturned, AudioLost int
	VideoSent, VideoReturned, VideoLost int
	FirstContent                        string
	FirstContentAfterKill               time.Duration
	RelayReplayPackets                  int
	RelayReplayDuration                 time.Duration
	Complete                            bool
	GateReleased                        bool
	UniqueDecodedFrames                 int
	KeyframeRequests                    int
}

func TestRelayBufferProcessCaller(t *testing.T) {
	configPath := os.Getenv("RELAIS_BUFFER_PROCESS_CONFIG")
	if configPath == "" {
		t.Skip("caller entrypoint is launched by crash-run -buffer or -compare-buffer")
	}
	data, err := os.ReadFile(configPath)
	require.NoError(t, err)
	var cfg bufferProcessConfig
	require.NoError(t, json.Unmarshal(data, &cfg))
	ctx, cancel := context.WithTimeout(context.Background(), cfg.After+20*time.Second)
	t.Cleanup(cancel)
	relayAddr, err := netip.ParseAddrPort(cfg.RelayMedia)
	require.NoError(t, err)
	h, err := Start(Options{External: &ExternalTopology{SignalingURL: cfg.SignalingURL, RelayAddr: relayAddr}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, h.Close()) })
	call, err := h.Dial(ctx, CallOptions{Video: true, Worker: -1, InitialSequenceNumbers: &RTPSequenceNumbers{Audio: 1000, Video: 2000}})
	require.NoError(t, err)
	o := observeBufferCall(call)
	mediaCtx, stopMedia := context.WithCancel(ctx)
	sent := make(chan error, 1)
	go func() { sent <- call.SendMedia(mediaCtx, cfg.After+15*time.Second) }()
	defer func() { stopMedia(); require.ErrorIs(t, <-sent, context.Canceled) }()
	time.Sleep(cfg.Warmup)
	status, err := h.Status(ctx)
	require.NoError(t, err)
	require.Len(t, status.Calls, 1)
	owner := status.Calls[0].Owner
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.KillURL+"/kill/"+owner, nil)
	require.NoError(t, err)
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var kill struct {
		At  time.Time
		PID int
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&kill))
	killed := kill.At
	h.RecordProcessKill(owner, killed)
	t.Logf("SIGKILL worker=%s pid=%d session=%s relay=%s", owner, kill.PID, call.SessionID(), cfg.RelayMedia)
	status = waitBufferTakeovers(t, ctx, h, 1)
	event := status.Takeovers[0]
	require.False(t, event.Lost)
	require.Empty(t, event.Error)
	time.Sleep(cfg.After)
	stopMedia()
	report, err := call.Hangup(ctx)
	require.NoError(t, err)
	remote := &controlplane.RemoteRelay{URL: cfg.RelayHTTP}
	relayStatus, err := remote.Status(ctx)
	require.NoError(t, err)
	require.Zero(t, relayStatus.Stats.Holds)
	require.Zero(t, relayStatus.Stats.HeldBytes)
	require.Zero(t, report.DecryptionFailures.Total())
	require.Zero(t, report.ICERestarts)
	require.Zero(t, report.Renegotiations)
	require.True(t, report.ConnectedThroughout())
	require.Equal(t, 1, report.OfferAnswerExchanges)
	require.Equal(t, cfg.RelayMedia, report.RemoteAddr)
	require.Positive(t, report.Consent.ResponsesAfter)
	if cfg.After >= 60*time.Second {
		require.GreaterOrEqual(t, report.Consent.ObservedFor, 60*time.Second)
	}
	first, delay := firstBufferContent(call, report, killed, event.End)
	var evidence bufferProcessEvidence
	evidence.FirstContent, evidence.FirstContentAfterKill = first, delay
	evidence.RelayReplayPackets = event.Result.RelayReplayPackets
	evidence.RelayReplayDuration = event.Result.RelayReplayDuration
	evidence.Complete, evidence.GateReleased = event.Result.RelayReplayComplete, relayStatus.Stats.Holds == 0
	evidence.KeyframeRequests = report.SentVideo.KeyframeRequests
	// Partition the same exact-index observation by track; duplicate returned
	// packets cannot cover up a missing caller packet.
	for _, pt := range []uint8{opusPayloadType, vp8PayloadType} {
		sentCount, lost := bufferPacketLoss(t, o, killed, event.End, pt)
		if pt == opusPayloadType {
			evidence.AudioSent, evidence.AudioReturned, evidence.AudioLost = sentCount, sentCount-lost, lost
		} else {
			evidence.VideoSent, evidence.VideoReturned, evidence.VideoLost = sentCount, sentCount-lost, lost
		}
	}
	decode := decodeBufferUnique(t, ctx, call, report, !cfg.BufferOff)
	evidence.UniqueDecodedFrames = decode.FramesDecoded
	gap := time.Duration(0)
	for _, kind := range []string{"audio", "video"} {
		track := report.Track(kind)
		require.NotNil(t, track)
		require.Zero(t, track.UnmatchedPayloads)
		if track.Video != nil {
			require.Zero(t, track.Video.UnmatchedFrames)
			require.Zero(t, track.Video.KeyframeDecodeErrors)
		}
		require.Greater(t, track.LastArrival, event.End.Sub(report.StartedAt))
		gap = max(gap, track.MediaGap)
	}
	require.Less(t, gap, 2*time.Second)
	if !cfg.BufferOff {
		require.True(t, evidence.Complete)
		require.Positive(t, evidence.RelayReplayPackets)
		require.Positive(t, evidence.AudioSent)
		require.Positive(t, evidence.VideoSent)
		require.Zero(t, evidence.AudioLost)
		require.Zero(t, evidence.VideoLost)
		require.Zero(t, evidence.KeyframeRequests)
		require.Equal(t, "Outage", first)
		require.Zero(t, relayStatus.Stats.HoldDrops)
		require.Zero(t, relayStatus.Stats.HoldSendFailures)
	} else {
		require.Positive(t, evidence.AudioLost)
		require.Positive(t, evidence.VideoLost)
		require.Zero(t, evidence.RelayReplayPackets)
	}
	require.Len(t, report.Moves, 1)
	move := report.Moves[0]
	result := map[string]any{"Gap": gap, "Decrypt": report.DecryptionFailures.Total(), "AfterResume": move.DecryptionFailuresAfterResume, "Reconnects": report.ICERestarts, "Renegotiations": report.Renegotiations, "Decoded": move.Recovery.FirstDecodedAfterKill, "Live": move.Recovery.FirstDecodedLiveAfterKill, "Detection": move.DetectionTime, "Pass": true, "Path": move.Recovery.Path, "LivePath": move.Recovery.LivePath, "ReplayPackets": move.Recovery.ReplayPackets, "BufferEvidence": evidence}
	encoded, err := json.Marshal(result)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(cfg.Dir, "result.json"), encoded, 0600))
	t.Logf("BUFFER_PROCESS_METRICS %s", encoded)
	fmt.Println(report.Summary())
}
