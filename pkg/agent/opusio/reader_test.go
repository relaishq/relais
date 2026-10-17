package opusio

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4/pkg/media/oggwriter"
	"github.com/stretchr/testify/require"
)

func page(seq uint32, flags byte, granule uint64, laces []byte, body []byte) []byte {
	p := make([]byte, 27+len(laces)+len(body))
	copy(p, "OggS")
	p[5] = flags
	binary.LittleEndian.PutUint64(p[6:], granule)
	binary.LittleEndian.PutUint32(p[14:], 17)
	binary.LittleEndian.PutUint32(p[18:], seq)
	p[26] = byte(len(laces))
	copy(p[27:], laces)
	copy(p[27+len(laces):], body)
	binary.LittleEndian.PutUint32(p[22:], crc(p))
	return p
}
func headers() ([]byte, []byte) {
	h := append([]byte("OpusHead"), make([]byte, 11)...)
	h[8] = 1
	h[9] = 1
	binary.LittleEndian.PutUint16(h[10:], 312)
	binary.LittleEndian.PutUint32(h[12:], 24000)
	return h, append([]byte("OpusTags"), make([]byte, 8)...)
}
func prefix() []byte {
	h, t := headers()
	return append(page(0, 2, 0, []byte{19}, h), page(1, 0, 0, []byte{16}, t)...)
}
func stream(p []byte) []byte {
	laces := bytes.Repeat([]byte{255}, len(p)/255)
	laces = append(laces, byte(len(p)%255))
	return append(prefix(), page(2, 4, 960, laces, p)...)
}
func packets(t *testing.T) [][]byte {
	t.Helper()
	b, err := os.ReadFile("../../callharness/testdata/tts-opus-20ms.packets")
	require.NoError(t, err)
	var out [][]byte
	for len(b) > 0 {
		require.GreaterOrEqual(t, len(b), 4)
		n := int(binary.BigEndian.Uint32(b))
		b = b[4:]
		require.GreaterOrEqual(t, len(b), n)
		out = append(out, bytes.Clone(b[:n]))
		b = b[n:]
	}
	require.Len(t, out, 80)
	return out
}
func TestProviderPacketsWithPionWriter(t *testing.T) {
	want := packets(t)
	name := filepath.Join(t.TempDir(), "provider.ogg")
	writer, err := oggwriter.New(name, 24000, 1)
	require.NoError(t, err)
	for i, p := range want {
		require.NoError(t, writer.WriteRTP(&rtp.Packet{Header: rtp.Header{Version: 2, Timestamp: uint32(i * 960), SequenceNumber: uint16(i)}, Payload: p}))
	}
	require.NoError(t, writer.Close())
	b, err := os.ReadFile(name)
	require.NoError(t, err)
	// Match spike metadata. Pion's default pre-skip is 3,840; provider uses 312.
	size := 27 + int(b[26]) + 19
	binary.LittleEndian.PutUint16(b[28+10:], 312)
	clear(b[22:26])
	binary.LittleEndian.PutUint32(b[22:], crc(b[:size]))
	r, err := NewReader(bytes.NewReader(b))
	require.NoError(t, err)
	require.Equal(t, Header{Channels: 1, InputRate: 24000, PreSkip: 312}, r.Header())
	for i, p := range want {
		audio, err := r.Next(context.Background())
		require.NoError(t, err)
		require.Equal(t, "audio/opus", audio.Format)
		require.Equal(t, [][]byte{p}, audio.Packets)
		require.Equal(t, 20*time.Millisecond, r.PacketDuration())
		require.Equal(t, time.Duration((i+1)*960-312)*time.Second/48000, r.Position())
	}
	_, err = r.Next(context.Background())
	require.ErrorIs(t, err, io.EOF)
}
func TestLacingContinuationAndBoundary(t *testing.T) {
	for _, n := range []int{254, 255, 256, 510, 1000} {
		t.Run(string(rune(n)), func(t *testing.T) {
			p := append([]byte{0xf8}, make([]byte, n-1)...)
			r, err := NewReader(bytes.NewReader(stream(p)))
			require.NoError(t, err)
			audio, err := r.Next(context.Background())
			require.NoError(t, err)
			require.Equal(t, p, audio.Packets[0])
			if n < 255 {
				return
			}
			b := append(prefix(), page(2, 0, ^uint64(0), []byte{255}, p[:255])...)
			rest := p[255:]
			lace := bytes.Repeat([]byte{255}, len(rest)/255)
			lace = append(lace, byte(len(rest)%255))
			b = append(b, page(3, 5, 960, lace, rest)...)
			r, err = NewReader(bytes.NewReader(b))
			require.NoError(t, err)
			audio, err = r.Next(context.Background())
			require.NoError(t, err)
			require.Equal(t, p, audio.Packets[0])
			require.Equal(t, 13500*time.Microsecond, r.Position())
		})
	}
}
func TestMultiPacketPageAndTrim(t *testing.T) {
	p := []byte{0xf8, 0xff, 0xfe}
	b := append(prefix(), page(2, 4, 1800, []byte{3, 3}, append(bytes.Clone(p), p...))...)
	r, err := NewReader(bytes.NewReader(b))
	require.NoError(t, err)
	first, err := r.Next(context.Background())
	require.NoError(t, err)
	require.Equal(t, 13500*time.Microsecond, r.Position())
	_, err = r.Next(context.Background())
	require.NoError(t, err)
	require.Equal(t, 31*time.Millisecond, r.Position())
	require.Equal(t, p, first.Packets[0])
}
func TestPreSkipLongerThanPacket(t *testing.T) {
	h, tags := headers()
	binary.LittleEndian.PutUint16(h[10:], 1200)
	b := append(page(0, 2, 0, []byte{19}, h), page(1, 0, 0, []byte{16}, tags)...)
	p := []byte{0xf8, 0xff, 0xfe}
	b = append(b, page(2, 4, 1920, []byte{3, 3}, append(bytes.Clone(p), p...))...)
	r, err := NewReader(bytes.NewReader(b))
	require.NoError(t, err)
	_, err = r.Next(context.Background())
	require.NoError(t, err)
	require.Zero(t, r.Position())
	_, err = r.Next(context.Background())
	require.NoError(t, err)
	require.Equal(t, 15*time.Millisecond, r.Position())
}
func TestMalformedStreams(t *testing.T) {
	good := stream([]byte{0xf8, 0xff, 0xfe})
	cases := []struct {
		name string
		data []byte
		want error
	}{
		{"crc", append(bytes.Clone(good[:len(good)-1]), 0), ErrCRC},
		{"truncated", good[:len(good)-1], io.ErrUnexpectedEOF},
		{"chained", append(bytes.Clone(good), good...), ErrChained},
		{"continuation", append(prefix(), page(2, 5, 960, []byte{3}, []byte{0xf8, 0xff, 0xfe})...), ErrContainer},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := NewReader(bytes.NewReader(tc.data))
			if err == nil {
				for i := 0; i < 10; i++ {
					_, err = r.Next(context.Background())
					if err != nil {
						break
					}
				}
			}
			require.ErrorIs(t, err, tc.want)
		})
	}
	for _, tc := range []struct {
		name   string
		change func([]byte)
		want   error
	}{
		{"mapping", func(h []byte) { h[18] = 1 }, ErrMapping},
		{"multichannel", func(h []byte) { h[9] = 6 }, ErrMapping},
		{"gain", func(h []byte) { h[16] = 1 }, ErrGain},
		{"version", func(h []byte) { h[8] = 16 }, ErrHeader},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, tags := headers()
			tc.change(h)
			_, err := NewReader(bytes.NewReader(append(page(0, 2, 0, []byte{19}, h), page(1, 0, 0, []byte{16}, tags)...)))
			require.ErrorIs(t, err, tc.want)
		})
	}
	for _, tags := range [][]byte{[]byte("OpusTags"), append([]byte("OpusTags"), 255, 255, 255, 255, 0, 0, 0, 0)} {
		h, _ := headers()
		_, err := NewReader(bytes.NewReader(append(page(0, 2, 0, []byte{19}, h), page(1, 0, 0, []byte{byte(len(tags))}, tags)...)))
		require.ErrorIs(t, err, ErrHeader)
	}
}
func TestCancellationAndBounds(t *testing.T) {
	r, err := NewReader(bytes.NewReader(stream([]byte{0xf8, 0xff, 0xfe})))
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = r.Next(ctx)
	require.ErrorIs(t, err, context.Canceled)
	_, err = r.Next(context.Background())
	require.NoError(t, err, "pre-cancelled call does not consume input")
	b := prefix()
	for i := uint32(2); i < 5; i++ {
		flag := byte(0)
		if i > 2 {
			flag = 1
		}
		b = append(b, page(i, flag, ^uint64(0), bytes.Repeat([]byte{255}, 255), make([]byte, 65025))...)
	}
	r, err = NewReader(bytes.NewReader(b))
	require.NoError(t, err)
	_, err = r.Next(context.Background())
	require.ErrorIs(t, err, ErrLimit)
}
func TestBoundedRandomInputs(t *testing.T) {
	random := rand.New(rand.NewSource(10))
	for i := 0; i < 1000; i++ {
		b := make([]byte, random.Intn(2048))
		_, err := random.Read(b)
		require.NoError(t, err)
		if i%2 == 0 {
			b = append(prefix(), b...)
		}
		r, err := NewReader(bytes.NewReader(b))
		if err != nil {
			continue
		}
		for j := 0; j < 8; j++ {
			_, err = r.Next(context.Background())
			if err != nil {
				break
			}
		}
		require.Error(t, err)
	}
}
func FuzzReader(f *testing.F) {
	f.Add(stream([]byte{0xf8, 0xff, 0xfe}))
	f.Add(prefix())
	f.Add([]byte("OggS"))
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) > 256<<10 {
			t.Skip()
		}
		r, err := NewReader(bytes.NewReader(b))
		if err != nil {
			return
		}
		for i := 0; i < 1024; i++ {
			_, err = r.Next(context.Background())
			if err != nil {
				return
			}
		}
	})
}

func TestTagsAcrossPagesAndEmptyEOS(t *testing.T) {
	h, _ := headers()
	tags := append([]byte("OpusTags"), 0, 0, 0, 0)
	binary.LittleEndian.PutUint32(tags[8:], 400)
	tags = append(tags, bytes.Repeat([]byte("v"), 400)...)
	tags = append(tags, 0, 0, 0, 0)
	b := page(0, 2, 0, []byte{19}, h)
	b = append(b, page(1, 0, ^uint64(0), []byte{255}, tags[:255])...)
	b = append(b, page(2, 1, 0, []byte{byte(len(tags) - 255)}, tags[255:])...)
	p := []byte{0xf8, 0xff, 0xfe}
	b = append(b, page(3, 0, 960, []byte{3}, p)...)
	b = append(b, page(4, 4, 960, nil, nil)...)
	r, err := NewReader(bytes.NewReader(b))
	require.NoError(t, err)
	audio, err := r.Next(context.Background())
	require.NoError(t, err)
	require.Equal(t, p, audio.Packets[0])
	_, err = r.Next(context.Background())
	require.ErrorIs(t, err, io.EOF)
}

func TestTruncationAtEveryPageBoundaryIsNotEOF(t *testing.T) {
	b := stream([]byte{0xf8, 0xff, 0xfe})
	for cut := range len(b) {
		r, err := NewReader(bytes.NewReader(b[:cut]))
		if err == nil {
			_, err = r.Next(context.Background())
		}
		require.ErrorIs(t, err, io.ErrUnexpectedEOF, "cut %d", cut)
	}
}

func TestPositionDoesNotOverflowAfterTwoDays(t *testing.T) {
	r := &Reader{position: 60 * 60 * 60 * 48000}
	require.Equal(t, 60*time.Hour, r.Position())
}

func TestDiscardMalformedAudioPackets(t *testing.T) {
	good := []byte{0xf8, 0xff, 0xfe}
	for _, bad := range [][]byte{nil, {0xfb}, {0xf9, 1}} {
		for _, sharedPage := range []bool{false, true} {
			b := prefix()
			if sharedPage {
				b = append(b, page(2, 4, 1920, []byte{byte(len(bad)), 3}, append(bytes.Clone(bad), good...))...)
			} else {
				b = append(b, page(2, 0, 960, []byte{byte(len(bad))}, bad)...)
				b = append(b, page(3, 4, 1920, []byte{3}, good)...)
			}
			r, err := NewReader(bytes.NewReader(b))
			require.NoError(t, err)
			audio, err := r.Next(context.Background())
			require.NoError(t, err)
			require.Equal(t, [][]byte{good}, audio.Packets)
			require.Equal(t, 33500*time.Microsecond, r.Position())
			counter, ok := any(r).(interface{ DiscardedPackets() uint64 })
			require.True(t, ok, "Reader must expose discarded audio packet count")
			require.Equal(t, uint64(1), counter.DiscardedPackets())
			_, err = r.Next(context.Background())
			require.ErrorIs(t, err, io.EOF)
		}
	}
	r, err := NewReader(bytes.NewReader(stream(nil)))
	require.NoError(t, err)
	_, err = r.Next(context.Background())
	require.ErrorIs(t, err, io.EOF)
}
func TestOpusHeadCompatibleVersions(t *testing.T) {
	for version := byte(0); version < 16; version++ {
		h, tags := headers()
		h[8] = version
		b := append(page(0, 2, 0, []byte{19}, h), page(1, 0, 0, []byte{16}, tags)...)
		_, err := NewReader(bytes.NewReader(b))
		require.NoError(t, err, "version %d", version)
	}
}
