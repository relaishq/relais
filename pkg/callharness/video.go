package callharness

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"image"
	"os/exec"
	"slices"
	"strings"
	"time"

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
//  3. An interframe counts as decodable when its authenticated caller source
//     follows the previous PictureID, back to a decoded keyframe. Unknown
//     sources require output sequence adjacency. A source gap breaks the chain.
//     Every frame is also compared byte for byte with the frames the caller
//     sent.
//  4. At hangup, when ffmpeg is on PATH, every frame from the first decoded
//     keyframe on is written to an IVF stream and decoded by ffmpeg's VP8
//     decoder, which decodes interframes too. Without ffmpeg this step is
//     reported as skipped, never as passed.

// vp8Frame is one VP8 frame the caller reassembled from RTP packets.
type vp8Frame struct {
	data         []byte
	timestamp    uint32        // RTP timestamp
	sourceAt     time.Duration // authenticated sender clock for decoder DTS
	firstSeq     uint16
	lastSeq      uint16
	keyframe     bool
	pictureID    uint16
	firstArrival time.Time
	completedAt  time.Time
}

const (
	frameReorderWindow = 100 * time.Millisecond
	maxAssemblyFrames  = 64
	maxAssemblyPackets = 512
	maxAssemblyBytes   = 1 << 20
)

type assemblyID struct {
	timestamp uint32
	pictureID uint16
}
type pendingFrame struct {
	id           assemblyID
	firstArrival time.Time
	earliest     uint16
	assembly     contentAssembly
	arrivals     map[uint16]time.Time
	ready        *vp8Frame
	bytes        int
}
type completedFrame struct {
	id   assemblyID
	span contentPacketSpan
}

// vp8Assembler retains at most 64 frames, 512 fragments per frame and 1 MiB
// per frame. Known earlier frames hold later frames for at most 100 ms.
// Fragment order and exact duplicates never create a frame gap. A missing
// fragment expires once; later frames then break the source decode chain.
type vp8Assembler struct {
	pending   []*pendingFrame
	completed []completedFrame
	last      completedFrame
	haveLast  bool
}

// push is retained for single-frame fixtures; the live reader drains pushAll.
func (a *vp8Assembler) push(pkt *rtp.Packet) (*vp8Frame, int) {
	frames, n := a.pushAll(pkt, time.Now())
	if len(frames) == 0 {
		return nil, n
	}
	return frames[0], n
}

func (a *vp8Assembler) pushAll(pkt *rtp.Packet, now time.Time) (frames []*vp8Frame, incomplete int) {
	if pkt != nil && len(pkt.Payload) > 0 {
		var desc codecs.VP8Packet
		payload, err := desc.Unmarshal(pkt.Payload)
		if err == nil {
			id := assemblyID{pkt.Timestamp, desc.PictureID}
			duplicate := false
			for _, done := range a.completed {
				if done.id == id && done.span.contains(pkt.SequenceNumber) {
					duplicate = true
					break
				}
			}
			if !duplicate {
				var f *pendingFrame
				for _, candidate := range slices.Backward(a.pending) {
					if candidate.id != id {
						continue
					}
					if candidate.ready != nil {
						if (contentPacketSpan{candidate.ready.firstSeq, candidate.ready.lastSeq}).contains(pkt.SequenceNumber) {
							f = candidate // duplicate while waiting for reordering
							break
						}
						continue
					}
					// A replay can retain timestamp/PictureID while changing its
					// sequence span. Never let an expired prefix absorb the new
					// copy, including marker-before-start reordering. The fragment
					// bound also separates disjoint spans within the reorder window.
					step := int16(pkt.SequenceNumber - candidate.earliest)
					newStart := desc.S == 1 && desc.PID == 0 && len(candidate.assembly.starts) > 0 && !candidate.assembly.starts[pkt.SequenceNumber]
					beforeStart := len(candidate.assembly.starts) > 0 && step < 0
					if now.Sub(candidate.firstArrival) < frameReorderWindow && step > -maxAssemblyPackets && step < maxAssemblyPackets && !newStart && !beforeStart {
						f = candidate
						break
					}
				}
				if f == nil {
					f = &pendingFrame{id: id, firstArrival: now, earliest: pkt.SequenceNumber, assembly: contentAssembly{parts: map[uint16][]byte{}, starts: map[uint16]bool{}, ends: map[uint16]bool{}}, arrivals: map[uint16]time.Time{}}
					if desc.S == 1 && desc.PID == 0 {
						// A nearby replay's tail may precede its new start and
						// temporarily join the interrupted prefix. Move that tail
						// into the new span once its start identifies the split.
						for _, old := range a.pending {
							if old.id != id || old.ready != nil || now.Sub(old.firstArrival) >= frameReorderWindow {
								continue
							}
							for seq, part := range old.assembly.parts {
								if uint16(seq-pkt.SequenceNumber) >= maxAssemblyPackets {
									continue
								}
								f.assembly.parts[seq] = part
								f.arrivals[seq] = old.arrivals[seq]
								if old.arrivals[seq].Before(f.firstArrival) {
									f.firstArrival = old.arrivals[seq]
								}
								if old.assembly.ends[seq] {
									f.assembly.ends[seq] = true
								}
								f.bytes += len(part)
								old.bytes -= len(part)
								delete(old.assembly.parts, seq)
								delete(old.assembly.ends, seq)
								delete(old.arrivals, seq)
							}
						}
					}
					a.pending = append(a.pending, f)
				}
				if _, exists := f.assembly.parts[pkt.SequenceNumber]; !exists && f.ready == nil {
					if int16(pkt.SequenceNumber-f.earliest) < 0 {
						f.earliest = pkt.SequenceNumber
					}
					f.assembly.parts[pkt.SequenceNumber] = bytes.Clone(payload)
					f.arrivals[pkt.SequenceNumber] = now
					f.bytes += len(payload)
					if desc.S == 1 && desc.PID == 0 {
						f.assembly.starts[pkt.SequenceNumber] = true
					}
					if pkt.Marker {
						f.assembly.ends[pkt.SequenceNumber] = true
					}
					if len(f.assembly.parts) > maxAssemblyPackets || f.bytes > maxAssemblyBytes {
						f.firstArrival = now.Add(-frameReorderWindow)
					} else {
						for first := range f.assembly.starts {
							for last := range f.assembly.ends {
								count := int(uint16(last-first)) + 1
								if count > len(f.assembly.parts) || count > maxAssemblyPackets {
									continue
								}
								var data []byte
								for i := 0; i < count; i++ {
									part, ok := f.assembly.parts[first+uint16(i)]
									if !ok {
										data = nil
										break
									}
									data = append(data, part...)
								}
								if len(data) > 0 {
									f.ready = &vp8Frame{data: data, timestamp: id.timestamp, firstSeq: first, lastSeq: last, keyframe: isVP8Keyframe(data), pictureID: id.pictureID, firstArrival: f.firstArrival, completedAt: now}
									break
								}
							}
							if f.ready != nil {
								break
							}
						}
					}
				}
			}
		}
	}
	slices.SortStableFunc(a.pending, func(x, y *pendingFrame) int { return int(int16(x.earliest - y.earliest)) })
	for len(a.pending) > 0 {
		f := a.pending[0]
		if f.ready != nil {
			// A whole later frame can arrive before any fragment of its predecessor.
			// Wait only for a small forward source+packet gap; replay and takeover
			// margins must not acquire an artificial playout delay.
			sourceStep := (f.id.pictureID - a.last.id.pictureID) & 0x7fff
			packetStep := uint16(f.ready.firstSeq - a.last.span.last)
			timestampStep := uint32(f.id.timestamp - a.last.id.timestamp)
			if a.haveLast && sourceStep > 1 && sourceStep < 1<<14 && packetStep > 1 && packetStep <= maxAssemblyPackets && timestampStep > 0 && timestampStep <= uint32(frameReorderWindow*vp8ClockRate/time.Second) && now.Sub(f.firstArrival) < frameReorderWindow {
				break
			}
			a.last = completedFrame{f.id, contentPacketSpan{f.ready.firstSeq, f.ready.lastSeq}}
			a.haveLast = true
			frames = append(frames, f.ready)
			a.completed = append(a.completed, completedFrame{f.id, contentPacketSpan{f.ready.firstSeq, f.ready.lastSeq}})
			if len(a.completed) > maxAssemblyPackets {
				a.completed = slices.Clone(a.completed[len(a.completed)-maxAssemblyPackets:])
			}
		} else if now.Sub(f.firstArrival) >= frameReorderWindow || len(a.pending) > maxAssemblyFrames {
			incomplete++
		} else {
			break
		}
		a.pending = slices.Delete(a.pending, 0, 1)
	}
	return frames, incomplete
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
		return FullDecode{Skipped: "no decoded keyframe to start from", Failure: DecodeUnavailable}
	}
	path, err := exec.LookPath("ffmpeg")
	if err != nil {
		return FullDecode{Skipped: "ffmpeg not found on PATH", Failure: DecodeUnavailable}
	}

	var ivf bytes.Buffer
	writeIVF(&ivf, frames, size)

	// framecrc prints one line per decoded frame; -xerror stops at the first
	// decode error, and -v error prints nothing unless something failed.
	// Preserve compressed replay timestamps: ffmpeg's default output pacing
	// would drop decoded burst frames to impose a constant output frame rate.
	cmd := exec.CommandContext(ctx, path, "-hide_banner", "-nostdin", "-v", "error", "-xerror",
		"-threads", "1", "-f", "ivf", "-i", "pipe:0", "-fps_mode", "passthrough", "-f", "framecrc", "-")
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
	if ctx.Err() != nil {
		result.Failure = DecodeUnavailable
		result.Skipped = "decode interrupted: " + ctx.Err().Error()
		result.Errors = ""
	} else if result.Errors != "" || result.FramesDecoded != result.FramesIn {
		// ffmpeg execution failures do not prove corrupt media. Only an
		// explicit bitstream diagnostic can produce a trusted failure.
		result.Failure = DecodeUnavailable
		message := strings.ToLower(result.Errors)
		if strings.Contains(message, "invalid data") || strings.Contains(message, "error while decoding") {
			result.Failure = DecodeInvalid
		}
	}

	return result
}

// writeIVF uses the authenticated source clock when available. Decoder input
// retains source order and excludes decoded repeats; relay output timestamps
// cannot move its DTS backwards. Unknown sources retain their RTP timestamps.
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
		dts := uint64(frame.timestamp - frames[0].timestamp)
		if frames[0].sourceAt > 0 && frame.sourceAt >= frames[0].sourceAt {
			delta := frame.sourceAt - frames[0].sourceAt
			dts = uint64(delta/time.Second)*vp8ClockRate + uint64(delta%time.Second)*vp8ClockRate/uint64(time.Second)
		}
		binary.LittleEndian.PutUint64(frameHeader[4:], dts)
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
