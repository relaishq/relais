package mediaworker

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"sync"
	"time"

	"github.com/pion/transport/v4/deadline"
)

const dataReassemblyTimeout = 2 * time.Second
const dataEndTimeout = 50 * time.Millisecond
const dataWatchStreams = 64
const dataTSNWindow = 1 << 16

// dataTransport lends DTLS to SCTP without transferring ownership. SCTP.Close
// must never close the media transport. The pump remains the sole DTLS reader
// after data shutdown, discarding application records while processing alerts.
type dataTransport struct {
	net.Conn
	stopped      chan struct{}
	readDone     chan struct{}
	packets      chan []byte
	once         sync.Once
	monitor      dataReadMonitor
	readDeadline *deadline.Deadline
}

func newDataTransport(conn net.Conn) *dataTransport {
	return &dataTransport{Conn: conn, stopped: make(chan struct{}), readDone: make(chan struct{}), packets: make(chan []byte, 1), readDeadline: deadline.New()}
}
func (t *dataTransport) Close() error { t.once.Do(func() { close(t.stopped) }); return nil }
func (t *dataTransport) Write(p []byte) (int, error) {
	select {
	case <-t.stopped:
		return 0, net.ErrClosed
	default:
		return t.Conn.Write(p)
	}
}
func (t *dataTransport) Read(p []byte) (int, error) {
	select {
	case <-t.stopped:
		return 0, io.EOF
	default:
	}
	select {
	case <-t.stopped:
		return 0, io.EOF
	case <-t.readDeadline.Done():
		return 0, context.DeadlineExceeded
	case packet := <-t.packets:
		if len(packet) > len(p) {
			return 0, io.ErrShortBuffer
		}
		return copy(p, packet), nil
	}
}
func (t *dataTransport) pump() {
	defer close(t.readDone)
	defer t.Close()
	buf := make([]byte, receiveMTU)
	for {
		n, err := t.Conn.Read(buf)
		if err != nil {
			return
		}
		select {
		case <-t.stopped:
			continue
		default:
		}
		t.monitor.observe(buf[:n], time.Now())
		packet := append([]byte(nil), buf[:n]...)
		select {
		case <-t.stopped:
		case t.packets <- packet:
		}
	}
}

type dataReadStream struct {
	pending    int
	fragmented bool
	progress   time.Time
}

// This observer reads only fixed DATA/I-DATA headers. It never rewrites packets,
// validates/reassembles messages, or produces SCTP acknowledgments; Pion does
// all protocol work. A fixed TSN window excludes retransmissions from byte
// accounting. Metadata is capped independently of Pion's complete-message queue.
type dataReadMonitor struct {
	mu        sync.Mutex
	streams   map[uint16]*dataReadStream
	highest   uint32
	haveTSN   bool
	seen      [dataTSNWindow / 64]uint64
	exhausted bool
}

func (m *dataReadMonitor) firstTSN(tsn uint32) bool {
	if !m.haveTSN {
		m.haveTSN = true
		m.highest = tsn
	} else {
		delta := int32(tsn - m.highest)
		if delta > 0 {
			if delta >= dataTSNWindow {
				clear(m.seen[:])
			} else {
				for i := uint32(1); i <= uint32(delta); i++ {
					slot := (m.highest + i) % dataTSNWindow
					m.seen[slot/64] &= ^(uint64(1) << (slot % 64))
				}
			}
			m.highest = tsn
		} else if delta <= -dataTSNWindow {
			return false
		}
	}
	slot := tsn % dataTSNWindow
	bit := uint64(1) << (slot % 64)
	if m.seen[slot/64]&bit != 0 {
		return false
	}
	m.seen[slot/64] |= bit
	return true
}
func (m *dataReadMonitor) observe(packet []byte, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Common SCTP header is 12 B; chunks are padded to a four-byte boundary.
	for at := 12; at+4 <= len(packet); {
		chunk := packet[at:]
		size := int(binary.BigEndian.Uint16(chunk[2:4]))
		if size < 4 || size > len(chunk) {
			return
		}
		header := 0
		switch chunk[0] {
		case 0:
			header = 16
		case 64:
			header = 20
		}
		if header != 0 && size >= header && m.firstTSN(binary.BigEndian.Uint32(chunk[4:8])) {
			// DCEP is not an application message. Its ordinary OPEN/ACK is complete.
			dcep := chunk[0] == 0 && binary.BigEndian.Uint32(chunk[12:16]) == 50 || chunk[0] == 64 && chunk[1]&2 != 0 && binary.BigEndian.Uint32(chunk[16:20]) == 50
			if !dcep {
				id := binary.BigEndian.Uint16(chunk[8:10])
				if m.streams == nil {
					m.streams = make(map[uint16]*dataReadStream)
				}
				state := m.streams[id]
				if state == nil {
					if len(m.streams) >= dataWatchStreams {
						m.exhausted = true
						return
					}
					state = &dataReadStream{progress: now}
					m.streams[id] = state
				}
				if state.pending == 0 {
					state.progress = now
				}
				state.pending += size - header
				state.fragmented = state.fragmented || chunk[1]&3 != 3
			}
		}
		at += (size + 3) &^ 3
	}
}
func (m *dataReadMonitor) consumed(id uint16, n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if state := m.streams[id]; state != nil {
		state.pending = max(0, state.pending-max(1, n))
		state.progress = time.Now()
		if state.pending == 0 {
			state.fragmented = false
		}
	}
}
func (m *dataReadMonitor) stalled(now time.Time, maxMessage int) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.exhausted {
		return true
	}
	for _, state := range m.streams {
		if state.fragmented && state.pending > maxMessage && now.Sub(state.progress) >= dataReassemblyTimeout {
			return true
		}
	}
	return false
}
func (m *dataReadMonitor) forget(id uint16) { m.mu.Lock(); delete(m.streams, id); m.mu.Unlock() }

// SCTP Abort uses a read deadline to stop its own reader. Do not forward that
// deadline to DTLS: doing so would close media as soon as the pump reads again.
func (t *dataTransport) SetReadDeadline(at time.Time) error { t.readDeadline.Set(at); return nil }
func (t *dataTransport) SetDeadline(at time.Time) error {
	t.readDeadline.Set(at)
	return t.SetWriteDeadline(at)
}

func (m *dataReadMonitor) pendingBytes() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, s := range m.streams {
		n += s.pending
	}
	return n
}
