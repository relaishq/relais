// Package opusio reads bounded Ogg/Opus packets without decoding or re-encoding.
// It implements RFC 3533 framing and the mono/stereo mapping in RFC 7845.
package opusio

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"time"
	"unicode/utf8"

	"github.com/relaishq/relais/internal/opuspacket"
	agent "github.com/relaishq/relais/pkg/agent/v2"
)

var (
	ErrContainer = errors.New("opusio: malformed Ogg stream")
	ErrCRC       = errors.New("opusio: Ogg checksum mismatch")
	ErrHeader    = errors.New("opusio: malformed Opus header")
	// ErrPacket is retained for source compatibility; malformed audio is now discarded.
	ErrPacket  = errors.New("opusio: malformed Opus packet")
	ErrChained = errors.New("opusio: chained or multiplexed streams unsupported")
	ErrMapping = errors.New("opusio: channel mapping unsupported")
	ErrGain    = errors.New("opusio: non-zero output gain unsupported")
	ErrLimit   = errors.New("opusio: packet exceeds 64 KiB bound")
	ErrGranule = errors.New("opusio: granule position unsupported or invalid")
)

const maxPacketBytes = 64 << 10

// Header holds source metadata. All timing uses 48 kHz, regardless of InputRate.
type Header struct {
	Channels  uint8
	InputRate uint32
	PreSkip   uint16
}

// Reader retains one Ogg page (at most 65,307 bytes) and one packet (at most
// 64 KiB). It is not safe for concurrent calls. Next checks cancellation between
// reads; a blocking io.Reader must itself support cancellation/deadlines.
// Packets are returned unchanged, including preroll and final padding. Position
// reports audible time after pre-skip and EOS trim; forwarding cannot trim PCM.
type Reader struct {
	input           io.Reader
	header          Header
	page            [65307]byte
	laces           []byte
	body            []byte
	segment         int
	offset          int
	partial         []byte
	continuing      bool
	serial          uint32
	sequence        uint32
	initialized     bool
	eos             bool
	flags           byte
	granule         uint64
	previousGranule uint64
	decoded         uint64
	position        uint64
	duration        time.Duration
	discarded       uint64
	damagedPage     bool
	failed          error
}

// NewReader reads and validates OpusHead and OpusTags. Input must be a single
// logical stream with channel mapping family zero and zero output gain.
func NewReader(input io.Reader) (*Reader, error) {
	if input == nil {
		return nil, ErrContainer
	}
	r := &Reader{input: input}
	ctx := context.Background()
	head, err := r.packet(ctx)
	if err != nil {
		return nil, err
	}
	if len(head) < 19 || !bytes.Equal(head[:8], []byte("OpusHead")) || head[8] > 15 {
		return nil, ErrHeader
	}
	if head[18] != 0 || head[9] < 1 || head[9] > 2 {
		return nil, ErrMapping
	}
	if head[8] == 1 && len(head) != 19 {
		return nil, ErrHeader
	}
	if binary.LittleEndian.Uint16(head[16:18]) != 0 {
		return nil, ErrGain
	}
	if r.flags != 2 || r.segment != len(r.laces) || r.granule != 0 {
		return nil, ErrHeader
	}
	r.header = Header{Channels: head[9], PreSkip: binary.LittleEndian.Uint16(head[10:12]), InputRate: binary.LittleEndian.Uint32(head[12:16])}
	tags, err := r.packet(ctx)
	if err != nil {
		return nil, err
	}
	if err = validateTags(tags); err != nil {
		return nil, err
	}
	if r.segment != len(r.laces) || r.granule != 0 || r.eos {
		return nil, ErrHeader
	}
	return r, nil
}

func (r *Reader) Header() Header { return r.header }

// DiscardedPackets counts empty or malformed audio packets skipped by Next.
// Header and Ogg framing errors remain fatal. After a discarded packet, timing
// is resynchronized at the next completed page's granule position.
func (r *Reader) DiscardedPackets() uint64 { return r.discarded }

// Position is the audible position after the last returned packet. It accounts
// for pre-skip and final granule trim without changing any encoded packet bytes.
func (r *Reader) Position() time.Duration {
	return time.Duration(r.position/48000)*time.Second + time.Duration(r.position%48000)*time.Second/48000
}

// PacketDuration is the encoded duration of the last returned packet, including
// any pre-skip or final trim. Use it to pace unchanged encoded packets.
func (r *Reader) PacketDuration() time.Duration { return r.duration }

func (r *Reader) Next(ctx context.Context) (agent.EncodedAudio, error) {
	if err := ctx.Err(); err != nil {
		return agent.EncodedAudio{}, err
	}
	if r.failed != nil {
		return agent.EncodedAudio{}, r.failed
	}
	audio, err := r.next(ctx)
	if err != nil {
		r.failed = err
	}
	return audio, err
}
func (r *Reader) next(ctx context.Context) (agent.EncodedAudio, error) {
	var p []byte
	var d time.Duration
	for {
		var err error
		p, err = r.packet(ctx)
		if err != nil {
			return agent.EncodedAudio{}, err
		}
		d, err = opuspacket.Duration(p)
		if err == nil {
			break
		}
		// RFC 7845 section 3: malformed audio is discarded, not a container
		// failure. No duration can be inferred from an invalid TOC sequence.
		r.discarded++
		r.damagedPage = true
		if r.segment == len(r.laces) {
			if err := r.resyncDamagedPage(0); err != nil {
				return agent.EncodedAudio{}, err
			}
		}
	}
	samples := uint64(d) * 48000 / uint64(time.Second)
	if r.decoded > ^uint64(0)-samples {
		return agent.EncodedAudio{}, ErrGranule
	}
	r.decoded += samples
	end := r.decoded
	if r.segment == len(r.laces) && r.damagedPage {
		if err := r.resyncDamagedPage(samples); err != nil {
			return agent.EncodedAudio{}, err
		}
		end = r.decoded
	} else if r.segment == len(r.laces) {
		if r.granule == ^uint64(0) || r.granule < r.previousGranule || r.granule > r.decoded {
			return agent.EncodedAudio{}, ErrGranule
		}
		if !r.eos && r.granule != r.decoded {
			return agent.EncodedAudio{}, ErrGranule
		}
		if r.eos {
			// End trimming may remove samples from the final packet, but cannot
			// retroactively retract packets already returned from this page.
			if r.granule < r.decoded-samples || r.granule < uint64(r.header.PreSkip) {
				return agent.EncodedAudio{}, ErrGranule
			}
			end = r.granule
		}
		r.previousGranule = r.granule
	}
	r.position = 0
	if end > uint64(r.header.PreSkip) {
		r.position = end - uint64(r.header.PreSkip)
	}
	r.duration = d
	return agent.EncodedAudio{Format: "audio/opus", Packets: [][]byte{p}}, nil
}

// Unknown durations create a gap in the local packet clock. A valid page
// granule anchors the next page without inventing samples for discarded audio.
func (r *Reader) resyncDamagedPage(lastSamples uint64) error {
	if r.granule == ^uint64(0) || r.granule < r.previousGranule || (!r.eos && r.granule < r.decoded) || (r.eos && (r.granule < r.decoded-lastSamples || r.granule < uint64(r.header.PreSkip))) {
		return ErrGranule
	}
	r.decoded, r.previousGranule = r.granule, r.granule
	r.damagedPage = false
	return nil
}

func (r *Reader) packet(ctx context.Context) ([]byte, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if r.segment == len(r.laces) {
			if err := r.readPage(ctx); err != nil {
				return nil, err
			}
		}
		for r.segment < len(r.laces) {
			n := int(r.laces[r.segment])
			r.segment++
			if len(r.partial)+n > maxPacketBytes {
				return nil, ErrLimit
			}
			r.partial = append(r.partial, r.body[r.offset:r.offset+n]...)
			r.offset += n
			r.continuing = n == 255
			if n < 255 {
				p := r.partial
				r.partial = nil // caller owns it; next packet cannot mutate a returned packet
				return p, nil
			}
		}
	}
}
func (r *Reader) readPage(ctx context.Context) error {
	if err := readFull(ctx, r.input, r.page[:27]); err != nil {
		if errors.Is(err, io.EOF) && r.eos && !r.continuing {
			return io.EOF
		}
		if errors.Is(err, io.EOF) {
			return io.ErrUnexpectedEOF
		}
		return err
	}
	if r.eos {
		return ErrChained
	}
	h := r.page[:27]
	if string(h[:4]) != "OggS" || h[4] != 0 || h[5]&^byte(7) != 0 {
		return ErrContainer
	}
	serial := binary.LittleEndian.Uint32(h[14:18])
	sequence := binary.LittleEndian.Uint32(h[18:22])
	if !r.initialized {
		if h[5] != 2 || sequence != 0 {
			return ErrContainer
		}
		r.serial = serial
		r.initialized = true
	} else if serial != r.serial || h[5]&2 != 0 {
		return ErrChained
	} else if sequence != r.sequence+1 {
		return ErrContainer
	}
	if (h[5]&1 != 0) != r.continuing {
		return ErrContainer
	}
	r.sequence = sequence
	n := int(h[26])
	r.laces = r.page[27 : 27+n]
	if err := readFull(ctx, r.input, r.laces); err != nil {
		if errors.Is(err, io.EOF) {
			return io.ErrUnexpectedEOF
		}
		return err
	}
	size := 0
	for _, v := range r.laces {
		size += int(v)
	}
	r.body = r.page[27+n : 27+n+size]
	if err := readFull(ctx, r.input, r.body); err != nil {
		if errors.Is(err, io.EOF) {
			return io.ErrUnexpectedEOF
		}
		return err
	}
	checksum := binary.LittleEndian.Uint32(h[22:26])
	clear(h[22:26])
	if crc(r.page[:27+n+size]) != checksum {
		return ErrCRC
	}
	r.flags = h[5]
	r.eos = r.flags&4 != 0
	r.granule = binary.LittleEndian.Uint64(h[6:14])
	r.segment = 0
	r.offset = 0
	if n == 0 {
		if r.eos && !r.continuing && r.granule == r.decoded {
			return nil
		}
		return ErrContainer
	}
	complete := false
	for _, v := range r.laces {
		complete = complete || v < 255
	}
	if !complete && r.granule != ^uint64(0) {
		return ErrGranule
	}
	if r.eos && (r.laces[n-1] == 255 || !complete) {
		return ErrContainer
	}
	return nil
}
func readFull(ctx context.Context, r io.Reader, p []byte) error {
	empty := 0
	total := 0
	for len(p) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := r.Read(p)
		p = p[n:]
		total += n
		if len(p) == 0 {
			return ctx.Err()
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				if total == 0 {
					return io.EOF
				}
				return io.ErrUnexpectedEOF
			}
			return err
		}
		if n == 0 {
			empty++
			if empty >= 100 {
				return io.ErrNoProgress
			}
		} else {
			empty = 0
		}
	}
	return nil
}
func validateTags(p []byte) error {
	if len(p) < 16 || string(p[:8]) != "OpusTags" {
		return ErrHeader
	}
	rest := p[8:]
	take := func() ([]byte, bool) {
		if len(rest) < 4 {
			return nil, false
		}
		n := uint64(binary.LittleEndian.Uint32(rest))
		rest = rest[4:]
		if n > uint64(len(rest)) {
			return nil, false
		}
		b := rest[:int(n)]
		rest = rest[int(n):]
		return b, true
	}
	vendor, ok := take()
	if !ok || !utf8.Valid(vendor) || len(rest) < 4 {
		return ErrHeader
	}
	count := uint64(binary.LittleEndian.Uint32(rest))
	rest = rest[4:]
	if count > uint64(len(rest)/4) {
		return ErrHeader
	}
	for range count {
		b, ok := take()
		if !ok || !utf8.Valid(b) {
			return ErrHeader
		}
	}
	// RFC 7845 allows trailing binary metadata/padding; do not interpret it.
	return nil
}
func crc(p []byte) uint32 {
	var sum uint32
	for _, b := range p {
		sum ^= uint32(b) << 24
		for range 8 {
			if sum&0x80000000 != 0 {
				sum = sum<<1 ^ 0x04c11db7
			} else {
				sum <<= 1
			}
		}
	}
	return sum
}
