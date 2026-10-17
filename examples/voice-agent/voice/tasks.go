package voice

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4/pkg/media/oggwriter"
	"github.com/relaishq/relais/pkg/agent/opusio"
	agent "github.com/relaishq/relais/pkg/agent/v2"
)

// Provider keeps keyless tests on the production task path. No method returns
// provider bodies in errors. Key is stable per task; retries can still be billed.
type Provider interface {
	Available() bool
	Transcribe(context.Context, string, []byte) (string, error)
	Reply(context.Context, string, string, []Exchange) (string, error)
	Speech(context.Context, string, string) (io.ReadCloser, error)
}
type replyRequest struct {
	Transcript string     `json:"transcript"`
	History    []Exchange `json:"history"`
}

type speechRequest struct {
	Text  string        `json:"text"`
	Bytes int           `json:"bytes"`
	Nanos time.Duration `json:"nanos"`
}

func handlers(p Provider) map[string]agent.TaskHandler {
	return map[string]agent.TaskHandler{
		WarmKind: func(ctx context.Context, run *agent.TaskRun) error {
			if run.After() == 0 {
				if provider, ok := p.(interface{ Warm(context.Context) }); ok {
					provider.Warm(ctx)
				}
			}
			return nil
		},
		TranscribeKind: func(ctx context.Context, run *agent.TaskRun) error {
			if run.After() > 0 {
				return nil
			}
			b, err := RecordingOgg(run.Spec().Request)
			if err != nil {
				return run.Fail(agent.Failed, "invalid_audio")
			}
			var text string
			err = retryOnce(ctx, func() error { var e error; text, e = p.Transcribe(ctx, run.Key(), b); return e })
			if err != nil {
				return taskFailure(run, err)
			}
			return run.Emit(ctx, 1, agent.Chunk{Data: []byte(boundedText(text, 1024))})
		},
		ReplyKind: func(ctx context.Context, run *agent.TaskRun) error {
			if run.After() > 0 {
				return nil
			}
			var request replyRequest
			if err := json.Unmarshal(run.Spec().Request, &request); err != nil {
				return run.Fail(agent.Failed, "invalid_request")
			}
			var text string
			err := retryOnce(ctx, func() error {
				var e error
				text, e = p.Reply(ctx, run.Key(), request.Transcript, request.History)
				return e
			})
			if err != nil {
				return taskFailure(run, err)
			}
			return run.Emit(ctx, 1, agent.Chunk{Data: []byte(boundedText(text, MaxReplyBytes))})
		},
		SpeechKind: func(ctx context.Context, run *agent.TaskRun) error {
			if run.After() > 0 {
				return nil
			}
			var request speechRequest
			if err := json.Unmarshal(run.Spec().Request, &request); err != nil || request.Bytes <= 1 || request.Bytes > MaxSegmentBytes || request.Nanos <= 0 || request.Nanos > 8*time.Second {
				return run.Fail(agent.Failed, "invalid_request")
			}
			audio, err := renderSpeech(ctx, p, run.Key(), request)
			if err != nil {
				return taskFailure(run, err)
			}
			return run.Emit(ctx, 1, agent.Chunk{Data: audio})
		},
		PlayKind: Play,
	}
}
func taskFailure(run *agent.TaskRun, err error) error {
	var e *ProviderError
	if errors.As(err, &e) {
		if e.Uncertain {
			return run.Fail(agent.OutcomeUnknown, e.Code)
		}
		return run.Fail(agent.Failed, e.Code)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return run.Fail(agent.Failed, "task_failed")
}

// Retry a failed idempotent provider request once, with the identical body/key.
// Nothing here promises provider idempotency or exactly-once billing.
func retryOnce(ctx context.Context, call func() error) error {
	err := call()
	var e *ProviderError
	if !errors.As(err, &e) || !e.Uncertain {
		return err
	}
	timer := time.NewTimer(200 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
	}
	return call()
}
func safeCode(s string) string {
	switch s {
	case "provider_unavailable", "provider_refused", "provider_uncertain", "provider_configuration", "invalid_audio", "invalid_speech", "invalid_request":
		return s
	default:
		return "task_failed"
	}
}

func decodePackets(b []byte, limit int) ([][]byte, error) {
	if len(b) == 0 || b[0] != 1 || len(b) > limit {
		return nil, errors.New("voice: recording version/size")
	}
	var packets [][]byte
	for b = b[1:]; len(b) > 0; {
		if len(b) < 2 {
			return nil, io.ErrUnexpectedEOF
		}
		n := int(binary.BigEndian.Uint16(b))
		b = b[2:]
		if n == 0 || n > 1200 || n > len(b) {
			return nil, errors.New("voice: recording packet size")
		}
		if _, err := packetDuration(b[:n]); err != nil {
			return nil, err
		}
		packets = append(packets, b[:n])
		b = b[n:]
	}
	if len(packets) == 0 {
		return nil, errors.New("voice: empty recording")
	}
	return packets, nil
}

// RecordingOgg writes unchanged RTP Opus using Pion. RTP carries an already
// running encoder, not an Ogg encoder's initial preroll. Set pre-skip to zero
// and update the header CRC, so even a single 20 ms packet remains audible.
// The upload filename is recording.ogg; identical bytes named .opus are refused.
func RecordingOgg(recording []byte) ([]byte, error) {
	packets, err := decodePackets(recording, MaxRecordingBytes)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	writer, err := oggwriter.NewWith(&out, 48000, 1)
	if err != nil {
		return nil, err
	}
	for i, p := range packets {
		if err = writer.WriteRTP(&rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: uint16(i)}, Payload: p}); err != nil {
			_ = writer.Close()
			return nil, err
		}
	}
	if err = writer.Close(); err != nil {
		return nil, err
	}
	b := out.Bytes()
	// Pion emits one header packet per first page. Only modify that known page.
	if len(b) < 47 || string(b[28:36]) != "OpusHead" {
		return nil, errors.New("voice: unexpected Pion header")
	}
	binary.LittleEndian.PutUint16(b[38:40], 0)
	rewriteCRC(b[:47])
	// NewWith cannot seek back at Close. Mark the last already-written audio page
	// EOS and recalculate its checksum; granules already count actual TOC samples.
	last := 0
	for pos := 0; pos < len(b); {
		if pos+27 > len(b) {
			return nil, io.ErrUnexpectedEOF
		}
		header := 27 + int(b[pos+26])
		if pos+header > len(b) {
			return nil, io.ErrUnexpectedEOF
		}
		n := 0
		for _, l := range b[pos+27 : pos+header] {
			n += int(l)
		}
		if pos+header+n > len(b) {
			return nil, io.ErrUnexpectedEOF
		}
		binary.LittleEndian.PutUint32(b[pos+14:pos+18], 1) // one deterministic logical stream per upload
		rewriteCRC(b[pos : pos+header+n])
		last = pos
		pos += header + n
		if pos > len(b) {
			return nil, io.ErrUnexpectedEOF
		}
	}
	b[last+5] |= 4
	rewriteCRC(b[last:])
	return b, nil
}
func rewriteCRC(page []byte) {
	clear(page[22:26])
	var crc uint32
	for _, v := range page {
		crc ^= uint32(v) << 24
		for range 8 {
			if crc&0x80000000 != 0 {
				crc = crc<<1 ^ 0x04c11db7
			} else {
				crc <<= 1
			}
		}
	}
	binary.LittleEndian.PutUint32(page[22:26], crc)
}
func readSpeech(ctx context.Context, body io.Reader) ([]byte, error) {
	return readSpeechBound(ctx, body, MaxSpeechBytes, SpeechTime)
}
func readSpeechBound(ctx context.Context, body io.Reader, byteLimit int, durationLimit time.Duration) ([]byte, error) {
	// Limit the wire response as well as the retained packets and duration.
	reader, err := opusio.NewReader(io.LimitReader(body, int64(2*byteLimit+1)))
	if err != nil {
		return nil, err
	}
	if reader.Header().Channels != 1 {
		return nil, errors.New("voice: speech must be mono")
	}
	b := []byte{1}
	var span time.Duration
	for {
		audio, err := reader.Next(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		for _, p := range audio.Packets {
			d, err := packetDuration(p)
			if err != nil {
				return nil, err
			}
			span += d
			if len(p) > 1200 || len(b)+2+len(p) > byteLimit || span > durationLimit {
				return nil, errors.New("voice: speech bound")
			}
			b = binary.BigEndian.AppendUint16(b, uint16(len(p)))
			b = append(b, p...)
		}
	}
	if reader.DiscardedPackets() != 0 || len(b) == 1 {
		return nil, errors.New("voice: incomplete speech")
	}
	return b, nil
}

// Play emits stable 100 ms chunks from a fully accepted speech rendering. An
// exact move retries this request above After, without paying for speech again.
func Play(ctx context.Context, run *agent.TaskRun) error {
	packets, err := decodePackets(run.Spec().Request, MaxSegmentBytes)
	if err != nil {
		return run.Fail(agent.Failed, "invalid_speech")
	}
	var sequence uint64
	for start := 0; start < len(packets); {
		end := start
		var span time.Duration
		for end < len(packets) && span < 100*time.Millisecond {
			d, _ := packetDuration(packets[end])
			span += d
			end++
		}
		sequence++
		if sequence > run.After() {
			if err := run.Emit(ctx, sequence, agent.Chunk{Audio: &agent.EncodedAudio{Format: "audio/opus", Packets: packets[start:end]}}); err != nil {
				return err
			}
		}
		start = end
	}
	return nil
}

// speechBody redacts failures at the transport boundary. A body read error
// (including HTTP truncation) is uncertain. A normally completed HTTP body
// containing an incomplete Ogg container is a definite format rejection.
type speechBody struct{ io.Reader }

func (r *speechBody) Read(b []byte) (int, error) {
	n, err := r.Reader.Read(b)
	if err != nil && !errors.Is(err, io.EOF) {
		return n, &ProviderError{Code: "provider_uncertain", Uncertain: true}
	}
	return n, err
}

// renderSpeech accepts a complete bounded segment before the task emits it.
func renderSpeech(ctx context.Context, p Provider, key string, request speechRequest) ([]byte, error) {
	var audio []byte
	err := retryOnce(ctx, func() error {
		body, err := p.Speech(ctx, key, request.Text)
		if err != nil {
			return err
		}
		defer func() { _ = body.Close() }()
		audio, err = readSpeechBound(ctx, &speechBody{Reader: body}, request.Bytes, request.Nanos)
		if err != nil {
			var provider *ProviderError
			if errors.As(err, &provider) || ctx.Err() != nil {
				return err
			}
			return &ProviderError{Code: "invalid_speech"}
		}
		return nil
	})
	return audio, err
}
