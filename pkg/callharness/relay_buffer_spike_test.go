package callharness

import (
	"context"
	"testing"
	"time"

	"github.com/relais/pkg/mediaworker"
	"github.com/stretchr/testify/require"
)

// Spike the cryptographic/codec boundary before building the rolling cache.
// The existing planned hold supplies real caller ciphertext without exposing
// keys. Automatic recovery is stopped so only this protocol drives adoption.
func TestRelayBufferSpike(t *testing.T) {
	h, err := Start(Options{Relay: true, Workers: 2, DisableFrameCache: true, DisableResumePLI: true})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, h.Close()) })
	h.workers.relay.cancel()
	<-h.workers.relay.done
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	call, err := h.Dial(ctx, CallOptions{Video: true})
	require.NoError(t, err)
	sent := make(chan error, 1)
	go func() { sent <- call.SendMedia(ctx, 3*time.Second) }()
	time.Sleep(600 * time.Millisecond)
	topology := h.workers.relay
	a, b := h.workers.list[0], h.workers.list[1]
	require.NoError(t, topology.relay.HoldSession(ctx, call.SessionID(), a.LocalAddr()))
	state, err := topology.owners.GetState(ctx, call.SessionID())
	require.NoError(t, err)
	lease, err := topology.owners.Get(ctx, call.SessionID())
	require.NoError(t, err)
	require.NoError(t, h.Kill(0))
	time.Sleep(400 * time.Millisecond)
	lease, err = topology.owners.Transfer(ctx, lease, b.LocalAddr(), 3*time.Second)
	require.NoError(t, err)
	require.NoError(t, topology.relay.MoveSession(call.SessionID(), a.LocalAddr(), b.LocalAddr()))
	_, err = b.ResumeSession(state, mediaworker.ResumeOptions{Context: ctx, Lease: lease, SequenceMargin: 8192, SRTCPIndexMargin: 128})
	require.NoError(t, err)
	count, err := topology.relay.ReleaseSession(call.SessionID(), b.LocalAddr())
	require.NoError(t, err)
	require.Positive(t, count)
	require.NoError(t, <-sent)
	workerFailures, err := b.SessionDecryptFailures(call.SessionID())
	require.NoError(t, err)
	report, err := call.Hangup(ctx)
	require.NoError(t, err)
	require.Zero(t, workerFailures)
	require.Zero(t, report.DecryptionFailures.Total())
	require.Zero(t, report.Track("audio").DuplicatePackets)
	require.Zero(t, report.Track("video").DuplicatePackets)
	require.Equal(t, report.SentVideo.Frames, report.Track("video").Video.Frames)
	decode := report.Track("video").Video.FullDecode
	require.Empty(t, decode.Skipped)
	require.Empty(t, decode.Errors)
	require.Equal(t, decode.FramesIn, decode.FramesDecoded)
	t.Logf("RELAY_BUFFER_SPIKE held_returned=%d audio_returned=%d video_frames=%d/%d ffmpeg_decoded=%d worker_decrypt_failures=%d caller_decrypt_failures=%d", count, report.Track("audio").Packets, report.Track("video").Video.Frames, report.SentVideo.Frames, decode.FramesDecoded, workerFailures, report.DecryptionFailures.Total())
}
