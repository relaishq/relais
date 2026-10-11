package callharness

import (
	"context"
	"fmt"
	"image"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Exercise the production ffmpeg invocation with controlled framecrc output.
// A successful process that silently omits a frame is trusted decode failure;
// a process that cannot run still provides no evidence about the bitstream.
func TestFullDecodeFrameCountMismatch(t *testing.T) {
	for _, test := range []struct {
		name    string
		decoded int
		exit    int
		failure DecodeFailure
	}{
		{"complete", 2, 0, ""},
		{"fewer frames", 1, 0, DecodeInvalid},
		{"extra frames", 3, 0, DecodeInvalid},
		{"execution failure", 0, 2, DecodeUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			output := strings.Repeat("0, 0, 0, 1, 100, 0x12345678\n", test.decoded)
			script := fmt.Sprintf("#!/bin/sh\n/bin/cat >/dev/null\nprintf '%%s' '%s'\nexit %d\n", output, test.exit)
			require.NoError(t, os.WriteFile(filepath.Join(dir, "ffmpeg"), []byte(script), 0700))
			t.Setenv("PATH", dir)
			frames := []vp8Frame{{data: []byte{1}, keyframe: true}, {data: []byte{2}, timestamp: 3000}}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			result := fullDecode(ctx, frames, image.Point{X: 1, Y: 1})
			require.NoError(t, ctx.Err())
			require.Equal(t, 2, result.FramesIn)
			require.Equal(t, test.decoded, result.FramesDecoded)
			require.Equal(t, test.failure, result.Failure, "%+v", result)
			require.Empty(t, result.Skipped)
			if test.exit == 0 {
				require.Empty(t, result.Errors, "decoder printed no error")
			}

			r := testMeasurementRecorder(t)
			track := r.tracks[0]
			r.recording.DecodeEvery = 30
			r.recordOnlineDecode(track, sampleJob{frames: frames, at: 1850 * time.Millisecond, sourceAt: 1848 * time.Millisecond}, result)
			if test.failure == "" {
				require.Equal(t, 1, track.video.OnlineDecode.Decoded)
				return
			}
			m := r.eventMeasurement(testMeasurementMove(), r.hungUpAt, 0)
			for _, verdict := range []MetricVerdict{m.FirstContentVerdict, m.FirstNewContentVerdict, m.FirstLiveVerdict} {
				require.Equal(t, test.failure == DecodeInvalid, verdict.Trusted, verdict.Summary())
				require.Equal(t, test.failure == DecodeInvalid, verdict.Failed, verdict.Summary())
			}
		})
	}
}
