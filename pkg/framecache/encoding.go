package framecache

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"time"
)

var errEncoding = errors.New("framecache: invalid binary frame")

// Version 1 uses network byte order. All variable fields carry uint32 lengths;
// packet descriptors, markers and source sequence numbers survive unchanged.
func encodeFrame(f Frame) ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte(1)
	writeBytes(&b, []byte(f.Track.Kind))
	for _, n := range []uint32{f.Track.SSRC, f.Timestamp, f.SourceSSRC, f.EchoTimestamp} {
		_ = binary.Write(&b, binary.BigEndian, n)
	}
	arrival, err := f.Arrival.MarshalBinary()
	if err != nil {
		return nil, err
	}
	writeBytes(&b, arrival)
	b.WriteByte(boolByte(f.Keyframe))
	_ = binary.Write(&b, binary.BigEndian, uint32(len(f.Packets)))
	for _, p := range f.Packets {
		_ = binary.Write(&b, binary.BigEndian, p.SequenceNumber)
		b.WriteByte(boolByte(p.Marker))
		writeBytes(&b, p.Payload)
	}
	return b.Bytes(), nil
}
func boolByte(b bool) byte {
	if b {
		return 1
	}
	return 0
}
func writeBytes(b *bytes.Buffer, value []byte) {
	_ = binary.Write(b, binary.BigEndian, uint32(len(value)))
	b.Write(value)
}
func readBytes(r *bytes.Reader) ([]byte, error) {
	var n uint32
	if binary.Read(r, binary.BigEndian, &n) != nil || uint64(n) > uint64(r.Len()) {
		return nil, errEncoding
	}
	value := make([]byte, int(n))
	_, err := io.ReadFull(r, value)
	return value, err
}
func decodeFrame(raw []byte) (Frame, error) {
	r := bytes.NewReader(raw)
	var f Frame
	version, err := r.ReadByte()
	if err != nil || version != 1 {
		return f, errEncoding
	}
	kind, err := readBytes(r)
	if err != nil {
		return f, errEncoding
	}
	f.Track.Kind = string(kind)
	for _, n := range []*uint32{&f.Track.SSRC, &f.Timestamp, &f.SourceSSRC, &f.EchoTimestamp} {
		if binary.Read(r, binary.BigEndian, n) != nil {
			return f, errEncoding
		}
	}
	arrival, err := readBytes(r)
	if err != nil {
		return f, errEncoding
	}
	var at time.Time
	if at.UnmarshalBinary(arrival) != nil {
		return f, errEncoding
	}
	f.Arrival = at
	key, err := r.ReadByte()
	if err != nil || key > 1 {
		return f, errEncoding
	}
	f.Keyframe = key == 1
	var count uint32
	if binary.Read(r, binary.BigEndian, &count) != nil || count == 0 || uint64(count)*8 > uint64(r.Len()) {
		return f, errEncoding
	}
	f.Packets = make([]Packet, int(count))
	for i := range f.Packets {
		p := &f.Packets[i]
		if binary.Read(r, binary.BigEndian, &p.SequenceNumber) != nil {
			return f, errEncoding
		}
		marker, err := r.ReadByte()
		if err != nil || marker > 1 {
			return f, errEncoding
		}
		p.Marker = marker == 1
		p.Payload, err = readBytes(r)
		if err != nil {
			return f, errEncoding
		}
	}
	if r.Len() != 0 {
		return f, errEncoding
	}
	if _, err := frameSize(f); err != nil {
		return f, errEncoding
	}
	return f, nil
}

// Shared admission keeps the two stores' complete-frame contract identical.
func frameSize(f Frame) (int, error) {
	if len(f.Packets) == 0 {
		return 0, ErrInvalidFrame
	}
	size := 0
	for i, p := range f.Packets {
		if len(p.Payload) == 0 || p.Marker != (i == len(f.Packets)-1) || i > 0 && p.SequenceNumber != f.Packets[i-1].SequenceNumber+1 {
			return 0, ErrInvalidFrame
		}
		size += len(p.Payload)
	}
	return size, nil
}
