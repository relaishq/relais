package callharness

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"time"

	// Embedding the test media keeps the harness usable from any package's
	// tests, whatever their working directory.
	_ "embed"

	"github.com/pion/webrtc/v4/pkg/media/ivfreader"
	"github.com/pion/webrtc/v4/pkg/media/oggreader"
)

// callerAudio is about 5 s of pre-encoded Opus (mono, 24 kb/s, 20 ms frames,
// one frame per Ogg page) that the caller plays in a loop. It was generated
// with:
//
//	ffmpeg -f lavfi -i "aevalsrc=0.4*sin(2*PI*(220+110*t)*t):s=48000:d=5" \
//	  -ac 1 -c:a libopus -b:a 24k -frame_duration 20 -application audio \
//	  -page_duration 20000 -map_metadata -1 -fflags +bitexact -flags:a +bitexact \
//	  testdata/caller-audio.ogg
//
//go:embed testdata/caller-audio.ogg
var callerAudio []byte

// opusFrameDuration is the pacing interval for sending callerAudio.
const opusFrameDuration = 20 * time.Millisecond

const opusSampleRate = 48000

// opusSource reads Opus frames from an Ogg file in the style of Pion's
// play-from-disk example, looping at the end of the file.
type opusSource struct {
	data        []byte
	reader      *oggreader.OggReader
	lastGranule uint64
}

func newOpusSource(data []byte) (*opusSource, error) {
	src := &opusSource{data: data}
	if err := src.rewind(); err != nil {
		return nil, err
	}

	return src, nil
}

func (s *opusSource) rewind() error {
	reader, _, err := oggreader.NewWith(bytes.NewReader(s.data))
	if err != nil {
		return fmt.Errorf("callharness: open Ogg: %w", err)
	}
	s.reader = reader
	s.lastGranule = 0

	return nil
}

// next returns the next Opus frame and its duration.
func (s *opusSource) next() ([]byte, time.Duration, error) {
	for rewinds := 0; rewinds < 2; {
		page, header, err := s.reader.ParseNextPage()
		if errors.Is(err, io.EOF) {
			rewinds++
			if err := s.rewind(); err != nil {
				return nil, 0, err
			}

			continue
		}
		if err != nil {
			return nil, 0, fmt.Errorf("callharness: read Ogg page: %w", err)
		}
		if _, isHeader := header.HeaderType(page); isHeader {
			continue // OpusTags
		}

		samples := header.GranulePosition - s.lastGranule
		s.lastGranule = header.GranulePosition

		return page, time.Duration(samples) * time.Second / opusSampleRate, nil
	}

	return nil, 0, errors.New("callharness: Ogg file has no Opus frames")
}

// callerVideo is 5 s of pre-encoded VP8 (320x240, 30 fps, about 160 kb/s, a
// keyframe every 30 frames, no alt-ref frames so every frame is shown) that
// the caller plays in a loop. It was generated with:
//
//	ffmpeg -f lavfi -i "testsrc2=size=320x240:rate=30:duration=5" \
//	  -pix_fmt yuv420p -c:v libvpx -b:v 160k -g 30 -keyint_min 30 \
//	  -auto-alt-ref 0 -lag-in-frames 0 -deadline realtime -cpu-used 8 -error-resilient 1 \
//	  -map_metadata -1 -fflags +bitexact -flags:v +bitexact \
//	  testdata/caller-video.ivf
//
//go:embed testdata/caller-video.ivf
var callerVideo []byte

const vp8ClockRate = 90000

// vp8Source reads VP8 frames from an IVF file in the style of Pion's
// play-from-disk example, looping at the end of the file. Every IVF frame of
// callerVideo is one shown frame, sent as one media sample.
type vp8Source struct {
	data          []byte
	reader        *ivfreader.IVFReader
	frameDuration time.Duration
}

func newVP8Source(data []byte) (*vp8Source, error) {
	src := &vp8Source{data: data}
	if err := src.rewind(); err != nil {
		return nil, err
	}

	return src, nil
}

// rewind restarts the file, whose first frame is a keyframe.
func (s *vp8Source) rewind() error {
	reader, header, err := ivfreader.NewWith(bytes.NewReader(s.data))
	if err != nil {
		return fmt.Errorf("callharness: open IVF: %w", err)
	}
	if header.FourCC != "VP80" {
		return fmt.Errorf("callharness: IVF codec %q, want VP80", header.FourCC)
	}
	if header.TimebaseDenominator == 0 || header.TimebaseNumerator == 0 {
		return errors.New("callharness: IVF has no frame rate")
	}
	s.reader = reader
	s.frameDuration = time.Second * time.Duration(header.TimebaseNumerator) / time.Duration(header.TimebaseDenominator)

	return nil
}

// next returns the next VP8 frame and whether it is a keyframe.
func (s *vp8Source) next() (frame []byte, keyframe bool, err error) {
	for rewinds := 0; rewinds < 2; {
		frame, _, err := s.reader.ParseNextFrame()
		if errors.Is(err, io.EOF) {
			rewinds++
			if err := s.rewind(); err != nil {
				return nil, false, err
			}

			continue
		}
		if err != nil {
			return nil, false, fmt.Errorf("callharness: read IVF frame: %w", err)
		}
		if len(frame) == 0 {
			continue
		}

		return frame, isVP8Keyframe(frame), nil
	}

	return nil, false, errors.New("callharness: IVF file has no VP8 frames")
}

// isVP8Keyframe reads the key frame flag of a VP8 frame (RFC 6386 section
// 9.1): bit 0 of the first byte is 0 for a keyframe.
func isVP8Keyframe(frame []byte) bool {
	return len(frame) > 0 && frame[0]&1 == 0
}

// keyframeInterval inspects the encoded fixture, independent of PLI rewinds.
func (s *vp8Source) keyframeInterval() time.Duration {
	probe, err := newVP8Source(s.data)
	if err != nil {
		return 0
	}
	first := -1
	for i := 0; i < 10000; i++ {
		frame, _, err := probe.reader.ParseNextFrame()
		if err != nil {
			return 0
		}
		if isVP8Keyframe(frame) {
			if first >= 0 {
				return time.Duration(i-first) * probe.frameDuration
			}
			first = i
		}
	}
	return 0
}
