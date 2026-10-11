package demo

import (
	"bytes"
	"context"
	"encoding/binary"
	"os/exec"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4/pkg/media/oggwriter"
	"github.com/relais/pkg/agent"
	"github.com/stretchr/testify/require"
)

// Decode the production output, including chimes and repacketized 60/120 ms
// packets. A well-formed RTP stream alone is not evidence that tones decode.
func TestAudioOutputDecodes(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable; Opus decode NOT-RUN")
	}
	for _, frames := range []int{1, 3, 6} {
		t.Run(string(rune('0'+frames)), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var ogg bytes.Buffer
			writer, err := oggwriter.NewWith(&ogg, 48000, 1)
			require.NoError(t, err)
			a := New()
			total := 0
			for n := uint64(1); n <= 260; n++ {
				in := input(n)
				if frames > 1 {
					in.Payload = []byte{0xfb, byte(frames), 1}
				}
				out, err := a.Process(ctx, in)
				require.NoError(t, err)
				require.NoError(t, writer.WriteRTP(&rtp.Packet{Header: rtp.Header{Version: 2, SSRC: 123, SequenceNumber: uint16(n), Timestamp: uint32(total)}, Payload: out.Audio}))
				total += frames * FrameSamples
				if n == 100 {
					require.NoError(t, a.Resume(ctx, agent.ResumeNotice{Kind: agent.PlannedMove, Progress: in.Progress}))
				}
			}
			require.NoError(t, writer.Close())
			command := exec.CommandContext(ctx, ffmpeg, "-v", "error", "-i", "pipe:0", "-f", "s16le", "-ac", "1", "-ar", "48000", "pipe:1")
			command.Stdin = bytes.NewReader(ogg.Bytes())
			var stderr bytes.Buffer
			command.Stderr = &stderr
			pcm, err := command.Output()
			require.NoError(t, err, "decode: %s", stderr.String())
			require.Empty(t, stderr.String())
			// The Ogg writer declares the standard 3840-sample preskip. All other
			// samples must decode, including every committed count and the resume.
			require.Equal(t, total-3840, len(pcm)/2)
			peak := 0
			for i := 0; i < len(pcm); i += 2 {
				sample := int(int16(binary.LittleEndian.Uint16(pcm[i:])))
				if sample < 0 {
					sample = -sample
				}
				peak = max(peak, sample)
			}
			require.Greater(t, peak, 1000, "output must be audible rather than silence")
			t.Logf("frames_per_packet=%d decoded_samples=%d peak=%d", frames, len(pcm)/2, peak)
		})
	}
}
