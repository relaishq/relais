package callharness

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"image"
	"os/exec"
	"strings"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"golang.org/x/image/vp8"
)

// How the caller decides that echoed video decodes:
//
//  1. It reassembles VP8 frames (RFC 7741) from the echoed RTP packets,
//     strictly: a frame counts only if it has its first partition start,
//     every packet in sequence and its marker bit.
//  2. It decodes every keyframe in pure Go with golang.org/x/image/vp8, which
//     implements keyframes only (it is the WebP lossy decoder).
//  3. An interframe counts as decodable when it is complete and directly
//     follows the previous complete frame, back to a keyframe that decoded.
//     Every frame is also compared byte for byte with the frames the caller
//     sent.
//  4. At hangup, when ffmpeg is on PATH, every frame from the first decoded
//     keyframe on is written to an IVF stream and decoded by ffmpeg's VP8
//     decoder, which decodes interframes too. Without ffmpeg this step is
//     reported as skipped, never as passed.

// vp8Frame is one VP8 frame the caller reassembled from RTP packets.
type vp8Frame struct {
	data      []byte
	timestamp uint32 // RTP timestamp
	firstSeq  uint16
	lastSeq   uint16
	keyframe  bool
}

// vp8Assembler reassembles the VP8 frames of one track from its RTP packets
// in arrival order. It repairs nothing: the echo runs over loopback, so a
// missing or reordered packet is a defect to report.
type vp8Assembler struct {
	building  bool
	broken    bool // the frame lost its start or a packet
	timestamp uint32
	firstSeq  uint16
	lastSeq   uint16
	buf       []byte
}

// push adds the next packet. It returns the frame the packet completes, if
// any, and how many frames turned out incomplete.
func (a *vp8Assembler) push(pkt *rtp.Packet) (frame *vp8Frame, incomplete int) {
	if len(pkt.Payload) == 0 {
		return nil, 0 // padding only: no frame data
	}

	var desc codecs.VP8Packet
	payload, err := desc.Unmarshal(pkt.Payload)

	if a.building && pkt.Timestamp != a.timestamp {
		// A new frame started before the last one saw its marker bit.
		incomplete++
		a.building = false
	}
	if !a.building {
		a.building = true
		a.broken = err != nil || desc.S != 1 || desc.PID != 0
		a.timestamp = pkt.Timestamp
		a.firstSeq = pkt.SequenceNumber
		a.buf = a.buf[:0]
	} else if err != nil || pkt.SequenceNumber != a.lastSeq+1 {
		a.broken = true
	}
	a.lastSeq = pkt.SequenceNumber
	a.buf = append(a.buf, payload...)

	if !pkt.Marker {
		return nil, incomplete
	}
	a.building = false
	if a.broken || len(a.buf) == 0 {
		return nil, incomplete + 1
	}
	data := bytes.Clone(a.buf)

	return &vp8Frame{
		data:      data,
		timestamp: a.timestamp,
		firstSeq:  a.firstSeq,
		lastSeq:   a.lastSeq,
		keyframe:  isVP8Keyframe(data),
	}, incomplete
}

// decodeKeyframe decodes a VP8 keyframe in pure Go and returns its size.
func decodeKeyframe(frame []byte) (image.Point, error) {
	decoder := vp8.NewDecoder()
	decoder.Init(bytes.NewReader(frame), len(frame))
	header, err := decoder.DecodeFrameHeader()
	if err != nil {
		return image.Point{}, err
	}
	if !header.KeyFrame {
		return image.Point{}, errors.New("callharness: not a VP8 keyframe")
	}
	img, err := decoder.DecodeFrame()
	if err != nil {
		return image.Point{}, err
	}

	return img.Bounds().Size(), nil
}

// fullDecode decodes frames with ffmpeg's VP8 decoder. frames must start with
// a keyframe of the given size.
func fullDecode(ctx context.Context, frames []vp8Frame, size image.Point) FullDecode {
	if len(frames) == 0 {
		return FullDecode{Skipped: "no decoded keyframe to start from"}
	}
	path, err := exec.LookPath("ffmpeg")
	if err != nil {
		return FullDecode{Skipped: "ffmpeg not found on PATH"}
	}

	var ivf bytes.Buffer
	writeIVF(&ivf, frames, size)

	// framecrc prints one line per decoded frame; -xerror stops at the first
	// decode error, and -v error prints nothing unless something failed.
	cmd := exec.CommandContext(ctx, path, "-hide_banner", "-nostdin", "-v", "error", "-xerror",
		"-threads", "1", "-f", "ivf", "-i", "pipe:0", "-f", "framecrc", "-")
	cmd.Stdin = &ivf
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()

	result := FullDecode{
		Decoder:       path,
		FramesIn:      len(frames),
		FramesDecoded: countFrameCRCs(&stdout),
		Errors:        strings.TrimSpace(stderr.String()),
	}
	if runErr != nil && result.Errors == "" {
		result.Errors = runErr.Error()
	}

	return result
}

// writeIVF writes VP8 frames as an IVF stream with the RTP clock as its time
// base.
func writeIVF(w *bytes.Buffer, frames []vp8Frame, size image.Point) {
	var header [32]byte
	copy(header[0:4], "DKIF")
	binary.LittleEndian.PutUint16(header[4:], 0)  // version
	binary.LittleEndian.PutUint16(header[6:], 32) // header size
	copy(header[8:12], "VP80")
	binary.LittleEndian.PutUint16(header[12:], uint16(size.X))
	binary.LittleEndian.PutUint16(header[14:], uint16(size.Y))
	binary.LittleEndian.PutUint32(header[16:], vp8ClockRate) // time base denominator
	binary.LittleEndian.PutUint32(header[20:], 1)            // time base numerator
	binary.LittleEndian.PutUint32(header[24:], uint32(len(frames)))
	w.Write(header[:])

	for _, frame := range frames {
		var frameHeader [12]byte
		binary.LittleEndian.PutUint32(frameHeader[0:], uint32(len(frame.data)))
		binary.LittleEndian.PutUint64(frameHeader[4:], uint64(frame.timestamp-frames[0].timestamp))
		w.Write(frameHeader[:])
		w.Write(frame.data)
	}
}

// countFrameCRCs counts the decoded-frame lines of ffmpeg's framecrc output.
func countFrameCRCs(out *bytes.Buffer) int {
	frames := 0
	scanner := bufio.NewScanner(out)
	for scanner.Scan() {
		if line := strings.TrimSpace(scanner.Text()); line != "" && !strings.HasPrefix(line, "#") {
			frames++
		}
	}

	return frames
}
