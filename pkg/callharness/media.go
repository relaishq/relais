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
